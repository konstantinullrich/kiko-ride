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

	FareLovelace uint64   `json:"fare_lovelace,omitempty"`
	PayTo        string   `json:"pay_to,omitempty"`
	EtaMinutes   *int     `json:"eta_minutes,omitempty"`
	EtaKm        *float64 `json:"eta_km,omitempty"`
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
		FareLovelace: r.FareLovelace,
		PaymentTx:    r.PaymentTx,
	}
	if r.Status == StatusRequested {
		return v
	}
	v.PayTo = r.PayTo
	if forDriver {
		v.AddressIndex, v.Script, v.PayoutTx = &r.AddressIndex, r.Script, r.PayoutTx
	}
	if forDriver || r.Status == StatusPaid || r.Status == StatusPaidOut {
		v.EtaMinutes, v.EtaKm = &r.EtaMinutes, &r.EtaKm
		if d, ok := s.store.driver(r.DriverKey); ok {
			v.Car = &d.Car
		}
	}
	return v
}

// geocodeRoute resolves both addresses, writing an error on failure.
func (s *Server) geocodeRoute(ctx context.Context, w http.ResponseWriter, startAddr, endAddr string) (start, end geo.Point, ok bool) {
	for _, a := range []struct {
		name, addr string
		p          *geo.Point
	}{{"start_address", startAddr, &start}, {"end_address", endAddr, &end}} {
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
	Min uint64 `json:"min"`
	Max uint64 `json:"max"`
}

type quote struct {
	StartAddress string     `json:"start_address"`
	EndAddress   string     `json:"end_address"`
	Start        geo.Point  `json:"start"`
	End          geo.Point  `json:"end"`
	DistanceKm   float64    `json:"distance_km"`
	FareLovelace *fareRange `json:"fare_lovelace,omitempty"` // over registered drivers
}

func (s *Server) quoteRide(w http.ResponseWriter, r *http.Request) {
	startAddr, endAddr := r.URL.Query().Get("start_address"), r.URL.Query().Get("end_address")
	if startAddr == "" || endAddr == "" {
		writeError(w, http.StatusBadRequest, "start_address and end_address are required")
		return
	}
	start, end, ok := s.geocodeRoute(r.Context(), w, startAddr, endAddr)
	if !ok {
		return
	}
	q := quote{StartAddress: startAddr, EndAddress: endAddr, Start: start, End: end, DistanceKm: geo.Distance(start, end)}
	for _, d := range s.store.allDrivers() {
		f := fare(d, q.DistanceKm)
		if q.FareLovelace == nil {
			q.FareLovelace = &fareRange{Min: f, Max: f}
		}
		q.FareLovelace.Min = min(q.FareLovelace.Min, f)
		q.FareLovelace.Max = max(q.FareLovelace.Max, f)
	}
	writeJSON(w, http.StatusOK, q)
}

type createRideRequest struct {
	StartAddress string    `json:"start_address"`
	EndAddress   string    `json:"end_address"`
	PickupTime   time.Time `json:"pickup_time"`
	// CosignerKey is the rider's optional "acct_shared_xvk". Without it the
	// server cosigns in the rider's place.
	CosignerKey string `json:"cosigner_key,omitempty"`
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
	req.CosignerKey = strings.TrimSpace(req.CosignerKey)
	if req.CosignerKey != "" {
		if err := s.cfg.Cosigner.CheckCosignerKey(req.CosignerKey); err != nil {
			writeError(w, http.StatusBadRequest, "cosigner_key: "+err.Error())
			return
		}
	}
	start, end, ok := s.geocodeRoute(r.Context(), w, req.StartAddress, req.EndAddress)
	if !ok {
		return
	}
	ride := s.store.addRide(Ride{
		StartAddress: req.StartAddress,
		EndAddress:   req.EndAddress,
		Start:        start,
		End:          end,
		DistanceKm:   geo.Distance(start, end),
		PickupTime:   req.PickupTime,
		RiderKey:     req.CosignerKey,
		CreatedAt:    time.Now(),
	})
	writeJSON(w, http.StatusCreated, s.view(ride, false))
}

// getRide is free until a driver accepts. An accepted ride must be paid via
// x402 once; the paid response and every later one include the car.
func (s *Server) getRide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ride, ok := s.store.ride(id)
	if !ok {
		writeError(w, http.StatusNotFound, "ride not found")
		return
	}
	if ride.Status != StatusAccepted {
		writeJSON(w, http.StatusOK, s.view(ride, false))
		return
	}

	price := func(*http.Request) (x402.Price, error) {
		return x402.Price{
			PayTo:       ride.PayTo,
			Amount:      ride.FareLovelace,
			Description: "Kiko ride " + ride.ID,
			MimeType:    "application/json",
		}, nil
	}
	// The middleware runs this after verifying the payment and settles only
	// if it succeeds, so a ride already being paid by another transaction
	// is refused here rather than charged twice.
	var claimedTx string
	paid := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := x402.PaymentFromContext(r.Context())
		claimed, ok := s.store.claim(id, p.TxID)
		if !ok {
			writeError(w, http.StatusConflict, "ride is already being paid")
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
		s.store.finishClaim(id, claimedTx, sw.status == http.StatusOK)
	}
}
