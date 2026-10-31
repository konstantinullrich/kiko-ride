package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiko-backend/geo"
	"kiko-backend/x402"
)

const timeFormat = time.RFC3339

type rideView struct {
	ID           string     `json:"id"`
	Status       RideStatus `json:"status"`
	StartAddress string     `json:"start_address"`
	EndAddress   string     `json:"end_address"`
	Start        geo.Point  `json:"start"`
	End          geo.Point  `json:"end"`
	DistanceKm   float64    `json:"distance_km"`
	PickupTime   string     `json:"pickup_time"`
	// Custodial is true when the rider gave no cosigner key, so the server
	// holds two of the ride address's three keys.
	Custodial bool `json:"custodial"`

	// Network is where the fare is paid; Fare is in its smallest unit.
	// FareLovelace repeats a Cardano fare for older clients.
	Network      x402.Network `json:"network,omitempty"`
	Fare         uint64       `json:"fare,omitempty"`
	FareLovelace uint64       `json:"fare_lovelace,omitempty"`
	PayTo        string       `json:"pay_to,omitempty"`
	EtaMinutes   *int         `json:"eta_minutes,omitempty"`
	EtaKm        *float64     `json:"eta_km,omitempty"`
	// Car is the driver's car, so the rider can find it.
	Car       *Car   `json:"car,omitempty"`
	PaymentTx string `json:"payment_tx,omitempty"`

	// Shown to the driver, who needs them to build the payout.
	AddressIndex *uint32 `json:"address_index,omitempty"`
	Script       string  `json:"script,omitempty"`
	PayoutTx     string  `json:"payout_tx,omitempty"`
}

// view renders a ride. The ETA and car are shown to the driver, and to the
// rider only once the ride is paid; payout details only to the driver.
func (s *Server) view(r Ride, forDriver bool) rideView {
	v := rideView{
		ID:           r.ID,
		Status:       r.Status,
		StartAddress: r.StartAddress,
		EndAddress:   r.EndAddress,
		Start:        r.Start,
		End:          r.End,
		DistanceKm:   r.DistanceKm,
		PickupTime:   r.PickupTime.Format(timeFormat),
		Custodial:    r.RiderKey == "",
		PaymentTx:    r.PaymentTx,
	}
	if r.Status == StatusRequested {
		return v
	}
	v.Network, v.Fare = s.network(r.Chain), r.Fare
	if r.Chain == ChainCardano {
		v.FareLovelace = r.Fare
	}
	v.PayTo = r.PayTo
	if forDriver {
		v.AddressIndex, v.Script, v.PayoutTx = &r.AddressIndex, r.Script, r.PayoutTx
	}
	if forDriver || r.Status == StatusPaid || r.Status == StatusPaidOut {
		v.EtaMinutes, v.EtaKm = &r.EtaMinutes, &r.EtaKm
		if d, err := s.store.driver(r.DriverKey); err == nil {
			v.Car = &d.Car
		} else {
			s.log.Error("store", "op", "ride driver", "ride", r.ID, "err", err)
		}
	}
	return v
}

// geocodeRoute resolves the addresses whose coordinates are not given,
// writing an error on failure.
func (s *Server) geocodeRoute(ctx context.Context, w http.ResponseWriter, startAddr, endAddr string, startAt, endAt *geo.Point) (start, end geo.Point, ok bool) {
	for _, a := range []struct {
		name, addr string
		at, p      *geo.Point
	}{{"start_address", startAddr, startAt, &start}, {"end_address", endAddr, endAt, &end}} {
		if a.at != nil {
			*a.p = *a.at
			continue
		}
		p, err := s.cfg.Geocoder.Geocode(ctx, a.addr)
		switch {
		case errors.Is(err, geo.ErrNotFound):
			writeError(w, http.StatusUnprocessableEntity, a.name+" not found")
			return start, end, false
		case err != nil:
			s.log.Error("geocode", "field", a.name, "err", err)
			writeError(w, http.StatusBadGateway, "could not geocode "+a.name)
			return start, end, false
		}
		*a.p = p
	}
	return start, end, true
}

// search lists addresses matching q, best first, so the rider can pick the
// one meant before requesting a ride.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit := DefaultSearchResults
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > geo.MaxSearchResults {
			writeError(w, http.StatusBadRequest, "limit must be 1-"+strconv.Itoa(geo.MaxSearchResults))
			return
		}
		limit = n
	}
	places, err := s.cfg.Geocoder.Search(r.Context(), query, limit)
	if err != nil {
		s.log.Error("search", "err", err)
		writeError(w, http.StatusBadGateway, "could not search addresses")
		return
	}
	writeJSON(w, http.StatusOK, places)
}

type fareRange struct {
	Network x402.Network `json:"network,omitempty"`
	Min     uint64       `json:"min"`
	Max     uint64       `json:"max"`
}

type quote struct {
	StartAddress string    `json:"start_address"`
	EndAddress   string    `json:"end_address"`
	Start        geo.Point `json:"start"`
	End          geo.Point `json:"end"`
	DistanceKm   float64   `json:"distance_km"`
	// Fares ranges over the registered drivers of each network, in its
	// smallest unit. FareLovelace repeats the Cardano range for older
	// clients.
	Fares        []fareRange `json:"fares"`
	FareLovelace *fareRange  `json:"fare_lovelace,omitempty"`
}

func (s *Server) quoteRide(w http.ResponseWriter, r *http.Request) {
	startAddr, endAddr := r.URL.Query().Get("start_address"), r.URL.Query().Get("end_address")
	if startAddr == "" || endAddr == "" {
		writeError(w, http.StatusBadRequest, "start_address and end_address are required")
		return
	}
	var at [2]*geo.Point
	for i, name := range []string{"start", "end"} {
		if v := r.URL.Query().Get(name); v != "" {
			p, err := geo.ParsePoint(v)
			if err != nil {
				writeError(w, http.StatusBadRequest, name+": "+err.Error())
				return
			}
			at[i] = &p
		}
	}
	start, end, ok := s.geocodeRoute(r.Context(), w, startAddr, endAddr, at[0], at[1])
	if !ok {
		return
	}
	q := quote{StartAddress: startAddr, EndAddress: endAddr, Start: start, End: end, DistanceKm: geo.Distance(start, end), Fares: []fareRange{}}
	drivers, err := s.store.allDrivers()
	if err != nil {
		s.storeError(w, "all drivers", err)
		return
	}
	for _, chain := range []Chain{ChainCardano, ChainBSV} {
		var fr *fareRange
		for _, d := range drivers {
			if d.Chain != chain {
				continue
			}
			f := fare(d, q.DistanceKm)
			if fr == nil {
				fr = &fareRange{Network: s.network(chain), Min: f, Max: f}
			}
			fr.Min, fr.Max = min(fr.Min, f), max(fr.Max, f)
		}
		if fr == nil {
			continue
		}
		q.Fares = append(q.Fares, *fr)
		if chain == ChainCardano {
			q.FareLovelace = &fareRange{Min: fr.Min, Max: fr.Max}
		}
	}
	writeJSON(w, http.StatusOK, q)
}

type createRideRequest struct {
	StartAddress string    `json:"start_address"`
	EndAddress   string    `json:"end_address"`
	PickupTime   time.Time `json:"pickup_time"`
	// CosignerKey is the rider's optional public key for this ride, a
	// Cardano "addr_shared_vk" or a BSV compressed public key in hex, and
	// only drivers of that chain can take the ride. Without it the server
	// cosigns in the rider's place and drivers of any chain can.
	CosignerKey string `json:"cosigner_key,omitempty"`
	// Start and End, when given, are used instead of geocoding the
	// addresses, which then only label the ride.
	Start *geo.Point `json:"start,omitempty"`
	End   *geo.Point `json:"end,omitempty"`
}

func (s *Server) createRide(w http.ResponseWriter, r *http.Request) {
	var req createRideRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.StartAddress == "" || req.EndAddress == "" || req.PickupTime.IsZero() {
		writeError(w, http.StatusBadRequest, "start_address, end_address and pickup_time (RFC 3339) are required")
		return
	}
	if (req.Start != nil && !req.Start.Valid()) || (req.End != nil && !req.End.Valid()) {
		writeError(w, http.StatusBadRequest, "start and end must be valid lat/lng coordinates")
		return
	}
	req.CosignerKey = strings.TrimSpace(req.CosignerKey)
	if req.CosignerKey != "" {
		_, key, err := s.checkRiderKey(req.CosignerKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cosigner_key: "+err.Error())
			return
		}
		req.CosignerKey = key
	}
	start, end, ok := s.geocodeRoute(r.Context(), w, req.StartAddress, req.EndAddress, req.Start, req.End)
	if !ok {
		return
	}
	ride, err := s.store.addRide(Ride{
		StartAddress: req.StartAddress,
		EndAddress:   req.EndAddress,
		Start:        start,
		End:          end,
		DistanceKm:   geo.Distance(start, end),
		PickupTime:   req.PickupTime,
		RiderKey:     req.CosignerKey,
		CreatedAt:    time.Now(),
	})
	if err != nil {
		s.storeError(w, "add ride", err)
		return
	}
	s.log.Info("ride created", "ride", ride.ID, "index", ride.AddressIndex, "distance_km", ride.DistanceKm,
		"custodial", ride.RiderKey == "")
	writeJSON(w, http.StatusCreated, s.view(ride, false))
}

// getRide is free until a driver accepts. An accepted ride must be paid via
// x402 once; the paid response and every later one include the car.
func (s *Server) getRide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ride, err := s.store.ride(id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "ride not found")
		return
	} else if err != nil {
		s.storeError(w, "ride", err)
		return
	}
	if ride.Status != StatusAccepted {
		writeJSON(w, http.StatusOK, s.view(ride, false))
		return
	}

	price := func(*http.Request) ([]x402.Price, error) {
		return []x402.Price{{
			Network:     s.network(ride.Chain),
			PayTo:       ride.PayTo,
			Amount:      ride.Fare,
			Description: "Kiko ride " + ride.ID,
			MimeType:    "application/json",
		}}, nil
	}
	// The middleware runs this after verifying the payment and settles only
	// if it succeeds, so a ride already being paid by another transaction
	// is refused here rather than charged twice.
	var claimedTx string
	paid := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := x402.PaymentFromContext(r.Context())
		claimed, err := s.store.claim(id, p.TxID)
		if errors.Is(err, errConflict) || errors.Is(err, errNotFound) {
			s.log.Warn("payment refused", "ride", id, "tx", p.TxID, "reason", "ride is already being paid")
			writeError(w, http.StatusConflict, "ride is already being paid")
			return
		} else if err != nil {
			s.storeError(w, "claim", err)
			return
		}
		claimedTx = p.TxID
		claimed.Status, claimed.PaymentTx = StatusPaid, p.TxID
		writeJSON(w, http.StatusOK, s.view(claimed, false))
	})

	sw := &statusWriter{ResponseWriter: w}
	s.cfg.Payments.Require(price)(paid).ServeHTTP(sw, r)
	if claimedTx != "" {
		// The middleware answers 200 only after settlement succeeded.
		if sw.status == http.StatusOK {
			s.log.Info("ride paid", "ride", id, "tx", claimedTx, "chain", ride.Chain, "fare", ride.Fare)
		} else if x402.Unresolved(sw.Header()) {
			// The transaction may still land: keep the ride for it, so
			// only a retry with the same payment can settle it.
			s.log.Warn("ride payment unresolved", "ride", id, "tx", claimedTx, "status", sw.status)
			return
		} else {
			s.log.Warn("ride payment not settled", "ride", id, "tx", claimedTx, "status", sw.status)
		}
		if err := s.store.finishClaim(id, claimedTx, sw.status == http.StatusOK); err != nil {
			// Settled but not recorded: the claim stays, so only this
			// transaction can claim the ride again.
			s.log.Error("store", "op", "finish claim", "ride", id, "tx", claimedTx, "settled", sw.status == http.StatusOK, "err", err)
		}
	}
}
