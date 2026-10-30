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
	"regexp"
	"strconv"
	"sync"

	apollo "github.com/Salvionied/apollo/v2"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
)

// DefaultMaxTimeoutSeconds is the payment window the Cardano scheme examples
// use; Cardano blocks are slow, so keep it generous.
const DefaultMaxTimeoutSeconds = 600

var nativeAssetRe = regexp.MustCompile(`^[0-9a-f]{56}\.[0-9a-f]{0,64}$`)

// Price is what a request costs and where the money goes.
type Price struct {
	// PayTo is the bech32 address that receives the payment.
	PayTo string
	// Amount is in atomic units of Asset (lovelace for ADA). A pure-lovelace
	// payment output must clear the ledger's min-UTxO, roughly 1 ADA.
	Amount uint64
	// Asset is Lovelace (the default when empty) or a native token as
	// "policyIdHex.assetNameHex".
	Asset string
	// Description and MimeType describe the resource to the client.
	Description string
	MimeType    string
}

// PriceFunc picks the price for a request. It is called again on the paid
// retry and must return the same Price for the same request, otherwise the
// payment is rejected as made against different requirements.
type PriceFunc func(r *http.Request) (Price, error)

// Fixed charges the same lovelace amount to the same address on every request.
func Fixed(payTo string, lovelace uint64) PriceFunc {
	return func(*http.Request) (Price, error) {
		return Price{PayTo: payTo, Amount: lovelace}, nil
	}
}

// Config configures the middleware.
type Config struct {
	Network     Network
	Facilitator Facilitator
	// MaxTimeoutSeconds defaults to DefaultMaxTimeoutSeconds.
	MaxTimeoutSeconds int
	// Confirmations sets extra.confirmationPolicy.l1Confirmations (-1..20).
	// Nil omits it, which the scheme treats as 1. It must lie within the
	// range the facilitator advertises in /supported.
	Confirmations *int
	// BaseURL, e.g. "https://api.example.com", is used for resource.url.
	// When empty it is derived from the request.
	BaseURL string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Middleware gates handlers behind x402 payments.
type Middleware struct {
	cfg      Config
	log      *slog.Logger
	inFlight sync.Map // canonical tx id -> struct{}
}

// New validates the config and returns a Middleware.
func New(cfg Config) (*Middleware, error) {
	switch cfg.Network {
	case Mainnet, Preprod, Preview:
	default:
		return nil, fmt.Errorf("unsupported network %q", cfg.Network)
	}
	if cfg.Facilitator == nil {
		return nil, errors.New("facilitator is required")
	}
	if cfg.MaxTimeoutSeconds == 0 {
		cfg.MaxTimeoutSeconds = DefaultMaxTimeoutSeconds
	}
	if c := cfg.Confirmations; c != nil && (*c < -1 || *c > 20) {
		return nil, fmt.Errorf("confirmations %d out of range -1..20", *c)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Middleware{cfg: cfg, log: log}, nil
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
	p, err := price(r)
	if err != nil {
		m.log.Error("x402: price", "err", err)
		http.Error(w, "pricing failed", http.StatusInternalServerError)
		return
	}
	req, err := m.requirements(p)
	if err != nil {
		m.log.Error("x402: invalid price", "err", err)
		http.Error(w, "pricing failed", http.StatusInternalServerError)
		return
	}
	resource := ResourceInfo{URL: m.resourceURL(r), Description: p.Description, MimeType: p.MimeType}

	header := r.Header.Get(HeaderPaymentSignature)
	if header == "" {
		m.paymentRequired(w, resource, req, "PAYMENT-SIGNATURE header is required")
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
	if !sameRequirements(payload.Accepted, req) {
		m.log.Warn("x402: requirements changed", "resource", resource.URL)
		m.paymentRequired(w, resource, req, ReasonRequirementsChanged)
		return
	}
	txID, err := transactionID(payload.Payload.Transaction)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, ReasonInvalidPayload, err.Error())
		return
	}

	// One settlement per transaction at a time, keyed by the canonical tx
	// id so a re-encoded copy of the same transaction cannot slip through.
	if _, busy := m.inFlight.LoadOrStore(txID, struct{}{}); busy {
		m.log.Warn("x402: duplicate settlement", "tx", txID)
		m.settleFailed(w, SettleResponse{ErrorReason: ReasonDuplicateSettlement, Transaction: txID, Network: m.cfg.Network})
		return
	}
	defer m.inFlight.Delete(txID)

	ctx := r.Context()
	verify, err := m.cfg.Facilitator.Verify(ctx, payload, req)
	if err != nil {
		m.log.Error("x402: verify", "tx", txID, "err", err)
		m.paymentRequired(w, resource, req, ReasonUnexpectedVerify)
		return
	}
	if !verify.IsValid {
		m.log.Warn("x402: payment invalid", "tx", txID, "payer", verify.Payer, "reason", verify.InvalidReason)
		m.paymentRequired(w, resource, req, verify.InvalidReason)
		return
	}

	// Run the handler with its response held back until settlement.
	rec := newRecorder()
	paid := Paid{Requirements: req, Payer: verify.Payer, TxID: txID}
	next.ServeHTTP(rec, r.WithContext(context.WithValue(ctx, ctxKey{}, paid)))
	if rec.status >= 400 {
		// The resource failed, so the client is not charged.
		m.log.Warn("x402: not settled, resource failed", "tx", txID, "status", rec.status)
		rec.flush(w, nil)
		return
	}

	settle, err := m.settle(ctx, payload, req)
	if err != nil {
		m.log.Error("x402: settle", "tx", txID, "err", err)
		m.settleFailed(w, SettleResponse{ErrorReason: ReasonUnexpectedSettle, Transaction: txID, Network: m.cfg.Network})
		return
	}
	if !settle.Success {
		m.log.Warn("x402: settlement failed", "tx", txID, "reason", settle.ErrorReason)
		m.settleFailed(w, settle)
		return
	}
	m.log.Info("x402: settled", "tx", settle.Transaction, "payer", verify.Payer, "amount", req.Amount, "pay_to", req.PayTo)
	rec.flush(w, &settle)
}

// settle calls /settle and, as the scheme requires, retries once with the
// same payload when the facilitator reports settlement_pending.
func (m *Middleware) settle(ctx context.Context, p PaymentPayload, req PaymentRequirements) (SettleResponse, error) {
	resp, err := m.cfg.Facilitator.Settle(ctx, p, req)
	if err == nil && !resp.Success && resp.ErrorReason == ReasonSettlementPending {
		return m.cfg.Facilitator.Settle(ctx, p, req)
	}
	return resp, err
}

func (m *Middleware) requirements(p Price) (PaymentRequirements, error) {
	if p.Amount == 0 {
		return PaymentRequirements{}, errors.New("amount must be positive")
	}
	addr, err := apollo.ParseAddress(p.PayTo)
	if err != nil {
		return PaymentRequirements{}, err
	}
	wantNet := uint(common.AddressNetworkTestnet)
	if m.cfg.Network == Mainnet {
		wantNet = common.AddressNetworkMainnet
	}
	if addr.NetworkId() != wantNet {
		return PaymentRequirements{}, fmt.Errorf("payTo %s is not a %s address", p.PayTo, m.cfg.Network)
	}
	asset := p.Asset
	if asset == "" {
		asset = Lovelace
	}
	if asset != Lovelace && !nativeAssetRe.MatchString(asset) {
		return PaymentRequirements{}, fmt.Errorf("asset %q must be %q or policyIdHex.assetNameHex", asset, Lovelace)
	}
	req := PaymentRequirements{
		Scheme:            "exact",
		Network:           m.cfg.Network,
		Amount:            strconv.FormatUint(p.Amount, 10),
		Asset:             asset,
		PayTo:             p.PayTo,
		MaxTimeoutSeconds: m.cfg.MaxTimeoutSeconds,
	}
	if c := m.cfg.Confirmations; c != nil {
		req.Extra = map[string]any{"confirmationPolicy": map[string]any{"l1Confirmations": *c}}
	}
	return req, nil
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

func (m *Middleware) paymentRequired(w http.ResponseWriter, resource ResourceInfo, req PaymentRequirements, reason string) {
	body := PaymentRequired{
		X402Version: Version,
		Error:       reason,
		Resource:    resource,
		Accepts:     []PaymentRequirements{req},
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

func (m *Middleware) settleFailed(w http.ResponseWriter, resp SettleResponse) {
	resp.Success = false
	if resp.Network == "" {
		resp.Network = m.cfg.Network
	}
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

// transactionID returns the canonical Cardano transaction id (hash of the
// body) of a base64-encoded signed transaction.
func transactionID(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("payload.transaction is not base64: %w", err)
	}
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(raw, &tx); err != nil {
		return "", fmt.Errorf("payload.transaction is not a Conway transaction: %w", err)
	}
	return tx.Body.Id().String(), nil
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
