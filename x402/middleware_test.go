package x402

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Salvionied/apollo/v2/backend/fixed"
	"github.com/blinklabs-io/bursa"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"

	"kiko-backend/multisig"
)

// fakeFacilitator records calls and answers from canned responses.
type fakeFacilitator struct {
	verify   VerifyResponse
	settles  []SettleResponse
	verified int
	settled  int
	lastReq  PaymentRequirements
}

func (f *fakeFacilitator) Verify(_ context.Context, _ PaymentPayload, req PaymentRequirements) (VerifyResponse, error) {
	f.verified++
	f.lastReq = req
	return f.verify, nil
}

func (f *fakeFacilitator) Settle(_ context.Context, _ PaymentPayload, _ PaymentRequirements) (SettleResponse, error) {
	resp := f.settles[min(f.settled, len(f.settles)-1)]
	f.settled++
	return resp, nil
}

func okFacilitator() *fakeFacilitator {
	return &fakeFacilitator{
		verify:  VerifyResponse{IsValid: true, Payer: "addr_test1payer"},
		settles: []SettleResponse{{Success: true, Transaction: "abc", Network: Preprod}},
	}
}

// fixture builds two preprod addresses and a real signed transaction.
type fixture struct {
	addrA, addrB string
	addrMainnet  string
	txB64        string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	signer := func() *multisig.Signer {
		m, err := bursa.GenerateMnemonic()
		if err != nil {
			t.Fatal(err)
		}
		s, err := multisig.NewSigner(m, 0)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b, c := signer(), signer(), signer()
	w, err := multisig.NewWallet(multisig.Preprod, a.AccountKey(), b.AccountKey(), c.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	addr0, _ := w.Address(0)
	addr1, _ := w.Address(1)
	mw, err := multisig.NewWallet(multisig.Mainnet, a.AccountKey(), b.AccountKey(), c.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	mainnet, _ := mw.Address(0)

	fc := fixed.NewEmptyFixedChainContext()
	out := babbage.BabbageTransactionOutput{
		OutputAddress: addr0.Address,
		OutputAmount:  mary.MaryTransactionOutputValue{Amount: 10_000_000},
	}
	fc.AddUtxo(addr0.Address, common.Utxo{
		Id:     shelley.ShelleyTransactionInput{TxId: common.Blake2b256{1}, OutputIndex: 0},
		Output: &out,
	})
	p, err := w.BuildPayment(context.Background(), fc, multisig.Payment{To: addr1.Address, Lovelace: 2_000_000})
	if err != nil {
		t.Fatal(err)
	}
	witA, _ := a.Sign(p)
	witB, _ := b.Sign(p)
	signed, err := w.Assemble(p, append(witA, witB...)...)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		addrA:       addr0.String(),
		addrB:       addr1.String(),
		addrMainnet: mainnet.String(),
		txB64:       base64.StdEncoding.EncodeToString(signed),
	}
}

func newMiddleware(t *testing.T, f Facilitator) *Middleware {
	t.Helper()
	m, err := New(Config{Networks: []NetworkConfig{{Network: Preprod, Facilitator: f}}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	paid, ok := PaymentFromContext(r.Context())
	if !ok {
		http.Error(w, "no payment in context", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Payer", paid.Payer)
	w.Write([]byte("premium data"))
})

func decodeRequired(t *testing.T, rec *httptest.ResponseRecorder) PaymentRequired {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(rec.Header().Get(HeaderPaymentRequired))
	if err != nil {
		t.Fatal(err)
	}
	var pr PaymentRequired
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

func paidRequest(t *testing.T, path string, accepted PaymentRequirements, txB64 string) *http.Request {
	t.Helper()
	raw, _ := json.Marshal(PaymentPayload{
		X402Version: Version,
		Accepted:    accepted,
		Payload:     TransactionPayload{Transaction: txB64, Nonce: hex.EncodeToString(make([]byte, 32)) + "#0"},
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(HeaderPaymentSignature, base64.StdEncoding.EncodeToString(raw))
	return req
}

// quote performs the unpaid request and returns the offered requirements.
func quote(t *testing.T, h http.Handler, path string) PaymentRequirements {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d", rec.Code)
	}
	return decodeRequired(t, rec).Accepts[0]
}

func TestPerRequestPrice(t *testing.T) {
	fx := newFixture(t)
	m := newMiddleware(t, okFacilitator())
	h := m.Require(func(r *http.Request) ([]Price, error) {
		if strings.HasPrefix(r.URL.Path, "/gold") {
			return []Price{{PayTo: fx.addrB, Amount: 5_000_000, Description: "gold"}}, nil
		}
		return []Price{{PayTo: fx.addrA, Amount: 1_500_000}}, nil
	})(okHandler)

	basic := quote(t, h, "/basic")
	gold := quote(t, h, "/gold")
	if basic.PayTo != fx.addrA || basic.Amount != "1500000" || basic.Asset != Lovelace {
		t.Fatalf("basic quote wrong: %+v", basic)
	}
	if gold.PayTo != fx.addrB || gold.Amount != "5000000" {
		t.Fatalf("gold quote wrong: %+v", gold)
	}
	if basic.Network != Preprod || basic.Scheme != "exact" || basic.MaxTimeoutSeconds != DefaultMaxTimeoutSeconds {
		t.Fatalf("unexpected requirement fields: %+v", basic)
	}
}

func TestPaidRequestSettlesAndReleasesResponse(t *testing.T) {
	fx := newFixture(t)
	f := okFacilitator()
	h := newMiddleware(t, f).Require(Fixed(fx.addrA, 2_000_000))(okHandler)

	accepted := quote(t, h, "/data")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepted, fx.txB64))

	if rec.Code != http.StatusOK || rec.Body.String() != "premium data" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Payer") != "addr_test1payer" {
		t.Fatal("handler did not see the verified payer")
	}
	raw, _ := base64.StdEncoding.DecodeString(rec.Header().Get(HeaderPaymentResponse))
	var settle SettleResponse
	if err := json.Unmarshal(raw, &settle); err != nil || !settle.Success {
		t.Fatalf("missing successful PAYMENT-RESPONSE: %s", raw)
	}
	if f.verified != 1 || f.settled != 1 {
		t.Fatalf("verify %d settle %d", f.verified, f.settled)
	}
	if f.lastReq.PayTo != fx.addrA {
		t.Fatal("facilitator got the wrong requirements")
	}
}

func TestTamperedRequirementsRejected(t *testing.T) {
	fx := newFixture(t)
	f := okFacilitator()
	h := newMiddleware(t, f).Require(Fixed(fx.addrA, 2_000_000))(okHandler)

	accepted := quote(t, h, "/data")
	accepted.Amount = "1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepted, fx.txB64))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d", rec.Code)
	}
	if decodeRequired(t, rec).Error != ReasonRequirementsChanged {
		t.Fatal("wrong error reason")
	}
	if f.verified != 0 {
		t.Fatal("facilitator was called for tampered requirements")
	}
}

func TestInvalidPaymentDoesNotRunHandler(t *testing.T) {
	fx := newFixture(t)
	f := okFacilitator()
	f.verify = VerifyResponse{IsValid: false, InvalidReason: "insufficient_funds"}
	ran := false
	h := newMiddleware(t, f).Require(Fixed(fx.addrA, 2_000_000))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))

	accepted := quote(t, h, "/data")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepted, fx.txB64))

	if rec.Code != http.StatusPaymentRequired || ran {
		t.Fatalf("got %d, handler ran: %v", rec.Code, ran)
	}
	if decodeRequired(t, rec).Error != "insufficient_funds" {
		t.Fatal("reason not forwarded")
	}
}

func TestHandlerErrorIsNotCharged(t *testing.T) {
	fx := newFixture(t)
	f := okFacilitator()
	h := newMiddleware(t, f).Require(Fixed(fx.addrA, 2_000_000))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))

	accepted := quote(t, h, "/data")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepted, fx.txB64))

	if rec.Code != http.StatusInternalServerError || f.settled != 0 {
		t.Fatalf("got %d, settled %d", rec.Code, f.settled)
	}
}

func TestSettlementPendingRetriesOnce(t *testing.T) {
	fx := newFixture(t)
	f := okFacilitator()
	f.settles = []SettleResponse{
		{Success: false, ErrorReason: ReasonSettlementPending, Transaction: "abc", Network: Preprod},
		{Success: true, Transaction: "abc", Network: Preprod},
	}
	h := newMiddleware(t, f).Require(Fixed(fx.addrA, 2_000_000))(okHandler)

	accepted := quote(t, h, "/data")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepted, fx.txB64))

	if rec.Code != http.StatusOK || f.settled != 2 {
		t.Fatalf("got %d after %d settles", rec.Code, f.settled)
	}
}

func TestSettlementFailureWithholdsResponse(t *testing.T) {
	fx := newFixture(t)
	f := okFacilitator()
	f.settles = []SettleResponse{{Success: false, ErrorReason: "invalid_transaction_state", Network: Preprod}}
	h := newMiddleware(t, f).Require(Fixed(fx.addrA, 2_000_000))(okHandler)

	accepted := quote(t, h, "/data")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepted, fx.txB64))

	if rec.Code != http.StatusPaymentRequired || strings.Contains(rec.Body.String(), "premium") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(HeaderPaymentResponse) == "" {
		t.Fatal("missing PAYMENT-RESPONSE on failure")
	}
}

func TestRejectsWrongNetworkAddress(t *testing.T) {
	fx := newFixture(t)
	m := newMiddleware(t, okFacilitator())
	if _, err := m.requirements(Price{PayTo: fx.addrMainnet, Amount: 1}); err == nil ||
		!strings.Contains(err.Error(), "is not a cardano:preprod address") {
		t.Fatalf("expected network mismatch error, got %v", err)
	}
	h := m.Require(Fixed(fx.addrMainnet, 2_000_000))(okHandler)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/data", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a mainnet payTo on preprod, got %d", rec.Code)
	}
}

func TestMalformedHeader(t *testing.T) {
	fx := newFixture(t)
	h := newMiddleware(t, okFacilitator()).Require(Fixed(fx.addrA, 2_000_000))(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	req.Header.Set(HeaderPaymentSignature, "not base64!!")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHTTPFacilitator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body facilitatorRequest
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/verify":
			json.NewEncoder(w).Encode(VerifyResponse{IsValid: body.PaymentRequirements.Amount == "7"})
		case "/settle":
			json.NewEncoder(w).Encode(SettleResponse{Success: true, Transaction: "tx", Network: body.PaymentRequirements.Network})
		}
	}))
	defer srv.Close()

	f := &HTTPFacilitator{URL: srv.URL + "/", Header: http.Header{"X-Api-Key": {"secret"}}}
	req := PaymentRequirements{Amount: "7", Network: Preprod}
	v, err := f.Verify(context.Background(), PaymentPayload{}, req)
	if err != nil || !v.IsValid {
		t.Fatalf("verify: %+v %v", v, err)
	}
	s, err := f.Settle(context.Background(), PaymentPayload{}, req)
	if err != nil || !s.Success || s.Network != Preprod {
		t.Fatalf("settle: %+v %v", s, err)
	}
}
