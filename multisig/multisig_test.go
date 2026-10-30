package multisig

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"testing"

	"github.com/Salvionied/apollo/v2/backend/fixed"
	"github.com/blinklabs-io/bursa"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/babbage"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	"github.com/blinklabs-io/gouroboros/ledger/mary"
	"github.com/blinklabs-io/gouroboros/ledger/shelley"
)

func newSigner(t *testing.T) *Signer {
	t.Helper()
	mnemonic, err := bursa.GenerateMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(mnemonic, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func setup(t *testing.T) ([Cosigners]*Signer, *Wallet) {
	t.Helper()
	signers := [Cosigners]*Signer{newSigner(t), newSigner(t), newSigner(t)}
	w, err := NewWallet(Preprod, signers[0].AccountKey(), signers[1].AccountKey(), signers[2].AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	return signers, w
}

func addUtxo(fc *fixed.FixedChainContext, addr common.Address, lovelace uint64, txByte byte) {
	var txHash common.Blake2b256
	txHash[0] = txByte
	output := babbage.BabbageTransactionOutput{
		OutputAddress: addr,
		OutputAmount:  mary.MaryTransactionOutputValue{Amount: lovelace},
	}
	fc.AddUtxo(addr, common.Utxo{
		Id:     shelley.ShelleyTransactionInput{TxId: txHash, OutputIndex: 0},
		Output: &output,
	})
}

func sign(t *testing.T, p *Proposal, signers ...*Signer) []common.VkeyWitness {
	t.Helper()
	var all []common.VkeyWitness
	for _, s := range signers {
		wits, err := s.Sign(p)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, wits...)
	}
	return all
}

func TestAccountKeyRoundTrip(t *testing.T) {
	s := newSigner(t)
	key := s.AccountKey()
	xpub, err := ParseAccountKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := EncodeAccountKey(xpub); again != key {
		t.Fatalf("round trip mismatch: %s != %s", again, key)
	}
}

func TestAddressesMatchPrivateKeys(t *testing.T) {
	signers, w := setup(t)
	for _, index := range []uint32{0, 1, 42, 1337} {
		addr, err := w.Address(index)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range signers {
			key, err := s.paymentKey(index)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(addr.KeyHashes[:], common.Blake2b224Hash(key.PublicKey())) {
				t.Fatalf("index %d: public derivation does not match signer key", index)
			}
		}
	}
}

func TestScriptIsTwoOfThree(t *testing.T) {
	_, w := setup(t)
	addr, _ := w.Address(0)
	nofk, ok := addr.Script.Item().(*common.NativeScriptNofK)
	if !ok {
		t.Fatalf("script is %T, want N-of-K", addr.Script.Item())
	}
	if nofk.N != Threshold || len(nofk.Scripts) != Cosigners {
		t.Fatalf("script is %d-of-%d, want %d-of-%d", nofk.N, len(nofk.Scripts), Threshold, Cosigners)
	}
}

func TestAddressesIndependentOfKeyOrder(t *testing.T) {
	signers, w := setup(t)
	k := func(i int) string { return signers[i].AccountKey() }
	for _, order := range [][3]int{{1, 0, 2}, {2, 1, 0}, {1, 2, 0}} {
		other, err := NewWallet(Preprod, k(order[0]), k(order[1]), k(order[2]))
		if err != nil {
			t.Fatal(err)
		}
		for index := range uint32(3) {
			x, _ := w.Address(index)
			y, _ := other.Address(index)
			if x.String() != y.String() {
				t.Fatalf("order %v index %d: %s != %s", order, index, x, y)
			}
		}
	}
	seen := map[string]bool{}
	for index := range uint32(5) {
		x, _ := w.Address(index)
		if seen[x.String()] {
			t.Fatalf("index %d: address repeated", index)
		}
		seen[x.String()] = true
	}
	if addr, _ := w.Address(0); addr.String()[:10] != "addr_test1" {
		t.Fatalf("unexpected preprod address %s", addr)
	}
}

func TestRejectsDuplicateCosigner(t *testing.T) {
	s, other := newSigner(t), newSigner(t)
	if _, err := NewWallet(Preprod, s.AccountKey(), other.AccountKey(), s.AccountKey()); err == nil {
		t.Fatal("accepted the same cosigner twice")
	}
}

func TestPaymentAnyTwoOfThree(t *testing.T) {
	signers, w := setup(t)
	ctx := context.Background()
	fc := fixed.NewEmptyFixedChainContext()

	addr0, _ := w.Address(0)
	addr3, _ := w.Address(3)
	addUtxo(fc, addr0.Address, 4_000_000, 1)
	addUtxo(fc, addr3.Address, 10_000_000, 2)

	dest, _ := w.Address(100)
	p, err := w.BuildPayment(ctx, fc, Payment{To: dest.Address, Lovelace: 8_000_000, ChangeIndex: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Indexes, []uint32{3}) {
		t.Fatalf("expected to spend only index 3, got %v", p.Indexes)
	}
	wantID, _ := p.TxID()
	pp, _ := fc.ProtocolParams()

	for _, pair := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		wits := sign(t, p, signers[pair[0]], signers[pair[1]])
		if _, err := w.Assemble(p, wits[:1]...); err == nil {
			t.Fatalf("pair %v: assembled with one signature", pair)
		}

		signed, err := w.Assemble(p, wits...)
		if err != nil {
			t.Fatalf("pair %v: %v", pair, err)
		}
		var tx conway.ConwayTransaction
		if _, err := cbor.DecodeExact(signed, &tx); err != nil {
			t.Fatal(err)
		}
		if tx.Body.Id() != wantID {
			t.Fatal("signing changed the transaction body")
		}
		if n := len(tx.WitnessSet.VkeyWitnesses.Items()); n != Threshold {
			t.Fatalf("pair %v: expected %d vkey witnesses, got %d", pair, Threshold, n)
		}
		scripts := tx.WitnessSet.WsNativeScripts.Items()
		if len(scripts) != 1 || scripts[0].Hash() != addr3.Script.Hash() {
			t.Fatal("expected exactly the index 3 script in the witness set")
		}
		if len(tx.Body.RequiredSigners()) != 0 {
			t.Fatal("required signers would force specific cosigners")
		}
		// The fee must cover the fully signed size (min fee = a*size + b).
		minFee := uint64(pp.MinFeeCoefficient)*uint64(len(signed)) + uint64(pp.MinFeeConstant)
		if fee := tx.Body.Fee().Uint64(); fee < minFee {
			t.Fatalf("fee %d below min fee %d for signed size %d", fee, minFee, len(signed))
		}
	}

	// All three signing still yields only the two signatures needed.
	signed, err := w.Assemble(p, sign(t, p, signers[:]...)...)
	if err != nil {
		t.Fatal(err)
	}
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(signed, &tx); err != nil {
		t.Fatal(err)
	}
	if n := len(tx.WitnessSet.VkeyWitnesses.Items()); n != Threshold {
		t.Fatalf("expected %d vkey witnesses with three signers, got %d", Threshold, n)
	}
}

func TestPaymentAcrossAddresses(t *testing.T) {
	signers, w := setup(t)
	fc := fixed.NewEmptyFixedChainContext()
	addr0, _ := w.Address(0)
	addr1, _ := w.Address(1)
	addUtxo(fc, addr0.Address, 3_000_000, 1)
	addUtxo(fc, addr1.Address, 3_000_000, 2)

	dest, _ := w.Address(50)
	p, err := w.BuildPayment(context.Background(), fc, Payment{To: dest.Address, Lovelace: 5_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Indexes, []uint32{0, 1}) {
		t.Fatalf("expected indexes 0 and 1, got %v", p.Indexes)
	}
	signed, err := w.Assemble(p, sign(t, p, signers[0], signers[2])...)
	if err != nil {
		t.Fatal(err)
	}
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(signed, &tx); err != nil {
		t.Fatal(err)
	}
	if n := len(tx.WitnessSet.VkeyWitnesses.Items()); n != 2*Threshold {
		t.Fatalf("expected %d vkey witnesses, got %d", 2*Threshold, n)
	}
	pp, _ := fc.ProtocolParams()
	minFee := uint64(pp.MinFeeCoefficient)*uint64(len(signed)) + uint64(pp.MinFeeConstant)
	if fee := tx.Body.Fee().Uint64(); fee < minFee {
		t.Fatalf("fee %d below min fee %d", fee, minFee)
	}
}

func TestSignRejectsForeignSigner(t *testing.T) {
	_, w := setup(t)
	fc := fixed.NewEmptyFixedChainContext()
	addr0, _ := w.Address(0)
	addUtxo(fc, addr0.Address, 10_000_000, 1)
	p, err := w.BuildPayment(context.Background(), fc, Payment{To: addr0.Address, Lovelace: 2_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSigner(t).Sign(p); err == nil {
		t.Fatal("outsider was allowed to sign")
	}
}

func TestParseAccountKeyRejectsWrongPrefix(t *testing.T) {
	s := newSigner(t)
	xpub, _ := ParseAccountKey(s.AccountKey())
	if _, err := ParseAccountKey(hex.EncodeToString(xpub)); err == nil {
		t.Fatal("accepted hex instead of bech32")
	}
	if _, err := ParseAccountKey(xpub.String()); err == nil {
		t.Fatal("accepted root_xvk prefix")
	}
}

func newServer(t *testing.T) *Server {
	t.Helper()
	mnemonic, err := bursa.GenerateMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Preprod, mnemonic)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ridePayment funds the ride wallet at a high index, outside Discover's gap,
// and proposes spending it.
func ridePayment(t *testing.T, w *Wallet) *Proposal {
	t.Helper()
	const index = 500
	fc := fixed.NewEmptyFixedChainContext()
	addr, _ := w.Address(index)
	addUtxo(fc, addr.Address, 10_000_000, 1)
	dest, _ := w.Address(0)
	p, err := w.BuildPayment(context.Background(), fc, Payment{To: dest.Address, Lovelace: 2_000_000, ChangeIndex: index, From: []uint32{index}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Indexes, []uint32{index}) {
		t.Fatalf("expected to spend index %d, got %v", index, p.Indexes)
	}
	return p
}

func TestServerWithRiderKey(t *testing.T) {
	server, driver, rider := newServer(t), newSigner(t), newSigner(t)
	w, custodial, err := server.RideWallet(driver.AccountKey(), rider.AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	if custodial {
		t.Fatal("wallet with a rider key reported custodial")
	}
	p := ridePayment(t, w)

	serverWits, err := server.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(serverWits) != 1 {
		t.Fatalf("server should sign once, got %d witnesses", len(serverWits))
	}
	if _, err := w.Assemble(p, serverWits...); err == nil {
		t.Fatal("server alone spent a non-custodial ride")
	}
	for _, other := range []*Signer{driver, rider} {
		if _, err := w.Assemble(p, append(serverWits, sign(t, p, other)...)...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Assemble(p, sign(t, p, driver, rider)...); err != nil {
		t.Fatal(err)
	}
}

func TestServerWithoutRiderKey(t *testing.T) {
	server, driver := newServer(t), newSigner(t)
	w, custodial, err := server.RideWallet(driver.AccountKey(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !custodial {
		t.Fatal("wallet without a rider key must be custodial")
	}
	p := ridePayment(t, w)
	wits, err := server.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Assemble(p, wits...); err != nil {
		t.Fatalf("server should spend alone with its two keys: %v", err)
	}
	if _, err := w.Assemble(p, append(wits[:1], sign(t, p, driver)...)...); err != nil {
		t.Fatal(err)
	}
}

func TestServerRejectsOwnKeys(t *testing.T) {
	server, driver := newServer(t), newSigner(t)
	for _, key := range []string{server.AccountKey(), server.backup.AccountKey()} {
		if err := server.CheckCosignerKey(key); err == nil {
			t.Fatal("accepted a server key as cosigner key")
		}
		if _, _, err := server.RideWallet(driver.AccountKey(), key); err == nil {
			t.Fatal("accepted a server key as rider key")
		}
		if _, _, err := server.RideWallet(key, ""); err == nil {
			t.Fatal("accepted a server key as driver key")
		}
	}
	if _, _, err := server.RideWallet(driver.AccountKey(), driver.AccountKey()); err == nil {
		t.Fatal("accepted the same key for driver and rider")
	}
}

// partiallySigned returns the proposal's transaction carrying the signers'
// witnesses, as a counterparty's wallet would send it.
func partiallySigned(t *testing.T, p *Proposal, signers ...*Signer) []byte {
	t.Helper()
	raw, err := hex.DecodeString(p.TxCbor)
	if err != nil {
		t.Fatal(err)
	}
	if len(signers) == 0 {
		return raw
	}
	out, err := replaceVkeyWitnesses(raw, sign(t, p, signers...))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServerCosignsDriverPayout(t *testing.T) {
	server, driver, rider := newServer(t), newSigner(t), newSigner(t)
	for _, riderKey := range []string{rider.AccountKey(), ""} {
		w, _, err := server.RideWallet(driver.AccountKey(), riderKey)
		if err != nil {
			t.Fatal(err)
		}
		p := ridePayment(t, w)
		index := p.Indexes[0]
		wantID, _ := p.TxID()

		if _, _, err := server.Cosign(w, index, driver.AccountKey(), partiallySigned(t, p)); !errors.Is(err, ErrMissingSignature) {
			t.Fatalf("cosigned an unsigned transaction: %v", err)
		}
		if riderKey != "" {
			if _, _, err := server.Cosign(w, index, driver.AccountKey(), partiallySigned(t, p, rider)); !errors.Is(err, ErrMissingSignature) {
				t.Fatalf("cosigned without the driver: %v", err)
			}
		}
		if _, _, err := server.Cosign(w, index, newSigner(t).AccountKey(), partiallySigned(t, p, driver)); err == nil {
			t.Fatal("cosigned for an outsider")
		}

		signed, txID, err := server.Cosign(w, index, driver.AccountKey(), partiallySigned(t, p, driver))
		if err != nil {
			t.Fatal(err)
		}
		if txID != wantID {
			t.Fatal("cosigning changed the transaction id")
		}
		var tx conway.ConwayTransaction
		if _, err := cbor.DecodeExact(signed, &tx); err != nil {
			t.Fatal(err)
		}
		if tx.Body.Id() != wantID {
			t.Fatal("cosigning changed the transaction body")
		}
		wits := tx.WitnessSet.VkeyWitnesses.Items()
		if len(wits) != Threshold {
			t.Fatalf("expected %d witnesses, got %d", Threshold, len(wits))
		}
		addr, _ := w.Address(index)
		have := func(h common.Blake2b224) bool {
			return slices.ContainsFunc(wits, func(v common.VkeyWitness) bool { return common.Blake2b224Hash(v.Vkey) == h })
		}
		if _, ok := satisfyingKeys(addr.Script, have); !ok {
			t.Fatal("driver and server signatures do not satisfy the ride script")
		}

		// Cosigning again does not add a second server signature.
		again, _, err := server.Cosign(w, index, driver.AccountKey(), signed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cbor.DecodeExact(again, &tx); err != nil || len(tx.WitnessSet.VkeyWitnesses.Items()) != Threshold {
			t.Fatalf("re-cosigning changed the witnesses: %v", err)
		}
	}
}

func TestServerSweepsCustodialRide(t *testing.T) {
	server, driver := newServer(t), newSigner(t)
	w, _, err := server.RideWallet(driver.AccountKey(), "")
	if err != nil {
		t.Fatal(err)
	}
	const index = 500
	fc := fixed.NewEmptyFixedChainContext()
	addr, _ := w.Address(index)
	addUtxo(fc, addr.Address, 10_000_000, 1)
	addUtxo(fc, addr.Address, 3_000_000, 2)
	other, _, err := server.RideWallet(driver.AccountKey(), newSigner(t).AccountKey())
	if err != nil {
		t.Fatal(err)
	}
	toAddr, _ := other.Address(0)

	p, err := w.BuildSweep(context.Background(), fc, index, toAddr.Address)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := p.decode()
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Body.Inputs()) != 2 {
		t.Fatalf("expected both UTxOs spent, got %d inputs", len(tx.Body.Inputs()))
	}
	outs := tx.Body.Outputs()
	if len(outs) != 1 || outs[0].Address().String() != toAddr.String() {
		t.Fatalf("expected one output to %s, got %v", toAddr, outs)
	}
	if got := outs[0].Amount().Uint64() + tx.Body.Fee().Uint64(); got != 13_000_000 {
		t.Fatalf("output plus fee is %d, want 13000000", got)
	}
	witnesses, err := server.Sign(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Assemble(p, witnesses...); err != nil {
		t.Fatalf("server alone cannot spend the sweep: %v", err)
	}
}
