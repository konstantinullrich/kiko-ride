package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apollo "github.com/Salvionied/apollo/v2"
	"github.com/Salvionied/apollo/v2/backend/fixed"
	"github.com/blinklabs-io/bursa"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"

	"kiko-backend/bsvmultisig"
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

// Search matches the known places containing query.
func (fakeGeocoder) Search(_ context.Context, query string, limit int) ([]geo.Place, error) {
	out := []geo.Place{}
	for addr, p := range places {
		if strings.Contains(addr, query) && len(out) < limit {
			out = append(out, geo.Place{Address: addr, Point: p})
		}
	}
	return out, nil
}

type fakeFacilitator struct {
	settle  x402.SettleResponse
	err     error
	settled int
}

func (f *fakeFacilitator) Verify(context.Context, x402.PaymentPayload, x402.PaymentRequirements) (x402.VerifyResponse, error) {
	return x402.VerifyResponse{IsValid: true, Payer: "addr_test1payer"}, nil
}

func (f *fakeFacilitator) Settle(context.Context, x402.PaymentPayload, x402.PaymentRequirements) (x402.SettleResponse, error) {
	f.settled++
	return f.settle, f.err
}

// fakeChain records submitted transactions, failing while reject is set.
type fakeChain struct {
	*fixed.FixedChainContext
	reject    error
	submitted [][]byte
}

func (c *fakeChain) SubmitTx(tx []byte) (common.Blake2b256, error) {
	if c.reject != nil {
		return common.Blake2b256{}, c.reject
	}
	c.submitted = append(c.submitted, tx)
	var decoded conway.ConwayTransaction
	if _, err := cbor.DecodeExact(tx, &decoded); err != nil {
		return common.Blake2b256{}, err
	}
	return decoded.Body.Id(), nil
}

type env struct {
	s           *Server
	bsv         *bsvmultisig.Server
	broadcaster *fakeBroadcaster
	h           http.Handler
	db          *sql.DB
	chain       *fakeChain
	cosigner    *multisig.Server
	facilitator *fakeFacilitator
	txB64       string // a real signed transaction
}

// newDB opens a fresh database file, closed when the test ends.
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "kiko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newMnemonic(t *testing.T) string {
	t.Helper()
	m, err := bursa.GenerateMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// signers holds a message signing function for every test signer by
// account key, so requests for a driver can be signed with their key.
var signers sync.Map // key -> func([]byte) []byte

func newSigner(t *testing.T) *multisig.Signer {
	t.Helper()
	s, err := multisig.NewSigner(newMnemonic(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	signers.Store(s.AccountKey(), s.SignMessage)
	return s
}

// signRequest signs req's method with target and body as the driver with
// key at time at, if key belongs to a test signer.
func signRequest(req *http.Request, key, target string, body []byte, at time.Time) {
	sign, ok := signers.Load(normalizeKey(key))
	if !ok {
		return
	}
	ts := at.Unix()
	sig := sign.(func([]byte) []byte)(DriverMessage(ts, req.Method, target, body))
	req.Header.Set(HeaderDriverTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderDriverSignature, hex.EncodeToString(sig))
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith settles BSV payments with bsvPayments, or with the fake
// facilitator when it is nil; its broadcaster is the env's.
func newEnvWith(t *testing.T, bsvPayments func(transaction.Broadcaster) x402.Facilitator) *env {
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
	broadcast := &fakeBroadcaster{}
	var bsvFacilitator x402.Facilitator = f
	if bsvPayments != nil {
		bsvFacilitator = bsvPayments(broadcast)
	}
	payments, err := x402.New(x402.Config{Networks: []x402.NetworkConfig{
		{Network: x402.Preprod, Facilitator: f},
		{Network: x402.BSVTestnet, Facilitator: bsvFacilitator},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cosigner, err := multisig.NewServer(multisig.Preprod, newMnemonic(t))
	if err != nil {
		t.Fatal(err)
	}
	chain := &fakeChain{FixedChainContext: fixed.NewEmptyFixedChainContext()}
	db := newDB(t)
	bsvCosigner, err := bsvmultisig.NewServer(bsvmultisig.Testnet, newMnemonic(t))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Network: x402.Preprod, Cosigner: cosigner, DB: db, Geocoder: fakeGeocoder{}, Payments: payments, Chain: chain,
		BSV: &BSVConfig{Network: x402.BSVTestnet, Cosigner: bsvCosigner, Broadcaster: broadcast}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{s: s, bsv: bsvCosigner, broadcaster: broadcast, h: s.Handler(), db: db, chain: chain, cosigner: cosigner, facilitator: f, txB64: base64.StdEncoding.EncodeToString(signed)}
}

// do sends a request, signed as the driver with driverKey or, for
// registration, as the public_key in the body.
func (e *env) do(t *testing.T, method, path, driverKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	raw := bytes.Clone(buf.Bytes())
	req := httptest.NewRequest(method, path, &buf)
	signer := driverKey
	if driverKey != "" {
		req.Header.Set(HeaderDriverKey, driverKey)
	} else if m, ok := body.(map[string]any); ok {
		signer, _ = m["public_key"].(string)
	}
	signRequest(req, signer, path, raw, time.Now())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) paid(t *testing.T, path string, accepted x402.PaymentRequirements) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(x402.PaymentPayload{
		X402Version: x402.Version,
		Accepted:    accepted,
		Payload:     x402.TransactionPayload{Transaction: e.txB64, Nonce: hex.EncodeToString(make([]byte, 32)) + "#0"},
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

func (e *env) registerDriverWithKey(t *testing.T, publicKey string) Driver {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
		"public_key": publicKey,
		"base_fee":   2_000_000,
		"fee_per_km": 500_000,
		"car":        map[string]any{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B-KK 402", "year": 2021},
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
		"pickup_time":   time.Now().Add(-time.Hour).Format(time.RFC3339),
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

	avail := e.do(t, http.MethodGet, "/api/v1/driver/drive/available?location=52.52,13.37&range=2", d.PublicKey, nil)
	expect(t, avail, http.StatusOK)
	if got := decode[[]availableRide](t, avail); len(got) != 1 || got[0].ID != ride.ID {
		t.Fatalf("expected the ride to be available, got %+v", got)
	}
	far := e.do(t, http.MethodGet, "/api/v1/driver/drive/available?location=48.137,11.575&range=6", d.PublicKey, nil)
	if got := decode[[]availableRide](t, far); len(got) != 0 {
		t.Fatalf("expected no rides near Munich, got %+v", got)
	}

	acc := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{"eta_minutes": 4, "eta_km": 1.2})
	expect(t, acc, http.StatusOK)
	accepted := decode[rideView](t, acc)
	wantFare := fare(d, accepted.DistanceKm)
	if accepted.Status != StatusAccepted || accepted.FareLovelace != wantFare || accepted.PayTo == "" {
		t.Fatalf("unexpected accepted ride: %+v", accepted)
	}
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{}), http.StatusConflict)

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
	if view.Status != StatusPaid || view.Car == nil || view.Car.LicensePlateNumber != "B-KK 402" || view.Car.Year != 2021 || view.PaymentTx == "" {
		t.Fatalf("unexpected paid view: %+v", view)
	}

	// Later polls are free.
	again := e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)
	expect(t, again, http.StatusOK)
	if v := decode[rideView](t, again); v.Status != StatusPaid || v.Car == nil {
		t.Fatalf("unexpected view after payment: %+v", v)
	}
	if e.facilitator.settled != 1 {
		t.Fatalf("expected one settlement, got %d", e.facilitator.settled)
	}

	drv := e.do(t, http.MethodGet, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, nil)
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
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{"eta_minutes": 4}), http.StatusOK)

	req := decode[x402.PaymentRequired](t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)).Accepts[0]
	expect(t, e.paid(t, "/api/v1/ride/"+ride.ID, req), http.StatusPaymentRequired)
	// Still unpaid, and the claim was released so the rider can pay again.
	expect(t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil), http.StatusPaymentRequired)
	if r, _ := e.s.store.ride(ride.ID); r.Status != StatusAccepted {
		t.Fatalf("unexpected ride: %+v", r)
	}
	if _, err := e.s.store.claim(ride.ID, "other"); err != nil {
		t.Fatal("a failed payment must free the ride:", err)
	}
}

func TestUnresolvedSettlementKeepsClaim(t *testing.T) {
	for _, reason := range []string{x402.ReasonSettlementPending, ""} {
		e := newEnv(t)
		e.facilitator.settle = x402.SettleResponse{ErrorReason: reason, Transaction: "tx"}
		if reason == "" {
			e.facilitator.err = errors.New("504 Gateway Time-out")
		}
		d := e.registerDriver(t)
		ride := e.createRide(t)
		expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{"eta_minutes": 4}), http.StatusOK)
		req := decode[x402.PaymentRequired](t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)).Accepts[0]
		expect(t, e.paid(t, "/api/v1/ride/"+ride.ID, req), http.StatusPaymentRequired)

		// The transaction may still land, so no other one can pay the ride.
		if _, err := e.s.store.claim(ride.ID, "other"); !errors.Is(err, errConflict) {
			t.Fatalf("%q: another transaction claimed the ride: %v", reason, err)
		}
		// Retrying the same payment settles it.
		e.facilitator.settle, e.facilitator.err = x402.SettleResponse{Success: true, Transaction: "tx"}, nil
		expect(t, e.paid(t, "/api/v1/ride/"+ride.ID, req), http.StatusOK)
		if r, _ := e.s.store.ride(ride.ID); r.Status != StatusPaid {
			t.Fatalf("%q: retry left the ride %s", reason, r.Status)
		}
	}
}

func TestClaimRefusesSecondTransaction(t *testing.T) {
	s, err := newStore(newDB(t))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.addRide(Ride{DistanceKm: 1, PickupTime: time.Now(), CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.accept(r.ID, Driver{PublicKey: "d"}, "addr", "script", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(r.ID, "tx1"); err != nil {
		t.Fatal("first claim refused:", err)
	}
	if _, err := s.claim(r.ID, "tx1"); err != nil {
		t.Fatal("same transaction must be able to retry:", err)
	}
	if _, err := s.claim(r.ID, "tx2"); !errors.Is(err, errConflict) {
		t.Fatal("second transaction must be refused, got", err)
	}
	// A claim left unresolved for longer than any payment can land in
	// gives way.
	if _, err := s.db.Exec(`UPDATE rides SET claimed_at = ? WHERE id = ?`, time.Now().Add(-claimHold-time.Minute).Unix(), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(r.ID, "tx2"); err != nil {
		t.Fatal("stale claim must give way:", err)
	}
	if _, err := s.claim(r.ID, "tx1"); !errors.Is(err, errConflict) {
		t.Fatal("the new claim must hold, got", err)
	}
	if err := s.finishClaim(r.ID, "tx2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claim(r.ID, "tx1"); err != nil {
		t.Fatal("a released ride must be claimable:", err)
	}
	if err := s.finishClaim(r.ID, "tx1", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ride(r.ID); got.Status != StatusPaid || got.PaymentTx != "tx1" {
		t.Fatalf("unexpected ride: %+v", got)
	}
}

// TestStorePersists reopens the database: drivers and rides survive, address
// indexes are never handed out twice, and an interrupted payout is freed.
func TestStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kiko.db")
	open := func() (*sql.DB, *store) {
		db, err := OpenDB(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := newStore(db)
		if err != nil {
			t.Fatal(err)
		}
		return db, s
	}

	db, s := open()
	d := Driver{PublicKey: "d", Chain: ChainCardano, BaseFee: 2_000_000, FeePerKm: 500_000, Car: Car{Brand: "VW", Model: "ID.3", Color: "white", LicensePlateNumber: "B", Year: 2021}}
	if _, err := s.putDriver(d); err != nil {
		t.Fatal(err)
	}
	pickup := time.Date(2026, 11, 1, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	first, err := s.addRide(Ride{StartAddress: "a", EndAddress: "b", Start: places["Berlin Hbf"], End: places["Alexanderplatz"], DistanceKm: 3, PickupTime: pickup, CreatedAt: time.Now(), RiderKey: "r"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.addRide(Ride{DistanceKm: 1, PickupTime: pickup, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if second.AddressIndex != first.AddressIndex+1 {
		t.Fatalf("indexes %d, %d", first.AddressIndex, second.AddressIndex)
	}
	if _, err := s.accept(first.ID, d, "addr", "script", 4, 1.2); err != nil {
		t.Fatal(err)
	}
	s.claim(first.ID, "pay")
	s.finishClaim(first.ID, "pay", true)
	if err := s.beginPayout(first.ID, "d"); err != nil {
		t.Fatal(err)
	}
	// The second ride is deleted: its index must still not come back.
	if _, err := db.Exec(`DELETE FROM rides WHERE id = ?`, second.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, s = open()
	defer db.Close()
	if got, err := s.driver("d"); err != nil || got != d {
		t.Fatalf("driver %+v, %v", got, err)
	}
	got, err := s.ride(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPaid || got.PaymentTx != "pay" || got.Fare != fare(d, 3) || got.Chain != ChainCardano || got.RiderKey != "r" ||
		got.Start != places["Berlin Hbf"] || !got.PickupTime.Equal(pickup) || got.PickupTime.Format(timeFormat) != pickup.Format(timeFormat) {
		t.Fatalf("unexpected ride: %+v", got)
	}
	if err := s.beginPayout(first.ID, "d"); err != nil {
		t.Fatal("interrupted payout must be freed on restart:", err)
	}
	third, err := s.addRide(Ride{DistanceKm: 1, PickupTime: pickup, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if third.AddressIndex <= second.AddressIndex {
		t.Fatalf("index %d reused after %d", third.AddressIndex, second.AddressIndex)
	}
}

func TestDriverValidationAndAuth(t *testing.T) {
	e := newEnv(t)
	bad := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
		"public_key": "addr1notanaddress",
		"car":        map[string]any{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B", "year": 2021},
	})
	expect(t, bad, http.StatusBadRequest)
	for _, year := range []int{0, MinCarYear - 1, time.Now().Year() + 2} {
		rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
			"public_key": newSigner(t).AccountKey(),
			"car":        map[string]any{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B", "year": year},
		})
		expect(t, rec, http.StatusBadRequest)
	}

	ride := e.createRide(t)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, "", map[string]any{}), http.StatusUnauthorized)

	d := e.registerDriver(t)
	other := e.registerDriver(t)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{}), http.StatusOK)
	expect(t, e.do(t, http.MethodGet, "/api/v1/driver/drive/"+ride.ID, other.PublicKey, nil), http.StatusNotFound)
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
	riderKey, _ := rider.Key(5)
	withKey := e.createRideWithKey(t, riderKey)
	if !custodial.Custodial || withKey.Custodial {
		t.Fatalf("custodial flags wrong: %v, %v", custodial.Custodial, withKey.Custodial)
	}
	if custodial.PayTo != "" || withKey.PayTo != "" {
		t.Fatal("pay_to must wait for a driver")
	}

	accept := func(id string) rideView {
		rec := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+id, d.PublicKey, map[string]any{"eta_minutes": 3})
		expect(t, rec, http.StatusOK)
		return decode[rideView](t, rec)
	}
	// Rides were created first and second, so they hold indexes 1 and 2.
	got := accept(custodial.ID)
	if got.AddressIndex == nil || *got.AddressIndex != 1 {
		t.Fatalf("custodial ride has index %v, want 1", got.AddressIndex)
	}
	if want := e.rideAddress(t, d.PublicKey, "", 1); got.PayTo != want {
		t.Fatalf("custodial ride pays to %s, want server+driver+backup %s", got.PayTo, want)
	}
	got = accept(withKey.ID)
	if got.AddressIndex == nil || *got.AddressIndex != 2 {
		t.Fatalf("ride has index %v, want 2", got.AddressIndex)
	}
	if want := e.rideAddress(t, d.PublicKey, riderKey, 2); got.PayTo != want {
		t.Fatalf("ride pays to %s, want server+driver+rider %s", got.PayTo, want)
	}
}

func TestCosignerKeyValidation(t *testing.T) {
	e := newEnv(t)
	car := map[string]any{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B", "year": 2021}
	for _, key := range []string{"", "acct_shared_xvk1invalid", "addr_test1vz2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzerspjrlsz", e.cosigner.AccountKey()} {
		rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{"public_key": key, "car": car})
		expect(t, rec, http.StatusBadRequest)
	}
	// Riders give a key for the ride, never an account key.
	for _, key := range []string{e.cosigner.AccountKey(), newSigner(t).AccountKey(), "addr_shared_vk1qqqq"} {
		rec := e.do(t, http.MethodPost, "/api/v1/ride", "", map[string]string{
			"start_address": "Berlin Hbf",
			"end_address":   "Alexanderplatz",
			"pickup_time":   "2026-11-01T09:00:00Z",
			"cosigner_key":  key,
		})
		expect(t, rec, http.StatusBadRequest)
	}

	// A driver cannot take a ride whose rider uses the driver's own key at
	// the ride's index, the first one.
	driver := newSigner(t)
	d := e.registerDriverWithKey(t, driver.AccountKey())
	shared, _ := driver.Key(1)
	ride := e.createRideWithKey(t, shared)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{}), http.StatusUnprocessableEntity)
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

func TestReregisterReplacesDriver(t *testing.T) {
	e := newEnv(t)
	d := e.registerDriver(t)
	ride := e.createRide(t)
	acc := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{})
	expect(t, acc, http.StatusOK)
	oldFare := decode[rideView](t, acc).FareLovelace

	// The key is matched case-insensitively, as everywhere else.
	rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
		"public_key": strings.ToUpper(d.PublicKey),
		"base_fee":   5_000_000,
		"fee_per_km": 1_000_000,
		"car":        map[string]any{"brand": "Tesla", "model": "3", "color": "red", "license_plate_number": "B-KK 403", "year": 2024},
	})
	expect(t, rec, http.StatusOK)
	updated := decode[Driver](t, rec)
	if updated.PublicKey != d.PublicKey || updated.BaseFee != 5_000_000 || updated.FeePerKm != 1_000_000 || updated.Car.Brand != "Tesla" {
		t.Fatalf("unexpected updated driver: %+v", updated)
	}

	q := decode[quote](t, e.do(t, http.MethodGet, "/api/v1/ride?start_address=Berlin+Hbf&end_address=Alexanderplatz", "", nil))
	if f := fare(updated, q.DistanceKm); q.FareLovelace == nil || q.FareLovelace.Min != f || q.FareLovelace.Max != f {
		t.Fatalf("quote does not use the new fees: %+v", q)
	}
	got := decode[rideView](t, e.do(t, http.MethodGet, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, nil))
	if got.FareLovelace != oldFare {
		t.Fatalf("accepted ride's fare changed from %d to %d", oldFare, got.FareLovelace)
	}
	next := e.createRide(t)
	acc = e.do(t, http.MethodPost, "/api/v1/driver/drive/"+next.ID, d.PublicKey, map[string]any{})
	expect(t, acc, http.StatusOK)
	if f := decode[rideView](t, acc).FareLovelace; f != fare(updated, next.DistanceKm) {
		t.Fatalf("new ride's fare %d does not use the new fees", f)
	}
}

func TestRideCoordinates(t *testing.T) {
	e := newEnv(t)
	// The fake geocoder knows neither address, so only the coordinates count.
	start, end := geo.Point{Lat: 52.5251, Lng: 13.3694}, geo.Point{Lat: 52.5219, Lng: 13.4132}
	rec := e.do(t, http.MethodGet, "/api/v1/ride?start_address=Hbf&end_address=Alex&start=52.5251,13.3694&end=52.5219,13.4132", "", nil)
	expect(t, rec, http.StatusOK)
	if q := decode[quote](t, rec); q.Start != start || q.End != end || q.StartAddress != "Hbf" {
		t.Fatalf("unexpected quote: %+v", q)
	}

	rec = e.do(t, http.MethodPost, "/api/v1/ride", "", map[string]any{
		"start_address": "Hbf",
		"end_address":   "Alex",
		"pickup_time":   "2026-11-01T09:00:00Z",
		"start":         start,
		"end":           end,
	})
	expect(t, rec, http.StatusCreated)
	if r := decode[rideView](t, rec); r.Start != start || r.End != end || r.DistanceKm < 2.9 || r.DistanceKm > 3.3 {
		t.Fatalf("unexpected ride: %+v", r)
	}

	// Coordinates for one side only: the other address is still geocoded.
	rec = e.do(t, http.MethodGet, "/api/v1/ride?start_address=Hbf&end_address=Alexanderplatz&start=52.5251,13.3694", "", nil)
	expect(t, rec, http.StatusOK)
	expect(t, e.do(t, http.MethodGet, "/api/v1/ride?start_address=Hbf&end_address=Alex&start=52.5251,13.3694", "", nil), http.StatusUnprocessableEntity)

	expect(t, e.do(t, http.MethodGet, "/api/v1/ride?start_address=Hbf&end_address=Alexanderplatz&start=north", "", nil), http.StatusBadRequest)
	expect(t, e.do(t, http.MethodPost, "/api/v1/ride", "", map[string]any{
		"start_address": "Hbf",
		"end_address":   "Alexanderplatz",
		"pickup_time":   "2026-11-01T09:00:00Z",
		"start":         geo.Point{Lat: 91, Lng: 0},
	}), http.StatusBadRequest)
}

func TestSearch(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodGet, "/api/v1/search?q=Alexander", "", nil)
	expect(t, rec, http.StatusOK)
	got := decode[[]geo.Place](t, rec)
	if len(got) != 1 || got[0].Address != "Alexanderplatz" || got[0].Point != places["Alexanderplatz"] {
		t.Fatalf("unexpected places: %+v", got)
	}
	rec = e.do(t, http.MethodGet, "/api/v1/search?q=Nowhere", "", nil)
	expect(t, rec, http.StatusOK)
	if got := decode[[]geo.Place](t, rec); len(got) != 0 {
		t.Fatalf("expected no places, got %+v", got)
	}
	for _, path := range []string{"/api/v1/search", "/api/v1/search?q=Berlin&limit=0", "/api/v1/search?q=Berlin&limit=11", "/api/v1/search?q=Berlin&limit=x"} {
		expect(t, e.do(t, http.MethodGet, path, "", nil), http.StatusBadRequest)
	}
}

func TestFareFloor(t *testing.T) {
	if got := fare(Driver{BaseFee: 100, FeePerKm: 10}, 1); got != MinFareLovelace {
		t.Fatalf("expected floor, got %d", got)
	}
	if got := fare(Driver{BaseFee: 2_000_000, FeePerKm: 500_000}, 2.5); got != 3_250_000 {
		t.Fatalf("got %d", got)
	}
}

func TestDriverPayout(t *testing.T) {
	for _, custodial := range []bool{false, true} {
		e := newEnv(t)
		driver := newSigner(t)
		d := e.registerDriverWithKey(t, driver.AccountKey())
		riderKey := ""
		if !custodial {
			riderKey, _ = newSigner(t).Key(3)
		}
		ride := e.createRideWithKey(t, riderKey)
		acc := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{"eta_minutes": 3})
		expect(t, acc, http.StatusOK)
		accepted := decode[rideView](t, acc)
		if accepted.AddressIndex == nil || accepted.Script == "" {
			t.Fatalf("driver view lacks payout details: %+v", accepted)
		}

		// The driver's wallet builds and signs a spend of the ride address.
		w, _, err := e.cosigner.RideWallet(d.PublicKey, riderKey)
		if err != nil {
			t.Fatal(err)
		}
		index := *accepted.AddressIndex
		addr, _ := w.Address(index)
		if script, _ := cbor.Encode(&addr.Script); hex.EncodeToString(script) != accepted.Script {
			t.Fatal("view script does not lock the ride address")
		}
		fc := fixed.NewEmptyFixedChainContext()
		out := babbage.BabbageTransactionOutput{
			OutputAddress: addr.Address,
			OutputAmount:  mary.MaryTransactionOutputValue{Amount: 10_000_000},
		}
		fc.AddUtxo(addr.Address, common.Utxo{
			Id:     shelley.ShelleyTransactionInput{TxId: common.Blake2b256{2}, OutputIndex: 0},
			Output: &out,
		})
		dest, _ := w.Address(index + 1)
		p, err := w.BuildPayment(context.Background(), fc, multisig.Payment{To: dest.Address, Lovelace: 2_000_000, ChangeIndex: index, From: []uint32{index}})
		if err != nil {
			t.Fatal(err)
		}
		wits, err := driver.Sign(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := apollo.New(nil).LoadTxCbor(p.TxCbor)
		if err != nil {
			t.Fatal(err)
		}
		if b, err = b.AddVerificationKeyWitness(wits[0]); err != nil {
			t.Fatal(err)
		}
		partial, err := b.GetTxCbor()
		if err != nil {
			t.Fatal(err)
		}
		payout := "/api/v1/driver/drive/" + ride.ID + "/payout"
		body := map[string]string{"transaction": hex.EncodeToString(partial)}

		expect(t, e.do(t, http.MethodPost, payout, d.PublicKey, body), http.StatusConflict) // not paid yet
		req := decode[x402.PaymentRequired](t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)).Accepts[0]
		expect(t, e.paid(t, "/api/v1/ride/"+ride.ID, req), http.StatusOK)

		// Paid, but the pickup time has not come: nothing is submitted.
		setPickup := func(at time.Time) {
			if _, err := e.db.Exec(`UPDATE rides SET pickup_time = ? WHERE id = ?`, at.UTC().Format(timeLayout), ride.ID); err != nil {
				t.Fatal(err)
			}
		}
		setPickup(time.Now().Add(time.Hour))
		expect(t, e.do(t, http.MethodPost, payout, d.PublicKey, body), http.StatusConflict)
		if len(e.chain.submitted) != 0 {
			t.Fatal("a ride was paid out before its pickup time")
		}
		setPickup(time.Now().Add(-time.Hour))

		expect(t, e.do(t, http.MethodPost, payout, e.registerDriver(t).PublicKey, body), http.StatusNotFound)
		expect(t, e.do(t, http.MethodPost, payout, d.PublicKey, map[string]string{"transaction": p.TxCbor}), http.StatusUnprocessableEntity)

		// A rejected submission leaves the ride paid so the driver can retry.
		e.chain.reject = errors.New("FeeTooSmallUTxO")
		expect(t, e.do(t, http.MethodPost, payout, d.PublicKey, body), http.StatusBadGateway)
		if v := decode[rideView](t, e.do(t, http.MethodGet, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, nil)); v.Status != StatusPaid {
			t.Fatalf("rejected payout changed the ride: %+v", v)
		}
		e.chain.reject = nil

		rec := e.do(t, http.MethodPost, payout, d.PublicKey, body)
		expect(t, rec, http.StatusOK)
		res := decode[rideView](t, rec)
		wantID, _ := p.TxID()
		if res.Status != StatusPaidOut || res.PayoutTx != wantID.String() {
			t.Fatalf("unexpected paid-out ride: %+v", res)
		}
		if len(e.chain.submitted) != 1 {
			t.Fatalf("expected one submission, got %d", len(e.chain.submitted))
		}
		var tx conway.ConwayTransaction
		if _, err := cbor.DecodeExact(e.chain.submitted[0], &tx); err != nil {
			t.Fatal(err)
		}
		if n := len(tx.WitnessSet.VkeyWitnesses.Items()); n != multisig.Threshold {
			t.Fatalf("expected driver and server witnesses, got %d", n)
		}
		if _, err := w.Assemble(p, tx.WitnessSet.VkeyWitnesses.Items()...); err != nil {
			t.Fatalf("payout does not satisfy the ride script: %v", err)
		}

		expect(t, e.do(t, http.MethodPost, payout, d.PublicKey, body), http.StatusConflict)
		if len(e.chain.submitted) != 1 {
			t.Fatal("a paid-out ride was paid out again")
		}
		rider := decode[rideView](t, e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil))
		if rider.Status != StatusPaidOut || rider.Car == nil || *rider.Car != d.Car {
			t.Fatalf("unexpected rider view: %+v", rider)
		}
		if raw := e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil).Body.String(); strings.Contains(raw, d.PublicKey) {
			t.Fatalf("rider view leaks the driver's public key: %s", raw)
		}
	}
}

func TestDriverSignature(t *testing.T) {
	for _, chain := range []Chain{ChainCardano, ChainBSV} {
		t.Run(string(chain), func(t *testing.T) { testDriverSignature(t, chain) })
	}
}

// testDriverSignature refuses requests a driver of chain did not sign for
// exactly this method, path, body and moment.
func testDriverSignature(t *testing.T, chain Chain) {
	e := newEnv(t)
	register := func() string {
		if chain == ChainBSV {
			return e.registerBSVDriver(t, newBSVSigner(t).AccountKey()).PublicKey
		}
		return e.registerDriver(t).PublicKey
	}
	d, other := register(), register()
	ride := e.createRide(t)
	accept := "/api/v1/driver/drive/" + ride.ID
	body := []byte(`{"eta_minutes":4}`)

	// send posts body to accept, signed by key over signedPath and signedBody.
	send := func(key string, signedBody []byte, signedPath string, at time.Time) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, accept, bytes.NewReader(body))
		req.Header.Set(HeaderDriverKey, d)
		signRequest(req, key, signedPath, signedBody, at)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	now := time.Now()
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"unsigned":      send("", body, accept, now),
		"stale":         send(d, body, accept, now.Add(-MaxSignatureAge-time.Second)),
		"future":        send(d, body, accept, now.Add(MaxSignatureAge+time.Second)),
		"other driver":  send(other, body, accept, now),
		"tampered body": send(d, []byte(`{"eta_minutes":5}`), accept, now),
		"other ride":    send(d, body, "/api/v1/driver/drive/other", now),
	} {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	expect(t, send(d, body, accept, now.Add(-MaxSignatureAge+time.Second)), http.StatusOK)

	// Registration must be signed by the key it registers, so nobody else
	// can replace a driver's profile.
	profile := map[string]any{
		"public_key": d,
		"base_fee":   1,
		"fee_per_km": 1,
		"car":        map[string]any{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B", "year": 2021},
	}
	raw, _ := json.Marshal(profile)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/driver", bytes.NewReader(raw))
	signRequest(req, other, "/api/v1/driver", raw, now)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	expect(t, rec, http.StatusUnauthorized)
}

// TestMigrateCardanoOnlyDB upgrades a database from before BSV support.
func TestMigrateCardanoOnlyDB(t *testing.T) {
	db := newDB(t)
	if _, err := db.Exec(`
CREATE TABLE drivers (public_key TEXT PRIMARY KEY, base_fee INTEGER NOT NULL, fee_per_km INTEGER NOT NULL,
	car_brand TEXT NOT NULL, car_model TEXT NOT NULL, car_color TEXT NOT NULL,
	car_license_plate_number TEXT NOT NULL, car_year INTEGER NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE rides (address_index INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, status TEXT NOT NULL,
	start_address TEXT NOT NULL, end_address TEXT NOT NULL, start_lat REAL NOT NULL, start_lng REAL NOT NULL,
	end_lat REAL NOT NULL, end_lng REAL NOT NULL, distance_km REAL NOT NULL, pickup_time TEXT NOT NULL,
	created_at TEXT NOT NULL, rider_key TEXT NOT NULL DEFAULT '', pay_to TEXT NOT NULL DEFAULT '',
	script TEXT NOT NULL DEFAULT '', driver_key TEXT NOT NULL DEFAULT '', eta_minutes INTEGER NOT NULL DEFAULT 0,
	eta_km REAL NOT NULL DEFAULT 0, fare_lovelace INTEGER NOT NULL DEFAULT 0, payment_tx TEXT NOT NULL DEFAULT '',
	payout_tx TEXT NOT NULL DEFAULT '', claim_tx TEXT NOT NULL DEFAULT '', paying_out INTEGER NOT NULL DEFAULT 0);
INSERT INTO drivers VALUES ('d', 2000000, 500000, 'VW', 'ID.3', 'white', 'B', 2021, '2026-01-01T00:00:00Z');
INSERT INTO rides (id, status, start_address, end_address, start_lat, start_lng, end_lat, end_lng, distance_km,
	pickup_time, created_at, driver_key, fare_lovelace)
	VALUES ('accepted', 'accepted', 'a', 'b', 0, 0, 0, 0, 1, '2026-11-01T09:00:00Z', '2026-11-01T08:00:00Z', 'd', 2500000),
	       ('requested', 'requested', 'a', 'b', 0, 0, 0, 0, 1, '2026-11-01T09:00:00Z', '2026-11-01T08:00:00Z', '', 0);
`); err != nil {
		t.Fatal(err)
	}
	s, err := newStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.driver("d"); err != nil || d.Chain != ChainCardano {
		t.Fatalf("driver %+v, %v", d, err)
	}
	if r, err := s.ride("accepted"); err != nil || r.Chain != ChainCardano || r.Fare != 2_500_000 {
		t.Fatalf("accepted ride %+v, %v", r, err)
	}
	if r, err := s.ride("requested"); err != nil || r.Chain != "" {
		t.Fatalf("requested ride %+v, %v", r, err)
	}
	if _, err := newStore(db); err != nil {
		t.Fatal("migrating twice:", err)
	}
}
