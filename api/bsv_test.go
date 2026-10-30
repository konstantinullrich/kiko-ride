package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"kiko-backend/bsvmultisig"
	"kiko-backend/x402"
)

// fakeBroadcaster records BSV payouts.
type fakeBroadcaster struct {
	sent []*transaction.Transaction
}

func (b *fakeBroadcaster) Broadcast(tx *transaction.Transaction) (*transaction.BroadcastSuccess, *transaction.BroadcastFailure) {
	return b.BroadcastCtx(context.Background(), tx)
}

func (b *fakeBroadcaster) BroadcastCtx(_ context.Context, tx *transaction.Transaction) (*transaction.BroadcastSuccess, *transaction.BroadcastFailure) {
	b.sent = append(b.sent, tx)
	return &transaction.BroadcastSuccess{Txid: tx.TxID().String()}, nil
}

// bsvUTXOs serves one UTXO at the ride script, enough for BuildPayment.
type bsvUTXOs struct {
	script string
	utxo   bsvmultisig.UTXO
	*fakeBroadcaster
}

func (c bsvUTXOs) Unspent(_ context.Context, s *script.Script) ([]bsvmultisig.UTXO, error) {
	if hex.EncodeToString(s.Bytes()) == c.script {
		return []bsvmultisig.UTXO{c.utxo}, nil
	}
	return nil, nil
}

func newBSVSigner(t *testing.T) *bsvmultisig.Signer {
	t.Helper()
	s, err := bsvmultisig.NewSigner(bsvmultisig.Testnet, newMnemonic(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	signers.Store(s.AccountKey(), func(msg []byte) []byte {
		sig, err := s.SignMessage(msg)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	})
	return s
}

func (e *env) registerBSVDriver(t *testing.T, publicKey string) driverView {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
		"public_key": publicKey,
		"base_fee":   20_000,
		"fee_per_km": 5_000,
		"car":        map[string]any{"brand": "Tesla", "model": "3", "color": "red", "license_plate_number": "B-SV 1", "year": 2022},
	})
	expect(t, rec, http.StatusCreated)
	d := decode[driverView](t, rec)
	d.Chain = ChainBSV // not serialized; network carries it
	return d
}

// bsvPayment returns a base64 raw BSV transaction for the x402 payload.
func bsvPayment(t *testing.T) string {
	t.Helper()
	key, _ := ec.NewPrivateKey()
	addr, _ := script.NewAddressFromPublicKey(key.PubKey(), false)
	lock, _ := p2pkh.Lock(addr)
	src, _ := chainhash.NewHashFromHex(strings.Repeat("cd", 32))
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: src, SequenceNumber: transaction.DefaultSequenceNumber, UnlockingScript: script.NewFromBytes([]byte{script.Op0})})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	return base64.StdEncoding.EncodeToString(tx.Bytes())
}

func TestDriverKeyPicksChain(t *testing.T) {
	e := newEnv(t)
	cardano := e.registerDriver(t)
	bsvSigner := newBSVSigner(t)
	bsv := e.registerBSVDriver(t, bsvSigner.AccountKey())
	if bsv.Network != x402.BSVTestnet || bsv.PublicKey != bsvSigner.AccountKey() {
		t.Fatalf("unexpected BSV driver: %+v", bsv)
	}

	for _, key := range []string{"not-a-key", e.bsv.AccountKey(), "xpub" + strings.Repeat("1", 107)} {
		rec := e.do(t, http.MethodPost, "/api/v1/driver", "", map[string]any{
			"public_key": key, "base_fee": 1, "fee_per_km": 1,
			"car": map[string]any{"brand": "a", "model": "b", "color": "c", "license_plate_number": "d", "year": 2020},
		})
		expect(t, rec, http.StatusBadRequest)
	}

	// The quote prices each network in its own unit.
	q := decode[quote](t, e.do(t, http.MethodGet, "/api/v1/ride?start_address=Berlin+Hbf&end_address=Alexanderplatz", "", nil))
	if len(q.Fares) != 2 || q.Fares[0].Network != x402.Preprod || q.Fares[1].Network != x402.BSVTestnet {
		t.Fatalf("expected a fare per network, got %+v", q.Fares)
	}
	if q.Fares[1].Min != fare(Driver{Chain: ChainBSV, BaseFee: 20_000, FeePerKm: 5_000}, q.DistanceKm) ||
		q.FareLovelace == nil || q.FareLovelace.Min != fare(cardano, q.DistanceKm) {
		t.Fatalf("unexpected fares: %+v %+v", q.Fares, q.FareLovelace)
	}

	// A rider with a BSV key can only be taken by BSV drivers.
	ride := e.createRideWithKey(t, newBSVSigner(t).AccountKey())
	avail := func(driverKey string) int {
		return len(decode[[]availableRide](t, e.do(t, http.MethodGet, "/api/v1/driver/drive/available?location=52.52,13.37", driverKey, nil)))
	}
	if avail(cardano.PublicKey) != 0 || avail(bsv.PublicKey) != 1 {
		t.Fatal("a BSV rider's ride must only be listed to BSV drivers")
	}
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, cardano.PublicKey, map[string]any{}), http.StatusUnprocessableEntity)
	expect(t, e.do(t, http.MethodGet, "/api/v1/driver/drive/available?location=52.52,13.37", "", nil), http.StatusUnauthorized)

	// A custodial ride is open to both.
	e.createRide(t)
	if avail(cardano.PublicKey) != 1 || avail(bsv.PublicKey) != 2 {
		t.Fatal("a custodial ride must be listed to drivers of every chain")
	}
}

func TestBSVRideLifecycle(t *testing.T) {
	e := newEnv(t)
	driver := newBSVSigner(t)
	d := e.registerBSVDriver(t, driver.AccountKey())
	ride := e.createRide(t)

	acc := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{"eta_minutes": 3})
	expect(t, acc, http.StatusOK)
	accepted := decode[rideView](t, acc)
	wantFare := fare(d.Driver, accepted.DistanceKm)
	if accepted.Network != x402.BSVTestnet || accepted.Fare != wantFare || accepted.FareLovelace != 0 ||
		accepted.PayTo == "" || accepted.Script != accepted.PayTo {
		t.Fatalf("unexpected accepted BSV ride: %+v", accepted)
	}
	wallet, custodial, err := e.bsv.RideWallet(d.PublicKey, "")
	if err != nil || !custodial {
		t.Fatal(err)
	}
	if addr, _ := wallet.Address(*accepted.AddressIndex); addr.String() != accepted.PayTo {
		t.Fatalf("pay_to %s is not the ride script %s", accepted.PayTo, addr)
	}

	unpaid := e.do(t, http.MethodGet, "/api/v1/ride/"+ride.ID, "", nil)
	expect(t, unpaid, http.StatusPaymentRequired)
	req := decode[x402.PaymentRequired](t, unpaid).Accepts
	if len(req) != 1 || req[0].Network != x402.BSVTestnet || req[0].Asset != x402.Satoshi || req[0].PayTo != accepted.PayTo {
		t.Fatalf("unexpected requirements: %+v", req)
	}
	raw, _ := json.Marshal(x402.PaymentPayload{
		X402Version: x402.Version,
		Accepted:    req[0],
		Payload:     x402.TransactionPayload{Transaction: bsvPayment(t), Nonce: strings.Repeat("cd", 32) + ":0"},
	})
	pay := httptest.NewRequest(http.MethodGet, "/api/v1/ride/"+ride.ID, nil)
	pay.Header.Set(x402.HeaderPaymentSignature, base64.StdEncoding.EncodeToString(raw))
	paid := httptest.NewRecorder()
	e.h.ServeHTTP(paid, pay)
	expect(t, paid, http.StatusOK)
	if v := decode[rideView](t, paid); v.Status != StatusPaid || v.Car == nil || v.Car.Brand != "Tesla" {
		t.Fatalf("unexpected paid ride: %+v", v)
	}

	// The driver spends the ride script, signs, and the server cosigns.
	chain := bsvUTXOs{script: accepted.PayTo, utxo: bsvmultisig.UTXO{TxID: strings.Repeat("ef", 32), Satoshis: wantFare}, fakeBroadcaster: &fakeBroadcaster{}}
	key, _ := ec.NewPrivateKey()
	addr, _ := script.NewAddressFromPublicKey(key.PubKey(), false)
	to, _ := p2pkh.Lock(addr)
	p, err := wallet.BuildPayment(context.Background(), chain, bsvmultisig.Payment{To: to, Satoshis: wantFare - 1_000, From: []uint32{*accepted.AddressIndex}, ChangeIndex: *accepted.AddressIndex})
	if err != nil {
		t.Fatal(err)
	}
	wits, err := driver.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	ef, _ := hex.DecodeString(p.Tx)
	tx, _ := transaction.NewTransactionFromBytes(ef)
	for _, wit := range wits {
		unlock := &script.Script{}
		unlock.AppendOpcodes(script.Op0)
		unlock.AppendPushData(wit.Signature)
		tx.Inputs[wit.Input].UnlockingScript = unlock
	}
	signedHex, _ := tx.EFHex()

	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID+"/payout", d.PublicKey, map[string]string{"transaction": p.Tx}), http.StatusUnprocessableEntity)
	out := e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID+"/payout", d.PublicKey, map[string]string{"transaction": signedHex})
	expect(t, out, http.StatusOK)
	v := decode[rideView](t, out)
	if v.Status != StatusPaidOut || len(e.broadcaster.sent) != 1 || v.PayoutTx != e.broadcaster.sent[0].TxID().String() {
		t.Fatalf("unexpected payout: %+v, broadcast %d", v, len(e.broadcaster.sent))
	}
	sent := e.broadcaster.sent[0]
	for i, in := range sent.Inputs {
		if err := interpreter.NewEngine().Execute(interpreter.WithTx(sent, i, in.SourceTxOutput()), interpreter.WithForkID(), interpreter.WithAfterGenesis()); err != nil {
			t.Fatalf("payout input %d does not validate: %v", i, err)
		}
	}
}

func TestBSVDriverSignature(t *testing.T) {
	e := newEnv(t)
	d, other := newBSVSigner(t), newBSVSigner(t)
	raw, _ := json.Marshal(map[string]any{
		"public_key": d.AccountKey(),
		"base_fee":   1000,
		"fee_per_km": 100,
		"car":        map[string]any{"brand": "VW", "model": "ID.3", "color": "white", "license_plate_number": "B", "year": 2021},
	})
	for name, key := range map[string]string{"other key": other.AccountKey(), "own key": d.AccountKey()} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/driver", bytes.NewReader(raw))
		signRequest(req, key, "/api/v1/driver", raw, time.Now())
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if key == d.AccountKey() {
			want = http.StatusCreated
		}
		if rec.Code != want {
			t.Errorf("%s: expected %d, got %d: %s", name, want, rec.Code, rec.Body.String())
		}
	}
}
