package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Salvionied/apollo/v2/backend/fixed"
	"github.com/blinklabs-io/bursa"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"

	"kiko-backend/geo"
	"kiko-backend/multisig"
	"kiko-backend/x402"
)

// Berlin Hbf -> Alexanderplatz is about 3.1 km in a straight line.
var places = map[string]geo.Point{
	"Berlin Hbf":     {Lat: 52.5251, Lng: 13.3694},
	"Alexanderplatz": {Lat: 52.5219, Lng: 13.4132},
}

type fakeGeocoder struct{}

func (fakeGeocoder) Geocode(_ context.Context, address string) (geo.Point, error) {
	p, ok := places[address]
	if !ok {
		return geo.Point{}, geo.ErrNotFound
	}
	return p, nil
}

type fakeFacilitator struct {
	settle  x402.SettleResponse
	settled int
}

func (f *fakeFacilitator) Verify(context.Context, x402.PaymentPayload, x402.PaymentRequirements) (x402.VerifyResponse, error) {
	return x402.VerifyResponse{IsValid: true, Payer: "addr_test1payer"}, nil
}

func (f *fakeFacilitator) Settle(context.Context, x402.PaymentPayload, x402.PaymentRequirements) (x402.SettleResponse, error) {
	f.settled++
	return f.settle, nil
}

type env struct {
	h           http.Handler
	cosigner    *multisig.Server
	facilitator *fakeFacilitator
	payout      string // a preprod address for drivers
	txB64       string // a real signed transaction
}

func newMnemonic(t *testing.T) string {
	t.Helper()
	m, err := bursa.GenerateMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newSigner(t *testing.T) *multisig.Signer {
	t.Helper()
	s, err := multisig.NewSigner(newMnemonic(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newEnv(t *testing.T) *env {
	t.Helper()
	a, b, c := newSigner(t), newSigner(t), newSigner(t)
	w, err := multisig.NewWallet(multisig.Preprod, a.AccountKey(), b.AccountKey(), c.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	from, _ := w.Address(0)
	to, _ := w.Address(1)
	fc := fixed.NewEmptyFixedChainContext()
	out := babbage.BabbageTransactionOutput{
		OutputAddress: from.Address,
		OutputAmount:  mary.MaryTransactionOutputValue{Amount: 10_000_000},
	}
	fc.AddUtxo(from.Address, common.Utxo{
		Id:     shelley.ShelleyTransactionInput{TxId: common.Blake2b256{1}, OutputIndex: 0},
		Output: &out,
	})
	p, err := w.BuildPayment(context.Background(), fc, multisig.Payment{To: to.Address, Lovelace: 2_000_000})
	if err != nil {
		t.Fatal(err)
	}
	witA, _ := a.Sign(p)
	witB, _ := b.Sign(p)
	signed, err := w.Assemble(p, append(witA, witB...)...)
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeFacilitator{settle: x402.SettleResponse{Success: true, Transaction: "tx", Network: x402.Preprod}}
	payments, err := x402.New(x402.Config{Network: x402.Preprod, Facilitator: f})
	if err != nil {
		t.Fatal(err)
	}
	cosigner, err := multisig.NewServer(multisig.Preprod, newMnemonic(t))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Network: x402.Preprod, Cosigner: cosigner, Geocoder: fakeGeocoder{}, Payments: payments})
	if err != nil {
		t.Fatal(err)
	}
	return &env{h: s.Handler(), cosigner: cosigner, facilitator: f, payout: to.String(), txB64: base64.StdEncoding.EncodeToString(signed)}
}

func (e *env) do(t *testing.T, method, path, driverID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if driverID != "" {
		req.Header.Set(HeaderDriverID, driverID)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) paid(t *testing.T, path string, accepted x402.PaymentRequirements) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(x402.PaymentPayload{
		X402Version: x402.Version,
		Accepted:    accepted,
		Payload:     x402.CardanoPayload{Transaction: e.txB64, Nonce: hex.EncodeToString(make([]byte, 32)) + "#0"},
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(x402.HeaderPaymentSignature, base64.StdEncoding.EncodeToString(raw))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("expected %d, got %d: %s", status, rec.Code, rec.Body.String())
	}
}

func (e *env) registerDriver(t *testing.T) Driver {
	t.Helper()
	return e.registerDriverWithKey(t, newSigner(t).AccountKey())
}

func (e *env) registerDriverWithKey(t *testing.T, cosignerKey string) Driver {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
		"public_key":   e.payout,
		"cosigner_key": cosignerKey,
		"base_fee":     2_000_000,
		"fee_per_km":   500_000,
		"car":          map[string]string{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B-KK 402"},
	})
	expect(t, rec, http.StatusCreated)
	return decode[Driver](t, rec)
}

func (e *env) createRide(t *testing.T) rideView {
	t.Helper()
	return e.createRideWithKey(t, "")
}

func (e *env) createRideWithKey(t *testing.T, cosignerKey string) rideView {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/ride", "", map[string]string{
		"start_address": "Berlin Hbf",
		"end_address":   "Alexanderplatz",
		"pickup_time":   "2026-11-01T09:00:00Z",
		"cosigner_key":  cosignerKey,
	})
	expect(t, rec, http.StatusCreated)
	return decode[rideView](t, rec)
}

func TestRideLifecycle(t *testing.T) {
	e := newEnv(t)
	d := e.registerDriver(t)
	ride := e.createRide(t)
	if ride.Status != StatusRequested || ride.DistanceKm < 2.9 || ride.DistanceKm > 3.3 {
		t.Fatalf("unexpected ride: %+v", ride)
	}

	// Free while no driver has accepted.
	expect(t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil), http.StatusOK)

	avail := e.do(t, http.MethodGet, "/api/v1/driver/drive/available?location=52.52,13.37&range=2", "", nil)
	expect(t, avail, http.StatusOK)
	if got := decode[[]availableRide](t, avail); len(got) != 1 || got[0].ID != ride.ID {
		t.Fatalf("expected the ride to be available, got %+v", got)
	}
	far := e.do(t, http.MethodGet, "/api/v1/driver/drive/available?location=48.137,11.575&range=6", "", nil)
	if got := decode[[]availableRide](t, far); len(got) != 0 {
		t.Fatalf("expected no rides near Munich, got %+v", got)
	}

	acc := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.ID, map[string]any{"eta_minutes": 4, "eta_km": 1.2})
	expect(t, acc, http.StatusOK)
	accepted := decode[rideView](t, acc)
	wantFare := fare(d, accepted.DistanceKm)
	if accepted.Status != StatusAccepted || accepted.FareLovelace != wantFare || accepted.PayTo == "" {
		t.Fatalf("unexpected accepted ride: %+v", accepted)
	}
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.ID, map[string]any{}), http.StatusConflict)

	// Accepted: the rider must pay the fare to the ride's multisig address.
	unpaid := e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)
	expect(t, unpaid, http.StatusPaymentRequired)
	pr := decode[x402.PaymentRequired](t, unpaid)
	req := pr.Accepts[0]
	if req.PayTo != accepted.PayTo || req.Amount != strconv.FormatUint(wantFare, 10) {
		t.Fatalf("unexpected requirements: %+v", req)
	}

	paid := e.paid(t, "/api/v1/ride/"+ride.ID, req)
	expect(t, paid, http.StatusOK)
	view := decode[rideView](t, paid)
	if view.Status != StatusPaid || view.Driver == nil || view.Driver.Car.LicensePlateNumber != "B-KK 402" || view.PaymentTx == "" {
		t.Fatalf("unexpected paid view: %+v", view)
	}

	// Later polls are free.
	again := e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)
	expect(t, again, http.StatusOK)
	if v := decode[rideView](t, again); v.Status != StatusPaid || v.Driver == nil {
		t.Fatalf("unexpected view after payment: %+v", v)
	}
	if e.facilitator.settled != 1 {
		t.Fatalf("expected one settlement, got %d", e.facilitator.settled)
	}

	drv := e.do(t, http.MethodGet, "/api/v1/driver/drive/"+ride.ID, d.ID, nil)
	expect(t, drv, http.StatusOK)
	if v := decode[rideView](t, drv); v.Status != StatusPaid {
		t.Fatalf("driver should see the ride paid: %+v", v)
	}
}

func TestFailedSettlementKeepsRideUnpaid(t *testing.T) {
	e := newEnv(t)
	e.facilitator.settle = x402.SettleResponse{Success: false, ErrorReason: "exact_cardano_settlement_failed"}
	d := e.registerDriver(t)
	ride := e.createRide(t)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.ID, map[string]any{"eta_minutes": 4}), http.StatusOK)

	req := decode[x402.PaymentRequired](t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)).Accepts[0]
	expect(t, e.paid(t, "/api/v1/ride/"+ride.ID, req), http.StatusPaymentRequired)
	// Still unpaid, and the claim was released so the rider can pay again.
	expect(t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil), http.StatusPaymentRequired)
}

func TestClaimRefusesSecondTransaction(t *testing.T) {
	s := newStore(0)
	r := s.addRide(Ride{DistanceKm: 1})
	s.accept(r.ID, Driver{ID: "d"}, "addr", 1, 1)
	if _, ok := s.claim(r.ID, "tx1"); !ok {
		t.Fatal("first claim refused")
	}
	if _, ok := s.claim(r.ID, "tx1"); !ok {
		t.Fatal("same transaction must be able to retry")
	}
	if _, ok := s.claim(r.ID, "tx2"); ok {
		t.Fatal("second transaction must be refused")
	}
	s.finishClaim(r.ID, "tx1", true)
	if got, _ := s.ride(r.ID); got.Status != StatusPaid || got.PaymentTx != "tx1" {
		t.Fatalf("unexpected ride: %+v", got)
	}
}

func TestDriverValidationAndAuth(t *testing.T) {
	e := newEnv(t)
	bad := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
		"public_key": "addr1notanaddress",
		"car":        map[string]string{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B"},
	})
	expect(t, bad, http.StatusBadRequest)

	ride := e.createRide(t)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, "", map[string]any{}), http.StatusUnauthorized)

	d := e.registerDriver(t)
	other := e.registerDriver(t)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.ID, map[string]any{}), http.StatusOK)
	expect(t, e.do(t, http.MethodGet, "/api/v1/driver/drive/"+ride.ID, other.ID, nil), http.StatusNotFound)
}

// rideAddress is what the rider and driver can derive on their own for an
// accepted ride.
func (e *env) rideAddress(t *testing.T, driverKey, riderKey string, index uint32) string {
	t.Helper()
	w, _, err := e.cosigner.RideWallet(driverKey, riderKey)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := w.Address(index)
	if err != nil {
		t.Fatal(err)
	}
	return addr.String()
}

func TestRideAddressCosigners(t *testing.T) {
	e := newEnv(t)
	d := e.registerDriver(t)
	rider := newSigner(t)

	custodial := e.createRide(t)
	withKey := e.createRideWithKey(t, rider.AccountKey())
	if !custodial.Custodial || withKey.Custodial {
		t.Fatalf("custodial flags wrong: %v, %v", custodial.Custodial, withKey.Custodial)
	}
	if custodial.PayTo != "" || withKey.PayTo != "" {
		t.Fatal("pay_to must wait for a driver")
	}

	accept := func(id string) rideView {
		rec := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+id, d.ID, map[string]any{"eta_minutes": 3})
		expect(t, rec, http.StatusOK)
		return decode[rideView](t, rec)
	}
	// Rides were created first and second, so they hold indexes 0 and 1.
	if got, want := accept(custodial.ID).PayTo, e.rideAddress(t, d.CosignerKey, "", 0); got != want {
		t.Fatalf("custodial ride pays to %s, want server+driver+backup %s", got, want)
	}
	if got, want := accept(withKey.ID).PayTo, e.rideAddress(t, d.CosignerKey, rider.AccountKey(), 1); got != want {
		t.Fatalf("ride pays to %s, want server+driver+rider %s", got, want)
	}
}

func TestCosignerKeyValidation(t *testing.T) {
	e := newEnv(t)
	car := map[string]string{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B"}
	for _, key := range []string{"", "acct_shared_xvk1invalid", e.cosigner.AccountKey()} {
		rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{"public_key": e.payout, "cosigner_key": key, "car": car})
		expect(t, rec, http.StatusBadRequest)
	}
	rec := e.do(t, http.MethodPost, "/api/v1/ride", "", map[string]string{
		"start_address": "Berlin Hbf",
		"end_address":   "Alexanderplatz",
		"pickup_time":   "2026-11-01T09:00:00Z",
		"cosigner_key":  e.cosigner.AccountKey(),
	})
	expect(t, rec, http.StatusBadRequest)

	// A driver cannot take a ride whose rider uses the driver's own key.
	shared := newSigner(t).AccountKey()
	d := e.registerDriverWithKey(t, shared)
	ride := e.createRideWithKey(t, shared)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.ID, map[string]any{}), http.StatusUnprocessableEntity)
}

func TestQuoteAndUnknownAddress(t *testing.T) {
	e := newEnv(t)
	d := e.registerDriver(t)
	rec := e.do(t, http.MethodGet, "/api/v1/ride?start_address=Berlin+Hbf&end_address=Alexanderplatz", "", nil)
	expect(t, rec, http.StatusOK)
	q := decode[quote](t, rec)
	if f := fare(d, q.DistanceKm); q.FareLovelace == nil || q.FareLovelace.Min != f || q.FareLovelace.Max != f {
		t.Fatalf("unexpected quote: %+v", q)
	}
	expect(t, e.do(t, http.MethodGet, "/api/v1/ride?start_address=Nowhere&end_address=Alexanderplatz", "", nil), http.StatusUnprocessableEntity)
}

func TestFareFloor(t *testing.T) {
	if got := fare(Driver{BaseFee: 100, FeePerKm: 10}, 1); got != MinFareLovelace {
		t.Fatalf("expected floor, got %d", got)
	}
	if got := fare(Driver{BaseFee: 2_000_000, FeePerKm: 500_000}, 2.5); got != 3_250_000 {
		t.Fatalf("got %d", got)
	}
}
