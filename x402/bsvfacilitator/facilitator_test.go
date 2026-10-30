package bsvfacilitator

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"kiko-backend/x402"
)

type fakeBroadcaster struct {
	fail *transaction.BroadcastFailure
	sent []*transaction.Transaction
}

func (b *fakeBroadcaster) Broadcast(tx *transaction.Transaction) (*transaction.BroadcastSuccess, *transaction.BroadcastFailure) {
	return b.BroadcastCtx(context.Background(), tx)
}

func (b *fakeBroadcaster) BroadcastCtx(_ context.Context, tx *transaction.Transaction) (*transaction.BroadcastSuccess, *transaction.BroadcastFailure) {
	if b.fail != nil {
		return nil, b.fail
	}
	b.sent = append(b.sent, tx)
	return &transaction.BroadcastSuccess{Txid: tx.TxID().String()}, nil
}

const prevTxID = "abababababababababababababababababababababababababababababababab"

func testnetAddress(t *testing.T) (*ec.PrivateKey, *script.Address, *script.Script) {
	t.Helper()
	key, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	addr, err := script.NewAddressFromPublicKey(key.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	return key, addr, lock
}

// payment returns a signed transaction spending 10,000 satoshis of the
// payer's to sats at to, with change back to the payer.
func payment(t *testing.T, payer *ec.PrivateKey, payerLock, to *script.Script, sats uint64) *transaction.Transaction {
	t.Helper()
	unlock, err := p2pkh.Unlock(payer, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	if err := tx.AddInputFrom(prevTxID, 0, payerLock.String(), 10_000, unlock); err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: to})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 10_000 - sats - 100, LockingScript: payerLock})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx
}

func payload(t *testing.T, raw []byte, req x402.PaymentRequirements, nonce string) x402.PaymentPayload {
	t.Helper()
	return x402.PaymentPayload{
		X402Version: x402.Version,
		Accepted:    req,
		Payload:     x402.TransactionPayload{Transaction: base64.StdEncoding.EncodeToString(raw), Nonce: nonce},
	}
}

func requirements(payTo string, amount string) x402.PaymentRequirements {
	return x402.PaymentRequirements{Scheme: "exact", Network: x402.BSVTestnet, Amount: amount, Asset: x402.Satoshi, PayTo: payTo, MaxTimeoutSeconds: 600}
}

func TestVerifyAndSettle(t *testing.T) {
	ctx := context.Background()
	b := &fakeBroadcaster{}
	f := &Facilitator{Network: x402.BSVTestnet, Broadcaster: b}
	payer, payerAddr, payerLock := testnetAddress(t)
	_, payeeAddr, payeeLock := testnetAddress(t)
	nonce := prevTxID + ":0"

	// payTo may be a hex locking script (as for a ride's bare multisig) or
	// a P2PKH address.
	tx := payment(t, payer, payerLock, payeeLock, 5_000)
	ef, err := tx.EF()
	if err != nil {
		t.Fatal(err)
	}
	for _, payTo := range []string{hex.EncodeToString(payeeLock.Bytes()), payeeAddr.AddressString} {
		req := requirements(payTo, "5000")
		v, err := f.Verify(ctx, payload(t, ef, req, nonce), req)
		if err != nil || !v.IsValid || v.Payer != payerAddr.AddressString {
			t.Fatalf("payTo %s: unexpected verify %+v, %v", payTo, v, err)
		}
	}

	req := requirements(hex.EncodeToString(payeeLock.Bytes()), "5000")
	s, err := f.Settle(ctx, payload(t, ef, req, nonce), req)
	if err != nil || !s.Success || s.Transaction != tx.TxID().String() || len(b.sent) != 1 || s.Network != x402.BSVTestnet {
		t.Fatalf("unexpected settle %+v, %v, sent %d", s, err, len(b.sent))
	}

	b.fail = &transaction.BroadcastFailure{Code: "465", Description: "fee too low"}
	if s, _ := f.Settle(ctx, payload(t, ef, req, nonce), req); s.Success || !strings.HasPrefix(s.ErrorReason, ReasonBroadcastFailed) {
		t.Fatalf("expected a broadcast failure, got %+v", s)
	}
}

func TestVerifyRejects(t *testing.T) {
	ctx := context.Background()
	f := &Facilitator{Network: x402.BSVTestnet, Broadcaster: &fakeBroadcaster{}}
	payer, _, payerLock := testnetAddress(t)
	_, _, payeeLock := testnetAddress(t)
	payTo := hex.EncodeToString(payeeLock.Bytes())
	nonce := prevTxID + ":0"

	tx := payment(t, payer, payerLock, payeeLock, 5_000)
	ef, _ := tx.EF()
	// Changing an output after signing breaks the signature.
	tampered := payment(t, payer, payerLock, payeeLock, 5_000)
	tampered.Outputs[0].Satoshis = 6_000
	tamperedEF, _ := tampered.EF()
	mainnetReq := requirements(payTo, "5000")
	mainnetReq.Network = x402.BSVMainnet

	for name, c := range map[string]struct {
		raw    []byte
		req    x402.PaymentRequirements
		nonce  string
		reason string
	}{
		"wrong amount":    {ef, requirements(payTo, "4999"), nonce, ReasonInvalidPayment},
		"wrong payee":     {ef, requirements(hex.EncodeToString(payerLock.Bytes())+"00", "5000"), nonce, ReasonInvalidPayment},
		"not EF":          {tx.Bytes(), requirements(payTo, "5000"), nonce, ReasonInvalidTransaction},
		"bad signature":   {tamperedEF, requirements(payTo, "6000"), nonce, ReasonInvalidTransaction},
		"foreign nonce":   {ef, requirements(payTo, "5000"), prevTxID + ":1", ReasonInvalidNonce},
		"other network":   {ef, mainnetReq, nonce, ReasonInvalidNetwork},
		"mainnet address": {ef, requirements("1BoatSLRHtKNngkdXEeobR76b53LETtpyT", "5000"), nonce, x402.ReasonInvalidPayload},
	} {
		v, err := f.Verify(ctx, payload(t, c.raw, c.req, c.nonce), c.req)
		if err != nil || v.IsValid || !strings.HasPrefix(v.InvalidReason, c.reason) {
			t.Errorf("%s: expected %s, got %+v, %v", name, c.reason, v, err)
		}
	}
}
