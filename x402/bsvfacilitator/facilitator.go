// Package bsvfacilitator verifies and settles x402 "exact" payments on
// Bitcoin SV itself, so a server can accept BSV without an external
// facilitator. It implements x402.Facilitator in process and broadcasts
// through any transaction.Broadcaster, typically ARC.
//
// The payment transaction must be in Extended Format (BRC-30): each input
// carries the output it spends, so every unlocking script is executed here
// without looking anything up. A payment is valid when every input script
// passes, the inputs cover the outputs, one output pays exactly the required
// amount to payTo, and the nonce is one of the inputs.
//
// Settlement broadcasts the transaction and succeeds once ARC accepts it;
// it does not wait for a block.
package bsvfacilitator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"kiko-backend/x402"
)

// Reasons a payment is invalid, besides x402.ReasonInvalidPayload.
const (
	ReasonUnsupportedScheme  = "unsupported_scheme"
	ReasonInvalidNetwork     = "invalid_network"
	ReasonInvalidTransaction = "invalid_transaction"
	ReasonInvalidPayment     = "invalid_payment"
	ReasonInvalidNonce       = "invalid_nonce"
	ReasonBroadcastFailed    = "broadcast_failed"
)

// Facilitator verifies and settles payments on one BSV network.
type Facilitator struct {
	// Network is x402.BSVMainnet or x402.BSVTestnet.
	Network x402.Network
	// Broadcaster submits settled payments, e.g. ARC.
	Broadcaster transaction.Broadcaster
}

// Verify checks the payment without broadcasting it.
func (f *Facilitator) Verify(_ context.Context, p x402.PaymentPayload, req x402.PaymentRequirements) (x402.VerifyResponse, error) {
	_, payer, err := f.check(p, req)
	if err != nil {
		return x402.VerifyResponse{InvalidReason: reason(err), Payer: payer}, nil
	}
	return x402.VerifyResponse{IsValid: true, Payer: payer}, nil
}

// Settle checks the payment again and broadcasts it.
func (f *Facilitator) Settle(ctx context.Context, p x402.PaymentPayload, req x402.PaymentRequirements) (x402.SettleResponse, error) {
	resp := x402.SettleResponse{Network: f.Network}
	tx, payer, err := f.check(p, req)
	resp.Payer = payer
	if err != nil {
		resp.ErrorReason = reason(err)
		return resp, nil
	}
	resp.Transaction = tx.TxID().String()
	if _, fail := f.Broadcaster.BroadcastCtx(ctx, tx); fail != nil {
		resp.ErrorReason = fmt.Sprintf("%s: %s %s", ReasonBroadcastFailed, fail.Code, fail.Description)
		return resp, nil
	}
	resp.Success = true
	return resp, nil
}

// invalid is a payment the client must fix.
type invalid struct {
	reason string
	err    error
}

func (e invalid) Error() string { return e.reason + ": " + e.err.Error() }

func reject(reason, format string, args ...any) error {
	return invalid{reason, fmt.Errorf(format, args...)}
}

// reason returns the x402 reason for a check failure, with its detail.
func reason(err error) string {
	var inv invalid
	if errors.As(err, &inv) {
		return inv.Error()
	}
	return ReasonInvalidTransaction + ": " + err.Error()
}

// check validates the payment and returns its transaction and the payer:
// the address, or else the hex locking script, of the output the nonce
// input spends.
func (f *Facilitator) check(p x402.PaymentPayload, req x402.PaymentRequirements) (*transaction.Transaction, string, error) {
	switch {
	case req.Scheme != "exact":
		return nil, "", reject(ReasonUnsupportedScheme, "scheme %q", req.Scheme)
	case req.Network != f.Network || p.Accepted.Network != f.Network:
		return nil, "", reject(ReasonInvalidNetwork, "expected %s", f.Network)
	case req.Asset != x402.Satoshi:
		return nil, "", reject(x402.ReasonInvalidPayload, "asset %q", req.Asset)
	}
	amount, err := strconv.ParseUint(req.Amount, 10, 64)
	if err != nil || amount == 0 {
		return nil, "", reject(x402.ReasonInvalidPayload, "amount %q", req.Amount)
	}
	payTo, err := f.lockingScript(req.PayTo)
	if err != nil {
		return nil, "", reject(x402.ReasonInvalidPayload, "payTo: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(p.Payload.Transaction)
	if err != nil {
		return nil, "", reject(x402.ReasonInvalidPayload, "transaction is not base64")
	}
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return nil, "", reject(x402.ReasonInvalidPayload, "transaction: %v", err)
	}

	nonce := -1
	for i, in := range tx.Inputs {
		if in.SourceTxOutput() == nil {
			return nil, "", reject(ReasonInvalidTransaction, "input %d has no source output; send the transaction in Extended Format", i)
		}
		if in.SourceTXID.String()+":"+strconv.FormatUint(uint64(in.SourceTxOutIndex), 10) == p.Payload.Nonce {
			nonce = i
		}
	}
	var payer string
	if nonce >= 0 {
		payer = f.payer(tx.Inputs[nonce].SourceTxScript())
	}
	if len(tx.Inputs) == 0 {
		return nil, payer, reject(ReasonInvalidTransaction, "no inputs")
	}
	if nonce < 0 {
		return nil, payer, reject(ReasonInvalidNonce, "nonce %q is not an input of the transaction", p.Payload.Nonce)
	}
	for i, in := range tx.Inputs {
		err := interpreter.NewEngine().Execute(
			interpreter.WithTx(tx, i, in.SourceTxOutput()), interpreter.WithForkID(), interpreter.WithAfterGenesis())
		if err != nil {
			return nil, payer, reject(ReasonInvalidTransaction, "input %d: %v", i, err)
		}
	}
	in, err := tx.TotalInputSatoshis()
	if err != nil {
		return nil, payer, err
	}
	if in < tx.TotalOutputSatoshis() {
		return nil, payer, reject(ReasonInvalidTransaction, "outputs exceed inputs")
	}
	for _, out := range tx.Outputs {
		if out.Satoshis == amount && bytes.Equal(out.LockingScript.Bytes(), payTo.Bytes()) {
			return tx, payer, nil
		}
	}
	return nil, payer, reject(ReasonInvalidPayment, "no output pays %d satoshis to payTo", amount)
}

// lockingScript returns the script payTo names: a hex locking script, or a
// P2PKH address of the network.
func (f *Facilitator) lockingScript(payTo string) (*script.Script, error) {
	if raw, err := hex.DecodeString(payTo); err == nil && len(raw) > 0 {
		s := script.NewFromBytes(raw)
		if _, err := s.Chunks(); err != nil {
			return nil, err
		}
		return s, nil
	}
	addr, err := script.NewAddressFromString(payTo)
	if err != nil {
		return nil, err
	}
	if mainnet := f.Network == x402.BSVMainnet; strings.HasPrefix(payTo, "1") != mainnet {
		return nil, fmt.Errorf("%s is not a %s address", payTo, f.Network)
	}
	return p2pkh.Lock(addr)
}

func (f *Facilitator) payer(lock *script.Script) string {
	if lock.IsP2PKH() {
		if hash, err := lock.PublicKeyHash(); err == nil {
			if addr, err := script.NewAddressFromPublicKeyHash(hash, f.Network == x402.BSVMainnet); err == nil {
				return addr.AddressString
			}
		}
	}
	return hex.EncodeToString(lock.Bytes())
}
