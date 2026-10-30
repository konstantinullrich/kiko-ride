// Package api implements Kiko's HTTP API for drivers and riders.
//
// Drivers register, look for requested rides near them and accept one. The
// rider polls GET /api/v1/ride/{id}; once a driver has accepted, that call
// requires the fare as an x402 payment to the ride's 2-of-3 multisig address,
// and only the paid response reveals the car.
//
// The ride address is locked by the Kiko server key, the driver's public key
// and the rider's cosigner key. A rider who brings no key gets the server's
// backup key in their place, so the server holds two of the three.
//
// The driver is paid out by building a transaction that spends the ride
// address, signing it and posting it to .../drive/{ride_id}/payout; the
// server adds the second signature and submits it.
//
// Drivers are identified by their public key, sent in the X-Driver-Key
// header. State is kept in SQLite.
package api

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/Salvionied/apollo/v2/backend"

	"kiko-backend/geo"
	"kiko-backend/multisig"
	"kiko-backend/x402"
)

// MinFareLovelace is the lowest Cardano fare charged. A pure-lovelace payment
// output must clear the ledger's min-UTxO, roughly 1 ADA.
const MinFareLovelace = 1_000_000

// DefaultSearchRangeKm is used when GET .../available has no range.
const DefaultSearchRangeKm = 5

// DefaultSearchResults is used when GET /api/v1/search has no limit.
const DefaultSearchResults = 5

// MinCarYear is the oldest model year a driver may register.
const MinCarYear = 1900

// HeaderDriverKey carries the calling driver's public key.
const HeaderDriverKey = "X-Driver-Key"

type Config struct {
	Network x402.Network
	// Cosigner is Kiko's server cosigner; each ride is paid to its own
	// 2-of-3 address with the driver and rider.
	Cosigner *multisig.Server
	// DB stores drivers and rides; open it with OpenDB.
	DB       *sql.DB
	Geocoder geo.Geocoder
	// Payments gates GET /api/v1/ride/{id}.
	Payments *x402.Middleware
	// Chain submits Cardano driver payouts.
	Chain backend.ChainContext
	// BSV, when set, accepts drivers and riders with BSV keys.
	BSV *BSVConfig
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

type Server struct {
	cfg   Config
	log   *slog.Logger
	store *store
}

func New(cfg Config) (*Server, error) {
	if cfg.Cosigner == nil || cfg.DB == nil || cfg.Geocoder == nil || cfg.Payments == nil || cfg.Chain == nil {
		return nil, errors.New("cosigner, db, geocoder, payments and chain are required")
	}
	if b := cfg.BSV; b != nil && (b.Cosigner == nil || b.Broadcaster == nil || b.Network == "") {
		return nil, errors.New("bsv needs a network, cosigner and broadcaster")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	st, err := newStore(cfg.DB)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, log: log, store: st}, nil
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
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/currency", s.getCurrency)
	return mux
}

// fare is base_fee + fee_per_km × km in the driver's currency, rounded up to
// its smallest unit and never below MinFareLovelace on Cardano or
// MinFareSatoshi on BSV.
func fare(d Driver, km float64) uint64 {
	f := d.BaseFee + uint64(math.Ceil(float64(d.FeePerKm)*km))
	if d.Chain == ChainBSV {
		return max(f, MinFareSatoshi)
	}
	return max(f, MinFareLovelace)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
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

// storeError logs a failed database call and answers 500.
func (s *Server) storeError(w http.ResponseWriter, op string, err error) {
	s.log.Error("store", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
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

// Flush keeps streaming responses, such as MCP's, working through the
// wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// LogRequests logs every request with its status and duration: server
// errors at error level, client errors at warn level, the rest at info.
func LogRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := cmp.Or(sw.status, http.StatusOK)
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", status,
			"duration", time.Since(start).Round(time.Millisecond), "remote", r.RemoteAddr)
	})
}

func (s *Server) getCurrency(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"currency": []string{"cardano", "bsv"}})
}
