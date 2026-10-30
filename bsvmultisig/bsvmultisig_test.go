package bsvmultisig

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/compat/bip39"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// fakeChain serves UTXOs by locking script.
type fakeChain struct {
	utxos map[string][]UTXO
}

func (c *fakeChain) Unspent(_ context.Context, s *script.Script) ([]UTXO, error) {
	return c.utxos[hex.EncodeToString(s.Bytes())], nil
}

// fund puts a UTXO of sats on the script at index.
func (c *fakeChain) fund(t *testing.T, w *Wallet, index uint32, sats uint64) {
	t.Helper()
	addr, err := w.Address(index)
	if err != nil {
		t.Fatal(err)
	}
	if c.utxos == nil {
		c.utxos = map[string][]UTXO{}
	}
	key := addr.String()
	txid := strings.Repeat("0", 60) + hex.EncodeToString([]byte{byte(index), byte(len(c.utxos[key]))})
	c.utxos[key] = append(c.utxos[key], UTXO{TxID: txid, Vout: 0, Satoshis: sats})
}

func newMnemonic(t *testing.T) string {
	t.Helper()
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		t.Fatal(err)
	}
	m, err := bip39.NewMnemonic(entropy)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(Testnet, newMnemonic(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func payee(t *testing.T) *script.Script {
	t.Helper()
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// verifyScripts runs every input through the script interpreter.
func verifyScripts(t *testing.T, tx *transaction.Transaction) {
	t.Helper()
	for i, in := range tx.Inputs {
		if err := interpreter.NewEngine().Execute(
			interpreter.WithTx(tx, i, in.SourceTxOutput()),
			interpreter.WithForkID(),
			interpreter.WithAfterGenesis(),
		); err != nil {
			t.Fatalf("input %d does not validate: %v", i, err)
		}
	}
}

func TestAddressDeterministic(t *testing.T) {
	a, b, c := newSigner(t), newSigner(t), newSigner(t)
	w1, err := NewWallet(Testnet, a.AccountKey(), b.AccountKey(), c.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	w2, err := NewWallet(Testnet, c.AccountKey(), a.AccountKey(), b.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	x, _ := w1.Address(5)
	y, _ := w2.Address(5)
	z, _ := w1.Address(6)
	if x.String() != y.String() || x.String() == z.String() {
		t.Fatalf("scripts should depend on the index only: %s %s %s", x, y, z)
	}
	if !x.LockingScript.IsMultiSigOut() {
		t.Fatalf("not a multisig output: %s", x.LockingScript)
	}
	if !strings.HasPrefix(a.AccountKey(), "tpub") {
		t.Fatalf("testnet account key should be a tpub: %s", a.AccountKey())
	}

	if _, err := NewWallet(Testnet, a.AccountKey(), a.AccountKey(), b.AccountKey()); err == nil {
		t.Fatal("expected duplicate keys to be rejected")
	}
	if _, err := NewWallet(Mainnet, a.AccountKey(), b.AccountKey(), c.AccountKey()); err == nil {
		t.Fatal("expected testnet keys to be rejected on mainnet")
	}
}

func TestPaymentTwoOfThree(t *testing.T) {
	ctx := context.Background()
	a, b, c := newSigner(t), newSigner(t), newSigner(t)
	w, err := NewWallet(Testnet, a.AccountKey(), b.AccountKey(), c.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	chain := &fakeChain{}
	chain.fund(t, w, 0, 30_000)
	chain.fund(t, w, 3, 40_000)

	to := payee(t)
	p, err := w.BuildPayment(ctx, chain, Payment{To: to, Satoshis: 50_000, ChangeIndex: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Indexes) != 2 {
		t.Fatalf("expected both UTXOs to be spent, got indexes %v", p.Indexes)
	}

	witA, err := a.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	witC, err := c.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Assemble(p, witA...); err == nil {
		t.Fatal("one cosigner alone must not be enough")
	}
	witB, _ := b.Sign(p)
	tx, err := w.Assemble(p, append(append(witA, witC...), witB...)...)
	if err != nil {
		t.Fatal(err)
	}
	verifyScripts(t, tx)

	change, _ := w.Address(9)
	if tx.Outputs[0].Satoshis != 50_000 || len(tx.Outputs) != 2 || tx.Outputs[1].LockingScript.String() != change.LockingScript.String() {
		t.Fatalf("unexpected outputs: %+v", tx.Outputs)
	}
	in, _ := tx.TotalInputSatoshis()
	paid := in - tx.TotalOutputSatoshis()
	if minFee := (uint64(tx.Size())*DefaultSatoshisPerKB + 999) / 1000; paid < minFee {
		t.Fatalf("fee %d is below %d for %d bytes", paid, minFee, tx.Size())
	}

	if _, err := newSigner(t).Sign(p); !errors.Is(err, ErrNotCosigner) {
		t.Fatalf("expected ErrNotCosigner, got %v", err)
	}
	if _, err := w.BuildPayment(ctx, chain, Payment{To: to, Satoshis: 70_000}); err == nil {
		t.Fatal("expected insufficient funds")
	}
}

func TestServerCustodialSign(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(Testnet, newMnemonic(t))
	if err != nil {
		t.Fatal(err)
	}
	driver, rider := newSigner(t), newSigner(t)

	custodial, isCustodial, err := server.RideWallet(driver.AccountKey(), "")
	if err != nil || !isCustodial {
		t.Fatalf("expected a custodial wallet: %v", err)
	}
	chain := &fakeChain{}
	chain.fund(t, custodial, 2, 20_000)
	p, err := custodial.BuildPayment(ctx, chain, Payment{To: payee(t), Satoshis: 10_000, From: []uint32{2}, ChangeIndex: 2})
	if err != nil {
		t.Fatal(err)
	}
	wits, err := server.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := custodial.Assemble(p, wits...)
	if err != nil {
		t.Fatalf("the server should spend a custodial wallet alone: %v", err)
	}
	verifyScripts(t, tx)

	if _, _, err := server.RideWallet(driver.AccountKey(), rider.AccountKey()); err == nil {
		t.Fatal("accepted an account key as the rider's key")
	}
	riderKey, _ := rider.Key(9)
	shared, isCustodial, err := server.RideWallet(driver.AccountKey(), riderKey)
	if err != nil || isCustodial {
		t.Fatalf("expected a non-custodial wallet: %v", err)
	}
	if addr, _ := shared.Address(2); !strings.Contains(addr.String(), riderKey) {
		t.Fatal("ride script does not hold the rider's key")
	}
	// The rider cannot use a key the server or driver holds at the index.
	for _, signer := range []*Signer{server.backup, server.primary, driver} {
		key, _ := signer.Key(2)
		w, _, err := server.RideWallet(driver.AccountKey(), key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Address(2); err == nil {
			t.Fatal("accepted a server or driver key as the rider's key at its index")
		}
	}
	chain.fund(t, shared, 2, 20_000)
	p, err = shared.BuildPayment(ctx, chain, Payment{To: payee(t), Satoshis: 10_000, From: []uint32{2}, ChangeIndex: 2})
	if err != nil {
		t.Fatal(err)
	}
	wits, err = server.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shared.Assemble(p, wits...); err == nil {
		t.Fatal("the server alone must not spend a wallet with a rider key")
	}
}

func TestCheckCosignerKey(t *testing.T) {
	server, err := NewServer(Testnet, newMnemonic(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.CheckCosignerKey(server.AccountKey()); err == nil {
		t.Fatal("expected the server key to be rejected")
	}
	if err := server.CheckCosignerKey(server.backup.AccountKey()); err == nil {
		t.Fatal("expected the backup key to be rejected")
	}
	if err := server.CheckCosignerKey(newSigner(t).AccountKey()); err != nil {
		t.Fatal(err)
	}
	mainnet, _ := NewSigner(Mainnet, newMnemonic(t), 0)
	if err := server.CheckCosignerKey(mainnet.AccountKey()); err == nil {
		t.Fatal("expected a mainnet key to be rejected on testnet")
	}
	if err := server.CheckCosignerKey(server.primary.account.String()); err == nil {
		t.Fatal("expected a private key to be rejected")
	}
}

// partiallySigned returns the proposal's transaction as EF hex with each
// input unlocked by OP_0 and the signer's signature, as a driver sends it.
func partiallySigned(t *testing.T, p *Proposal, s *Signer) string {
	t.Helper()
	wits, err := s.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p.decode()
	if err != nil {
		t.Fatal(err)
	}
	for _, wit := range wits {
		unlock := &script.Script{}
		unlock.AppendOpcodes(script.Op0)
		unlock.AppendPushData(wit.Signature)
		tx.Inputs[wit.Input].UnlockingScript = unlock
	}
	ef, err := tx.EFHex()
	if err != nil {
		t.Fatal(err)
	}
	return ef
}

func TestServerCosign(t *testing.T) {
	ctx := context.Background()
	server, err := NewServer(Testnet, newMnemonic(t))
	if err != nil {
		t.Fatal(err)
	}
	driver, rider := newSigner(t), newSigner(t)
	perRide, _ := rider.Key(9)
	for _, riderKey := range []string{"", perRide} {
		w, _, err := server.RideWallet(driver.AccountKey(), riderKey)
		if err != nil {
			t.Fatal(err)
		}
		chain := &fakeChain{}
		chain.fund(t, w, 7, 15_000)
		chain.fund(t, w, 7, 5_000)
		p, err := w.BuildPayment(ctx, chain, Payment{To: payee(t), Satoshis: 19_000, From: []uint32{7}, ChangeIndex: 7})
		if err != nil {
			t.Fatal(err)
		}

		unsigned := p.Tx
		if _, _, err := server.Cosign(w, 7, driver.AccountKey(), unsigned); !errors.Is(err, ErrMissingSignature) {
			t.Fatalf("expected ErrMissingSignature, got %v", err)
		}
		signed := partiallySigned(t, p, driver)
		if _, _, err := server.Cosign(w, 7, newSigner(t).AccountKey(), signed); err == nil {
			t.Fatal("an outsider's key must not count as the driver's signature")
		}
		if _, _, err := server.Cosign(w, 6, driver.AccountKey(), signed); err == nil {
			t.Fatal("expected a wrong index to be rejected")
		}

		tx, txID, err := server.Cosign(w, 7, driver.AccountKey(), signed)
		if err != nil {
			t.Fatal(err)
		}
		verifyScripts(t, tx)
		if txID != tx.TxID().String() {
			t.Fatalf("returned id %s, transaction is %s", txID, tx.TxID())
		}
	}
}
