package api

import (
	"cmp"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Salvionied/apollo/v2/backend"
	"github.com/blinklabs-io/gouroboros/cbor"

	"kiko-backend/geo"
	"kiko-backend/multisig"
)

func (s *Server) createDriver(w http.ResponseWriter, r *http.Request) {
	var d Driver
	if !decodeJSON(w, r, &d) {
		return
	}
	d.PublicKey = normalizeKey(d.PublicKey)
	if err := s.cfg.Cosigner.CheckCosignerKey(d.PublicKey); err != nil {
		writeError(w, http.StatusBadRequest, "public_key must be an acct_shared_xvk cosigner key: "+err.Error())
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
	d, err := s.store.addDriver(d)
	if errors.Is(err, errConflict) {
		writeError(w, http.StatusConflict, "a driver with this public_key is already registered")
		return
	} else if err != nil {
		s.storeError(w, "add driver", err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// normalizeKey trims a bech32 key and lowercases it; bech32 is case
// insensitive, so both spellings name the same driver.
func normalizeKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// currentDriver resolves the X-Driver-Key header, writing an error if
// unknown.
func (s *Server) currentDriver(w http.ResponseWriter, r *http.Request) (Driver, bool) {
	d, err := s.store.driver(normalizeKey(r.Header.Get(HeaderDriverKey)))
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

// availableRides lists requested rides whose pickup lies within range km of
// location, nearest first.
func (s *Server) availableRides(w http.ResponseWriter, r *http.Request) {
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
	// The address index and rider key are fixed at creation, so the address
	// can be derived before taking the ride.
	wallet, _, err := s.cfg.Cosigner.RideWallet(d.PublicKey, pending.RiderKey)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "cannot form the ride wallet: "+err.Error())
		return
	}
	payTo, err := wallet.Address(pending.AddressIndex)
	if err != nil {
		s.log.Error("derive ride address", "ride", rideID, "index", pending.AddressIndex, "err", err)
		writeError(w, http.StatusInternalServerError, "could not derive the ride address")
		return
	}
	script, err := cbor.Encode(&payTo.Script)
	if err != nil {
		s.log.Error("encode ride script", "ride", rideID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not derive the ride address")
		return
	}
	ride, err := s.store.accept(rideID, d, payTo.String(), hex.EncodeToString(script), req.EtaMinutes, req.EtaKm)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "ride not found")
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, "ride is no longer available")
	case err != nil:
		s.storeError(w, "accept", err)
	default:
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
	// Transaction is the hex CBOR of a transaction spending the ride address,
	// already signed by the driver's key at the ride's address index.
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
	txCbor, err := hex.DecodeString(strings.TrimSpace(req.Transaction))
	if err != nil || len(txCbor) == 0 {
		writeError(w, http.StatusBadRequest, "transaction must be hex CBOR")
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
	wallet, _, err := s.cfg.Cosigner.RideWallet(d.PublicKey, ride.RiderKey)
	if err != nil {
		s.log.Error("ride wallet", "ride", rideID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not form the ride wallet")
		return
	}
	signed, txID, err := s.cfg.Cosigner.Cosign(wallet, ride.AddressIndex, d.PublicKey, txCbor)
	if errors.Is(err, multisig.ErrMissingSignature) {
		writeError(w, http.StatusUnprocessableEntity, "transaction must be signed by the driver first")
		return
	} else if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "cannot cosign: "+err.Error())
		return
	}
	// The signed transaction never leaves the server unless the payout is
	// approved, so a dispute raised meanwhile wins.
	switch err := s.store.beginPayout(rideID, d.PublicKey); {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "ride not found")
		return
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, "ride is not paid or is already being paid out")
		return
	case err != nil:
		s.storeError(w, "begin payout", err)
		return
	}
	if _, err := backend.SubmitTxContext(r.Context(), s.cfg.Chain, signed); err != nil {
		if _, ferr := s.store.finishPayout(rideID, txID.String(), false); ferr != nil {
			s.log.Error("store", "op", "finish payout", "ride", rideID, "err", ferr)
		}
		s.log.Warn("submit payout", "ride", rideID, "tx", txID, "err", err)
		writeError(w, http.StatusBadGateway, "payout rejected: "+err.Error())
		return
	}
	paidOut, err := s.store.finishPayout(rideID, txID.String(), true)
	if err != nil {
		// Submitted but not recorded: the ride stays paid and a repeated
		// payout is refused by the chain, its inputs being spent.
		s.log.Error("store", "op", "finish payout", "ride", rideID, "tx", txID, "submitted", true, "err", err)
		writeError(w, http.StatusInternalServerError, "payout "+txID.String()+" submitted but not recorded")
		return
	}
	writeJSON(w, http.StatusOK, s.view(paidOut, true))
}
