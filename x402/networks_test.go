package x402

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// bsvFixture holds BSV payTo values and a transaction paying the first.
type bsvFixture struct {
	address, mainnetAddress string
	scriptHex               string
	tx                      *transaction.Transaction
}

func newBSVFixture(t *testing.T) bsvFixture {
	t.Helper()
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	testnet, _ := script.NewAddressFromPublicKey(key.PubKey(), false)
	mainnet, _ := script.NewAddressFromPublicKey(key.PubKey(), true)
	lock, err := p2pkh.Lock(testnet)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := chainhash.NewHashFromHex(strings.Repeat("ab", 32))
	tx := transaction.NewTransaction()
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID:      src,
		SequenceNumber:  transaction.DefaultSequenceNumber,
		UnlockingScript: script.NewFromBytes([]byte{script.Op0}),
	}, &transaction.TransactionOutput{Satoshis: 10_000, LockingScript: lock})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 5_000, LockingScript: lock})
	return bsvFixture{
		address:        testnet.AddressString,
		mainnetAddress: mainnet.AddressString,
		scriptHex:      hex.EncodeToString(lock.Bytes()),
		tx:             tx,
	}
}

func TestBSVRequirements(t *testing.T) {
	bx := newBSVFixture(t)
	m, err := New(Config{Networks: []NetworkConfig{{Network: BSVTestnet, Facilitator: okFacilitator()}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, payTo := range []string{bx.address, bx.scriptHex} {
		req, err := m.requirements(Price{PayTo: payTo, Amount: 5_000})
		if err != nil {
			t.Fatalf("%s: %v", payTo, err)
		}
		if req.Network != BSVTestnet || req.Asset != Satoshi || req.Amount != "5000" {
			t.Fatalf("unexpected requirements: %+v", req)
		}
	}
	for _, p := range []Price{
		{PayTo: bx.mainnetAddress, Amount: 1},
		{PayTo: bx.address[:len(bx.address)-1] + "x", Amount: 1},
		{PayTo: "addr_test1vz", Amount: 1},
	} {
		if _, err := m.requirements(p); err == nil {
			t.Fatalf("expected %+v to be rejected", p)
		}
	}

	if _, err := New(Config{Networks: []NetworkConfig{{Network: "bsv:regtest", Facilitator: okFacilitator()}}}); err == nil {
		t.Fatal("expected an unknown network to be rejected")
	}
}

func TestBSVTransactionID(t *testing.T) {
	bx := newBSVFixture(t)
	ef, err := bx.tx.EF()
	if err != nil {
		t.Fatal(err)
	}
	c, _ := chainFor(BSVTestnet)
	for _, raw := range [][]byte{bx.tx.Bytes(), ef} {
		id, err := c.transactionID(base64.StdEncoding.EncodeToString(raw))
		if err != nil || id != bx.tx.TxID().String() {
			t.Fatalf("got %q %v, want %s", id, err, bx.tx.TxID())
		}
	}
	if _, err := c.transactionID(base64.StdEncoding.EncodeToString([]byte{1, 2, 3})); err == nil {
		t.Fatal("expected garbage to be rejected")
	}
}

func TestMultipleNetworks(t *testing.T) {
	fx := newFixture(t)
	bx := newBSVFixture(t)
	cardanoF, bsvF := okFacilitator(), okFacilitator()
	bsvF.settles = []SettleResponse{{Success: true, Transaction: bx.tx.TxID().String(), Network: BSVTestnet}}
	m, err := New(Config{Networks: []NetworkConfig{
		{Network: Preprod, Facilitator: cardanoF},
		{Network: BSVTestnet, Facilitator: bsvF},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := m.Require(func(*http.Request) ([]Price, error) {
		return []Price{
			{Network: Preprod, PayTo: fx.addrA, Amount: 2_000_000},
			{Network: BSVTestnet, PayTo: bx.scriptHex, Amount: 5_000},
		}, nil
	})(okHandler)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/data", nil))
	accepts := decodeRequired(t, rec).Accepts
	if len(accepts) != 2 || accepts[0].Network != Preprod || accepts[1].Network != BSVTestnet || accepts[1].Asset != Satoshi {
		t.Fatalf("expected both networks offered, got %+v", accepts)
	}

	// Paying on BSV goes to the BSV facilitator only.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepts[1], base64.StdEncoding.EncodeToString(bx.tx.Bytes())))
	if rec.Code != http.StatusOK || rec.Body.String() != "premium data" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if bsvF.verified != 1 || bsvF.settled != 1 || cardanoF.verified != 0 {
		t.Fatalf("wrong facilitator: bsv %d/%d, cardano %d", bsvF.verified, bsvF.settled, cardanoF.verified)
	}
	if bsvF.lastReq.PayTo != bx.scriptHex {
		t.Fatalf("BSV facilitator got %+v", bsvF.lastReq)
	}

	// Paying on Cardano still works.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepts[0], fx.txB64))
	if rec.Code != http.StatusOK || cardanoF.settled != 1 || bsvF.settled != 1 {
		t.Fatalf("got %d, cardano settled %d, bsv settled %d", rec.Code, cardanoF.settled, bsvF.settled)
	}

	// A Cardano transaction offered against the BSV requirements is malformed.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, paidRequest(t, "/data", accepts[1], fx.txB64))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	// With several networks a price must name its own.
	rec = httptest.NewRecorder()
	m.Require(Fixed(fx.addrA, 2_000_000))(okHandler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/data", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a price without network, got %d", rec.Code)
	}
}
