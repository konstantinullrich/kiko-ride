// Package api implements Kiko's HTTP API for drivers and riders.
//
// Drivers register, look for requested rides near them and accept one. The
// rider polls GET /api/v1/ride/{id}; once a driver has accepted, that call
// requires the fare as an x402 payment to the ride's 2-of-3 multisig address,
// and only the paid response reveals the driver and car.
//
// The ride address is locked by the Kiko server key, the driver's public key
// and the rider's cosigner key. A rider who brings no key gets the server's
// backup key in their place, so the server holds two of the three.
//
// The driver is paid out by building a transaction that spends the ride
// address, signing it and posting it to .../drive/{ride_id}/payout; the
// server adds the second signature and returns it for the driver to submit.
//
// Drivers identify themselves with the X-Driver-ID header (the id returned by
// POST /api/v1/driver). State is kept in memory.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"

	"kiko-backend/geo"
	"kiko-backend/multisig"
	"kiko-backend/x402"
)

// MinFareLovelace is the lowest fare charged. A pure-lovelace payment output
// must clear the ledger's min-UTxO, roughly 1 ADA.
const MinFareLovelace = 1_000_000

// DefaultSearchRangeKm is used when GET .../available has no range.
const DefaultSearchRangeKm = 5

// HeaderDriverID identifies the calling driver.
const HeaderDriverID = "X-Driver-ID"

type Config struct {
	Network x402.Network
	// Cosigner is Kiko's server cosigner; each ride is paid to its own
	// 2-of-3 address with the driver and rider.
	Cosigner *multisig.Server
	// FirstAddressIndex is the address index the first ride uses. Rides are
	// in memory, so after a restart set it past the indexes already handed
	// out.
	FirstAddressIndex uint32
	Geocoder          geo.Geocoder
	// Payments gates GET /api/v1/ride/{id}.
	Payments *x402.Middleware
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

type Server struct {
	cfg   Config
	log   *slog.Logger
	store *store
}

func New(cfg Config) (*Server, error) {
	if cfg.Cosigner == nil || cfg.Geocoder == nil || cfg.Payments == nil {
		return nil, errors.New("cosigner, geocoder and payments are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{cfg: cfg, log: log, store: newStore(cfg.FirstAddressIndex)}, nil
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/driver", s.createDriver)
	mux.HandleFunc("GET /api/v1/driver/drive/available", s.availableRides)
	mux.HandleFunc("POST /api/v1/driver/drive/{ride_id}", s.acceptRide)
	mux.HandleFunc("GET /api/v1/driver/drive/{ride_id}", s.driverRide)
	mux.HandleFunc("POST /api/v1/driver/drive/{ride_id}/payout", s.payout)
	mux.HandleFunc("GET /api/v1/ride", s.quoteRide)
	mux.HandleFunc("POST /api/v1/ride", s.createRide)
	mux.HandleFunc("GET /api/v1/ride/{id}", s.getRide)
	return mux
}

// fare is base_fee + fee_per_km × km, rounded up to the lovelace and never
// below MinFareLovelace.
func fare(d Driver, km float64) uint64 {
	f := d.BaseFee + uint64(math.Ceil(float64(d.FeePerKm)*km))
	return max(f, MinFareLovelace)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// statusWriter records the status code written through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
