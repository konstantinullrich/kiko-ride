package x402

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
)

// DefaultMaxTimeoutSeconds is the payment window the Cardano scheme examples
// use; Cardano blocks are slow, so keep it generous.
const DefaultMaxTimeoutSeconds = 600

// Price is what a request costs on one network and where the money goes.
type Price struct {
	// Network may be left empty when the middleware accepts one network.
	Network Network
	// PayTo receives the payment: a bech32 address on Cardano; a P2PKH
	// address or a hex locking script on BSV.
	PayTo string
	// Amount is in the network's native coin: lovelace on Cardano, satoshis
	// on BSV. A pure-lovelace payment output must clear the ledger's
	// min-UTxO, roughly 1 ADA.
	Amount uint64
	// Description and MimeType describe the resource to the client.
	Description string
	MimeType    string
}

// PriceFunc picks the prices for a request, one per way the client may pay,
// in order of preference. It is called again on the paid retry and must
// return the same prices for the same request, otherwise the payment is
// rejected as made against different requirements.
type PriceFunc func(r *http.Request) ([]Price, error)

// Fixed charges the same amount of the native coin to the same payTo on
// every request, on the middleware's only network.
func Fixed(payTo string, amount uint64) PriceFunc {
	return func(*http.Request) ([]Price, error) {
		return []Price{{PayTo: payTo, Amount: amount}}, nil
	}
}

// NetworkConfig accepts payments on one network.
type NetworkConfig struct {
	Network     Network
	Facilitator Facilitator
	// MaxTimeoutSeconds defaults to DefaultMaxTimeoutSeconds.
	MaxTimeoutSeconds int
}

// Config configures the middleware.
type Config struct {
	// Networks are the networks payments are accepted on, each once.
	Networks []NetworkConfig
	// BaseURL, e.g. "https://api.example.com", is used for resource.url.
	// When empty it is derived from the request.
	BaseURL string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

type network struct {
	NetworkConfig
	chain chain
}

// Middleware gates handlers behind x402 payments.
type Middleware struct {
	cfg      Config
	log      *slog.Logger
	networks map[Network]*network
	inFlight sync.Map // network and canonical tx id -> struct{}
}

// New validates the config and returns a Middleware.
func New(cfg Config) (*Middleware, error) {
	if len(cfg.Networks) == 0 {
		return nil, errors.New("at least one network is required")
	}
	networks := make(map[Network]*network, len(cfg.Networks))
	for _, nc := range cfg.Networks {
		c, err := chainFor(nc.Network)
		if err != nil {
			return nil, err
		}
		if _, dup := networks[nc.Network]; dup {
			return nil, fmt.Errorf("network %s is configured twice", nc.Network)
		}
		if nc.Facilitator == nil {
			return nil, fmt.Errorf("%s: facilitator is required", nc.Network)
		}
		if nc.MaxTimeoutSeconds == 0 {
			nc.MaxTimeoutSeconds = DefaultMaxTimeoutSeconds
		}
		networks[nc.Network] = &network{NetworkConfig: nc, chain: c}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Middleware{cfg: cfg, log: log, networks: networks}, nil
}

// Paid describes the verified payment, available to the handler via
// PaymentFromContext. The payment is settled only after the handler returns
// a non-error status.
type Paid struct {
	Requirements PaymentRequirements
	Payer        string
	TxID         string
}

type ctxKey struct{}

// PaymentFromContext returns the verified payment for the current request.
func PaymentFromContext(ctx context.Context) (Paid, bool) {
	p, ok := ctx.Value(ctxKey{}).(Paid)
	return p, ok
}

// Require wraps a handler so that each request must be paid according to
// price.
func (m *Middleware) Require(price PriceFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.serve(w, r, price, next)
		})
	}
}

func (m *Middleware) serve(w http.ResponseWriter, r *http.Request, price PriceFunc, next http.Handler) {
	prices, err := price(r)
	if err == nil && len(prices) == 0 {
		err = errors.New("no prices")
	}
	if err != nil {
		m.log.Error("x402: price", "err", err)
		http.Error(w, "pricing failed", http.StatusInternalServerError)
		return
	}
	reqs := make([]PaymentRequirements, len(prices))
	for i, p := range prices {
		if reqs[i], err = m.requirements(p); err != nil {
			m.log.Error("x402: invalid price", "err", err)
			http.Error(w, "pricing failed", http.StatusInternalServerError)
			return
		}
	}
	resource := ResourceInfo{URL: m.resourceURL(r), Description: prices[0].Description, MimeType: prices[0].MimeType}

	header := r.Header.Get(HeaderPaymentSignature)
	if header == "" {
		m.paymentRequired(w, resource, reqs, "PAYMENT-SIGNATURE header is required")
		return
	}
	payload, err := decodePayload(header)
	if err != nil {
		m.log.Warn("x402: invalid payload", "resource", resource.URL, "err", err)
		writeJSONError(w, http.StatusBadRequest, ReasonInvalidPayload, err.Error())
		return
	}
	if payload.X402Version != Version {
		writeJSONError(w, http.StatusBadRequest, ReasonInvalidPayload, "unsupported x402Version")
		return
	}
	i := slices.IndexFunc(reqs, func(req PaymentRequirements) bool { return sameRequirements(payload.Accepted, req) })
	if i < 0 {
		m.log.Warn("x402: requirements changed", "resource", resource.URL)
		m.paymentRequired(w, resource, reqs, ReasonRequirementsChanged)
		return
	}
	req := reqs[i]
	n := m.networks[req.Network]
	txID, err := n.chain.transactionID(payload.Payload.Transaction)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, ReasonInvalidPayload, err.Error())
		return
	}

	// One settlement per transaction at a time, keyed by the canonical tx
	// id so a re-encoded copy of the same transaction cannot slip through.
	key := string(req.Network) + " " + txID
	if _, busy := m.inFlight.LoadOrStore(key, struct{}{}); busy {
		m.log.Warn("x402: duplicate settlement", "network", req.Network, "tx", txID)
		m.settleFailed(w, SettleResponse{ErrorReason: ReasonDuplicateSettlement, Transaction: txID, Network: req.Network})
		return
	}
	defer m.inFlight.Delete(key)

	ctx := r.Context()
	verify, err := n.Facilitator.Verify(ctx, payload, req)
	if err != nil {
		m.log.Error("x402: verify", "network", req.Network, "tx", txID, "err", err)
		m.paymentRequired(w, resource, reqs, ReasonUnexpectedVerify)
		return
	}
	if !verify.IsValid {
		m.log.Warn("x402: payment invalid", "network", req.Network, "tx", txID, "payer", verify.Payer, "reason", verify.InvalidReason)
		m.paymentRequired(w, resource, reqs, verify.InvalidReason)
		return
	}

	// Run the handler with its response held back until settlement.
	rec := newRecorder()
	paid := Paid{Requirements: req, Payer: verify.Payer, TxID: txID}
	next.ServeHTTP(rec, r.WithContext(context.WithValue(ctx, ctxKey{}, paid)))
	if rec.status >= 400 {
		// The resource failed, so the client is not charged.
		m.log.Warn("x402: not settled, resource failed", "network", req.Network, "tx", txID, "status", rec.status)
		rec.flush(w, nil)
		return
	}

	settle, err := m.settle(ctx, n.Facilitator, payload, req)
	if err != nil {
		m.log.Error("x402: settle", "network", req.Network, "tx", txID, "err", err)
		m.settleFailed(w, SettleResponse{ErrorReason: ReasonUnexpectedSettle, Transaction: txID, Network: req.Network})
		return
	}
	if !settle.Success {
		if settle.Network == "" {
			settle.Network = req.Network
		}
		m.log.Warn("x402: settlement failed", "network", req.Network, "tx", txID, "reason", settle.ErrorReason)
		m.settleFailed(w, settle)
		return
	}
	m.log.Info("x402: settled", "network", req.Network, "tx", settle.Transaction, "payer", verify.Payer,
		"amount", req.Amount, "asset", req.Asset, "pay_to", req.PayTo)
	rec.flush(w, &settle)
}

// settle calls /settle and, as the scheme requires, retries once with the
// same payload when the facilitator reports settlement_pending.
func (m *Middleware) settle(ctx context.Context, f Facilitator, p PaymentPayload, req PaymentRequirements) (SettleResponse, error) {
	resp, err := f.Settle(ctx, p, req)
	if err == nil && !resp.Success && resp.ErrorReason == ReasonSettlementPending {
		return f.Settle(ctx, p, req)
	}
	return resp, err
}

func (m *Middleware) requirements(p Price) (PaymentRequirements, error) {
	if p.Network == "" {
		if len(m.networks) != 1 {
			return PaymentRequirements{}, errors.New("price needs a network when several are accepted")
		}
		p.Network = m.cfg.Networks[0].Network
	}
	n, ok := m.networks[p.Network]
	if !ok {
		return PaymentRequirements{}, fmt.Errorf("network %s is not accepted", p.Network)
	}
	if p.Amount == 0 {
		return PaymentRequirements{}, errors.New("amount must be positive")
	}
	if err := n.chain.checkPayTo(p.PayTo); err != nil {
		return PaymentRequirements{}, err
	}
	return PaymentRequirements{
		Scheme:            "exact",
		Network:           p.Network,
		Amount:            strconv.FormatUint(p.Amount, 10),
		Asset:             n.chain.asset(),
		PayTo:             p.PayTo,
		MaxTimeoutSeconds: n.MaxTimeoutSeconds,
	}, nil
}

func (m *Middleware) resourceURL(r *http.Request) string {
	if m.cfg.BaseURL != "" {
		return m.cfg.BaseURL + r.URL.RequestURI()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

func (m *Middleware) paymentRequired(w http.ResponseWriter, resource ResourceInfo, reqs []PaymentRequirements, reason string) {
	body := PaymentRequired{
		X402Version: Version,
		Error:       reason,
		Resource:    resource,
		Accepts:     reqs,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set(HeaderPaymentRequired, base64.StdEncoding.EncodeToString(raw))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write(raw)
}

// Unresolved reports whether the PAYMENT-RESPONSE in a paid request's
// response headers leaves the payment undecided: settlement is pending, or
// the facilitator failed without a verdict. The transaction may still land,
// so what it pays for should stay reserved for it.
func Unresolved(h http.Header) bool {
	raw, err := base64.StdEncoding.DecodeString(h.Get(HeaderPaymentResponse))
	if err != nil || len(raw) == 0 {
		return false
	}
	var resp SettleResponse
	if json.Unmarshal(raw, &resp) != nil {
		return false
	}
	return !resp.Success && (resp.ErrorReason == ReasonSettlementPending || resp.ErrorReason == ReasonUnexpectedSettle)
}

func (m *Middleware) settleFailed(w http.ResponseWriter, resp SettleResponse) {
	resp.Success = false
	raw, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set(HeaderPaymentResponse, base64.StdEncoding.EncodeToString(raw))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write([]byte("{}"))
}

func decodePayload(header string) (PaymentPayload, error) {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return PaymentPayload{}, fmt.Errorf("PAYMENT-SIGNATURE is not base64: %w", err)
	}
	var p PaymentPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return PaymentPayload{}, fmt.Errorf("PAYMENT-SIGNATURE is not valid JSON: %w", err)
	}
	if p.Payload.Transaction == "" || p.Payload.Nonce == "" {
		return PaymentPayload{}, errors.New("payload.transaction and payload.nonce are required")
	}
	return p, nil
}

// sameRequirements reports whether the client's accepted requirements equal
// ours, comparing the JSON forms so extra is matched structurally.
func sameRequirements(a, b PaymentRequirements) bool {
	norm := func(r PaymentRequirements) any {
		raw, _ := json.Marshal(r)
		var v any
		_ = json.Unmarshal(raw, &v)
		return v
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func writeJSONError(w http.ResponseWriter, status int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": reason, "message": msg})
}

// recorder buffers a handler's response so it can be released only after
// settlement succeeds.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder {
	return &recorder{header: http.Header{}}
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *recorder) flush(w http.ResponseWriter, settle *SettleResponse) {
	for k, vs := range r.header {
		w.Header()[k] = vs
	}
	if settle != nil {
		if raw, err := json.Marshal(settle); err == nil {
			w.Header().Set(HeaderPaymentResponse, base64.StdEncoding.EncodeToString(raw))
		}
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	w.WriteHeader(r.status)
	w.Write(r.body.Bytes())
}
