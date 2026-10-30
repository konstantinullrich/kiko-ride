package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"kiko-backend/geo"
	"kiko-backend/x402"
)

// driverView is a registered driver with the network their fares are paid
// on.
type driverView struct {
	Driver
	Network x402.Network `json:"network"`
}

// createDriver registers a driver, or replaces their profile. The request
// must be signed by the public_key it registers. The format of that key
// decides the chain they are paid on and so the currency of their fees.
func (s *Server) createDriver(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var d Driver
	if !decodeJSON(w, r, &d) {
		return
	}
	chain, key, err := s.checkCosignerKey(d.PublicKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "public_key: "+err.Error())
		return
	}
	d.PublicKey, d.Chain = key, chain
	if !s.checkSignature(w, r, d.PublicKey, body) {
		return
	}
	if d.Car.Brand == "" || d.Car.Model == "" || d.Car.Color == "" || d.Car.LicensePlateNumber == "" {
		writeError(w, http.StatusBadRequest, "car brand, model, color and license_plate_number are required")
		return
	}
	// Model years run up to a year ahead of the calendar.
	if maxYear := time.Now().Year() + 1; d.Car.Year < MinCarYear || d.Car.Year > maxYear {
		writeError(w, http.StatusBadRequest, "car year must be "+strconv.Itoa(MinCarYear)+"-"+strconv.Itoa(maxYear))
		return
	}
	created, err := s.store.putDriver(d)
	if err != nil {
		s.storeError(w, "put driver", err)
		return
	}
	status, event := http.StatusCreated, "driver registered"
	if !created {
		status, event = http.StatusOK, "driver updated"
	}
	s.log.Info(event, "driver", d.PublicKey, "chain", d.Chain, "base_fee", d.BaseFee, "fee_per_km", d.FeePerKm)
	writeJSON(w, status, driverView{Driver: d, Network: s.network(d.Chain)})
}

// rideChain returns the chain a requested ride is tied to by its rider's
// key; a custodial ride is open to any chain and returns "".
func rideChain(r Ride) Chain {
	if r.RiderKey == "" {
		return ""
	}
	chain, _, _ := parseRiderKey(r.RiderKey)
	return chain
}

// currentDriver resolves the X-Driver-Key header and checks the request's
// signature by that key, writing an error if either fails. The body stays
// readable for the handler.
func (s *Server) currentDriver(w http.ResponseWriter, r *http.Request) (Driver, bool) {
	key := normalizeKey(r.Header.Get(HeaderDriverKey))
	if key == "" {
		writeError(w, http.StatusUnauthorized, HeaderDriverKey+" header must carry a registered driver's public_key")
		return Driver{}, false
	}
	body, ok := readBody(w, r)
	if !ok || !s.checkSignature(w, r, key, body) {
		return Driver{}, false
	}
	d, err := s.store.driver(key)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusUnauthorized, HeaderDriverKey+" header must carry a registered driver's public_key")
		return Driver{}, false
	} else if err != nil {
		s.storeError(w, "driver", err)
		return Driver{}, false
	}
	return d, true
}

type availableRide struct {
	ID               string    `json:"id"`
	StartAddress     string    `json:"start_address"`
	EndAddress       string    `json:"end_address"`
	Start            geo.Point `json:"start"`
	End              geo.Point `json:"end"`
	DistanceKm       float64   `json:"distance_km"`
	PickupDistanceKm float64   `json:"pickup_distance_km"`
	PickupTime       string    `json:"pickup_time"`
}

// availableRides lists requested rides the driver can take whose pickup
// lies within range km of location, nearest first.
func (s *Server) availableRides(w http.ResponseWriter, r *http.Request) {
	driver, ok := s.currentDriver(w, r)
	if !ok {
		return
	}
	loc, err := geo.ParsePoint(r.URL.Query().Get("location"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "location must be lat,lng")
		return
	}
	rangeKm := float64(DefaultSearchRangeKm)
	if v := r.URL.Query().Get("range"); v != "" {
		rangeKm, err = strconv.ParseFloat(v, 64)
		if err != nil || rangeKm <= 0 {
			writeError(w, http.StatusBadRequest, "range must be a positive number of km")
			return
		}
	}
	requested, err := s.store.ridesWithStatus(StatusRequested)
	if err != nil {
		s.storeError(w, "requested rides", err)
		return
	}
	out := []availableRide{}
	for _, ride := range requested {
		if c := rideChain(ride); c != "" && c != driver.Chain {
			continue
		}
		d := geo.Distance(loc, ride.Start)
		if d > rangeKm {
			continue
		}
		out = append(out, availableRide{
			ID:               ride.ID,
			StartAddress:     ride.StartAddress,
			EndAddress:       ride.EndAddress,
			Start:            ride.Start,
			End:              ride.End,
			DistanceKm:       ride.DistanceKm,
			PickupDistanceKm: d,
			PickupTime:       ride.PickupTime.Format(timeFormat),
		})
	}
	slices.SortFunc(out, func(a, b availableRide) int { return cmp.Compare(a.PickupDistanceKm, b.PickupDistanceKm) })
	writeJSON(w, http.StatusOK, out)
}

type acceptRequest struct {
	EtaMinutes int     `json:"eta_minutes"`
	EtaKm      float64 `json:"eta_km"`
}

func (s *Server) acceptRide(w http.ResponseWriter, r *http.Request) {
	d, ok := s.currentDriver(w, r)
	if !ok {
		return
	}
	var req acceptRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EtaMinutes < 0 || req.EtaKm < 0 {
		writeError(w, http.StatusBadRequest, "eta_minutes and eta_km must not be negative")
		return
	}
	rideID := r.PathValue("ride_id")
	pending, err := s.store.ride(rideID)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "ride not found")
		return
	} else if err != nil {
		s.storeError(w, "ride", err)
		return
	}
	if c := rideChain(pending); c != "" && c != d.Chain {
		writeError(w, http.StatusUnprocessableEntity, "the rider pays on "+string(c)+"; this driver is paid on "+string(d.Chain))
		return
	}
	// The address index and rider key are fixed at creation, so the address
	// can be derived before taking the ride.
	payTo, script, err := s.rideAddress(d.Chain, d.PublicKey, pending.RiderKey, pending.AddressIndex)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "cannot form the ride wallet: "+err.Error())
		return
	}
	ride, err := s.store.accept(rideID, d, payTo, script, req.EtaMinutes, req.EtaKm)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "ride not found")
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, "ride is no longer available")
	case err != nil:
		s.storeError(w, "accept", err)
	default:
		s.log.Info("ride accepted", "ride", rideID, "driver", d.PublicKey, "index", ride.AddressIndex,
			"pay_to", ride.PayTo, "chain", ride.Chain, "fare", ride.Fare, "custodial", ride.RiderKey == "")
		writeJSON(w, http.StatusOK, s.view(ride, true))
	}
}

func (s *Server) driverRide(w http.ResponseWriter, r *http.Request) {
	d, ok := s.currentDriver(w, r)
	if !ok {
		return
	}
	ride, err := s.store.ride(r.PathValue("ride_id"))
	if errors.Is(err, errNotFound) || (err == nil && ride.DriverKey != d.PublicKey) {
		writeError(w, http.StatusNotFound, "ride not found")
		return
	} else if err != nil {
		s.storeError(w, "ride", err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(ride, true))
}

type payoutRequest struct {
	// Transaction spends the ride address, already signed by the driver's
	// key at the ride's address index: hex CBOR on Cardano, hex Extended
	// Format on BSV.
	Transaction string `json:"transaction"`
}

// payout cosigns the driver's transaction spending a paid ride's address and
// submits it, answering with the paid-out ride.
func (s *Server) payout(w http.ResponseWriter, r *http.Request) {
	d, ok := s.currentDriver(w, r)
	if !ok {
		return
	}
	var req payoutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rideID := r.PathValue("ride_id")
	ride, err := s.store.ride(rideID)
	if errors.Is(err, errNotFound) || (err == nil && ride.DriverKey != d.PublicKey) {
		writeError(w, http.StatusNotFound, "ride not found")
		return
	} else if err != nil {
		s.storeError(w, "ride", err)
		return
	}
	txID, signed, submit, err := s.cosignPayout(ride, strings.TrimSpace(req.Transaction))
	switch {
	case errors.Is(err, errBadPayout):
		s.log.Warn("payout refused", "ride", rideID, "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, errMissingSignature), errors.Is(err, errCannotCosign):
		s.log.Warn("payout refused", "ride", rideID, "err", err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.log.Error("ride wallet", "ride", rideID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not form the ride wallet")
		return
	}
	// The signed transaction never leaves the server unless the payout is
	// approved, so a dispute raised meanwhile wins.
	switch err := s.store.beginPayout(rideID, d.PublicKey); {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "ride not found")
		return
	case errors.Is(err, errConflict):
		s.log.Warn("payout refused", "ride", rideID, "tx", txID, "status", ride.Status,
			"reason", "ride is not paid or is already being paid out")
		writeError(w, http.StatusConflict, "ride is not paid or is already being paid out")
		return
	case err != nil:
		s.storeError(w, "begin payout", err)
		return
	}
	s.log.Info("submitting payout", "ride", rideID, "chain", ride.Chain, "tx", txID, "index", ride.AddressIndex)
	if err := submit(r.Context()); err != nil {
		if _, ferr := s.store.finishPayout(rideID, txID, false); ferr != nil {
			s.log.Error("store", "op", "finish payout", "ride", rideID, "err", ferr)
		}
		s.log.Warn("submit payout", "ride", rideID, "tx", txID, "tx_hex", signed, "err", err)
		writeError(w, http.StatusBadGateway, "payout rejected: "+err.Error())
		return
	}
	paidOut, err := s.store.finishPayout(rideID, txID, true)
	if err != nil {
		// Submitted but not recorded: the ride stays paid and a repeated
		// payout is refused by the chain, its inputs being spent.
		s.log.Error("store", "op", "finish payout", "ride", rideID, "tx", txID, "submitted", true, "err", err)
		writeError(w, http.StatusInternalServerError, "payout "+txID+" submitted but not recorded")
		return
	}
	s.log.Info("payout submitted", "ride", rideID, "tx", txID)
	writeJSON(w, http.StatusOK, s.view(paidOut, true))
}
