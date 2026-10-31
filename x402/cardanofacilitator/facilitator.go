// Package cardanofacilitator verifies and settles x402 "exact" lovelace
// payments on Cardano itself, so a server can accept ADA without an external
// facilitator. It implements x402.Facilitator in process and reads the chain
// through a Chain, typically Koios.
//
// It enforces the scheme's facilitator rules for address-to-address payments
// (x402 specs/schemes/exact/scheme_exact_cardano.md): an output pays at least
// the amount to payTo, the nonce and every other input are unspent, inputs
// balance outputs plus fee, the fee meets the live minimum, the TTL is set and
// within maxTimeoutSeconds, and the payment output holds the minimum UTxO
// value. Transactions it cannot balance from UTxO data, those that mint,
// withdraw, carry certificates, governance actions or donations, or run
// Plutus scripts, are refused; riders pay from plain wallets.
//
// Settlement submits the transaction and waits, bounded by Wait, for the
// confirmations the requirements ask for (one by default). Short of them it
// answers settlement_pending; settling the same payload again resumes
// watching the transaction and never submits it twice. Once the ledger holds
// the transaction, its inputs are spent by it, so verification no longer
// checks them.
package cardanofacilitator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"

	"kiko-backend/x402"
)

// Reasons a payment is invalid or unsettled, besides x402.ReasonInvalidPayload
// and x402.ReasonSettlementPending.
const (
	ReasonUnsupportedScheme  = "unsupported_scheme"
	ReasonInvalidNetwork     = "invalid_network"
	ReasonInvalidTransaction = "invalid_transaction"
	ReasonInvalidPayment     = "invalid_payment"
	ReasonInvalidNonce       = "invalid_nonce"
	ReasonSubmissionRejected = "submission_rejected"
	ReasonExpired            = "transaction_expired"
)

// DefaultWait and DefaultPoll are used when Facilitator leaves them zero.
// Two waits, settle and its one retry, fit in the server's write timeout.
const (
	DefaultWait = 90 * time.Second
	DefaultPoll = 5 * time.Second
)

// Chain is what the facilitator reads from and submits to the network.
type Chain interface {
	// Tip returns the slot and time of the latest block.
	Tip(ctx context.Context) (Tip, error)
	// Params returns the current protocol parameters the checks need.
	Params(ctx context.Context) (Params, error)
	// Outputs looks up transaction outputs, spent or not, by
	// "txHash#index"; unknown ones are left out.
	Outputs(ctx context.Context, refs []string) (map[string]Output, error)
	// Submit sends a signed transaction to the network. A *Rejected error
	// means the node refused it; after any other error it is unknown
	// whether the transaction went out.
	Submit(ctx context.Context, tx []byte) error
	// Confirmations returns how many blocks follow the one holding the
	// transaction, 0 for the newest, or -1 while it is in no block.
	Confirmations(ctx context.Context, txID string) (int, error)
}

type Tip struct {
	Slot uint64
	Time time.Time
}

type Params struct {
	MinFeeA, MinFeeB uint64
	CoinsPerUTxOByte uint64
	// SlotLength is the duration of a slot.
	SlotLength time.Duration
}

type Output struct {
	Address  string
	Lovelace uint64
	// Assets maps policy id and asset name, both hex and concatenated, to
	// quantities.
	Assets map[string]uint64
	Spent  bool
}

// Rejected is the node's refusal of a submitted transaction.
type Rejected struct{ Reason string }

func (e *Rejected) Error() string { return "rejected: " + e.Reason }

// Facilitator verifies and settles payments on one Cardano network.
type Facilitator struct {
	// Network is x402.Mainnet, x402.Preprod or x402.Preview.
	Network x402.Network
	Chain   Chain
	// Wait bounds how long Settle waits for confirmations before answering
	// settlement_pending, Poll how often it looks. Zero means DefaultWait
	// and DefaultPoll.
	Wait, Poll time.Duration

	// submitted holds the ids of transactions sent from here and not yet
	// settled or expired, which are never sent again.
	submitted sync.Map
}

// Verify checks the payment without submitting it.
func (f *Facilitator) Verify(ctx context.Context, p x402.PaymentPayload, req x402.PaymentRequirements) (x402.VerifyResponse, error) {
	pay, err := f.check(p, req)
	if err != nil {
		return x402.VerifyResponse{InvalidReason: reason(err)}, nil
	}
	inputs, err := f.Chain.Outputs(ctx, pay.inputRefs())
	if err != nil {
		return x402.VerifyResponse{}, err
	}
	payer := inputs[p.Payload.Nonce].Address
	landed, err := f.landedOrSent(ctx, pay.id)
	if err != nil {
		return x402.VerifyResponse{}, err
	}
	if !landed {
		if err := f.checkLedger(ctx, pay, inputs, req); err != nil {
			if err := chainError(err); err != nil {
				return x402.VerifyResponse{}, err
			}
			return x402.VerifyResponse{InvalidReason: reason(err), Payer: payer}, nil
		}
	}
	return x402.VerifyResponse{IsValid: true, Payer: payer}, nil
}

// Settle checks the payment again, submits it unless it was already, and
// waits for its confirmations.
func (f *Facilitator) Settle(ctx context.Context, p x402.PaymentPayload, req x402.PaymentRequirements) (x402.SettleResponse, error) {
	resp := x402.SettleResponse{Network: f.Network}
	pay, err := f.check(p, req)
	if err != nil {
		resp.ErrorReason = reason(err)
		return resp, nil
	}
	resp.Transaction = pay.id
	inputs, err := f.Chain.Outputs(ctx, pay.inputRefs())
	if err != nil {
		return resp, err
	}
	resp.Payer = inputs[p.Payload.Nonce].Address
	required := confirmationPolicy(req)

	landed, err := f.landedOrSent(ctx, pay.id)
	if err != nil {
		return resp, err
	}
	if !landed {
		if err := f.checkLedger(ctx, pay, inputs, req); err != nil {
			if err := chainError(err); err != nil {
				return resp, err
			}
			resp.ErrorReason = reason(err)
			return resp, nil
		}
		// Claimed before the first wait on submission, so a concurrent
		// settle of the same transaction watches it instead.
		if _, sent := f.submitted.LoadOrStore(pay.id, struct{}{}); !sent {
			if err := f.Chain.Submit(ctx, pay.raw); err != nil {
				var rejected *Rejected
				if errors.As(err, &rejected) {
					f.submitted.Delete(pay.id)
					resp.ErrorReason = ReasonSubmissionRejected + ": " + rejected.Reason
					return resp, nil
				}
				// It may have gone out: keep the claim and watch for it.
				return pending(resp, -1), nil
			}
			if required < 0 {
				f.submitted.Delete(pay.id)
				return settled(resp, -1), nil
			}
		}
	}
	return f.await(ctx, resp, pay.ttl, required)
}

// await polls for the transaction until it has required confirmations,
// expires, or the wait is over.
func (f *Facilitator) await(ctx context.Context, resp x402.SettleResponse, ttl uint64, required int) (x402.SettleResponse, error) {
	wait, poll := f.Wait, f.Poll
	if wait <= 0 {
		wait = DefaultWait
	}
	if poll <= 0 {
		poll = DefaultPoll
	}
	deadline := time.Now().Add(wait)
	for {
		n, err := f.Chain.Confirmations(ctx, resp.Transaction)
		if err != nil {
			return resp, err
		}
		if n >= 0 && n >= required {
			f.submitted.Delete(resp.Transaction)
			return settled(resp, n), nil
		}
		if n < 0 {
			// A block past the TTL can no longer hold the transaction.
			tip, err := f.Chain.Tip(ctx)
			if err != nil {
				return resp, err
			}
			if tip.Slot >= ttl {
				f.submitted.Delete(resp.Transaction)
				resp.ErrorReason = ReasonExpired
				return resp, nil
			}
		}
		if !time.Now().Add(poll).Before(deadline) {
			return pending(resp, n), nil
		}
		select {
		case <-ctx.Done():
			return pending(resp, n), nil
		case <-time.After(poll):
		}
	}
}

// landedOrSent reports whether the transaction is on chain or was submitted
// from here; either way its inputs may be spent by it already.
func (f *Facilitator) landedOrSent(ctx context.Context, id string) (bool, error) {
	if _, sent := f.submitted.Load(id); sent {
		return true, nil
	}
	n, err := f.Chain.Confirmations(ctx, id)
	return n >= 0, err
}

func settled(resp x402.SettleResponse, confirmations int) x402.SettleResponse {
	resp.Success = true
	resp.Extra = map[string]any{"status": "confirmed", "confirmations": confirmations}
	if confirmations < 0 {
		resp.Extra["status"] = "mempool"
	}
	return resp
}

func pending(resp x402.SettleResponse, confirmations int) x402.SettleResponse {
	resp.ErrorReason = x402.ReasonSettlementPending
	resp.Extra = map[string]any{"status": "pending", "transactionId": resp.Transaction}
	if confirmations >= 0 {
		resp.Extra["confirmations"] = confirmations
	}
	return resp
}

// confirmationPolicy returns extra.confirmationPolicy.l1Confirmations,
// defaulting to 1.
func confirmationPolicy(req x402.PaymentRequirements) int {
	policy, _ := req.Extra["confirmationPolicy"].(map[string]any)
	switch n := policy["l1Confirmations"].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 1
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

// chainError returns err if it is a failure to read the chain, which is not
// the payment's fault, and nil if it rejects the payment.
func chainError(err error) error {
	if errors.As(err, new(invalid)) {
		return nil
	}
	return err
}

// payment is a decoded payment transaction that passed the checks needing
// no chain data.
type payment struct {
	tx  conway.ConwayTransaction
	raw []byte
	id  string
	ttl uint64
	// out is the output paying payTo.
	out common.TransactionOutput
}

func (p payment) inputRefs() []string {
	refs := make([]string, 0, len(p.tx.Body.Inputs()))
	for _, in := range p.tx.Body.Inputs() {
		refs = append(refs, ref(in))
	}
	return refs
}

func ref(in common.TransactionInput) string {
	return hex.EncodeToString(in.Id().Bytes()) + "#" + strconv.FormatUint(uint64(in.Index()), 10)
}

// check decodes the payment and checks what needs no chain data.
func (f *Facilitator) check(p x402.PaymentPayload, req x402.PaymentRequirements) (payment, error) {
	switch {
	case req.Scheme != "exact":
		return payment{}, reject(ReasonUnsupportedScheme, "scheme %q", req.Scheme)
	case req.Network != f.Network || p.Accepted.Network != f.Network:
		return payment{}, reject(ReasonInvalidNetwork, "expected %s", f.Network)
	case req.Asset != x402.Lovelace:
		return payment{}, reject(x402.ReasonInvalidPayload, "asset %q", req.Asset)
	}
	amount, err := strconv.ParseUint(req.Amount, 10, 64)
	if err != nil || amount == 0 {
		return payment{}, reject(x402.ReasonInvalidPayload, "amount %q", req.Amount)
	}
	payTo, err := common.NewAddress(req.PayTo)
	if err != nil {
		return payment{}, reject(x402.ReasonInvalidPayload, "payTo: %v", err)
	}
	if mainnet := f.Network == x402.Mainnet; (payTo.NetworkId() == common.AddressNetworkMainnet) != mainnet {
		return payment{}, reject(ReasonInvalidNetwork, "payTo %s is not a %s address", req.PayTo, f.Network)
	}
	payToBytes, err := payTo.Bytes()
	if err != nil {
		return payment{}, reject(x402.ReasonInvalidPayload, "payTo: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(p.Payload.Transaction)
	if err != nil {
		return payment{}, reject(x402.ReasonInvalidPayload, "transaction is not base64")
	}
	var pay payment
	if _, err := cbor.DecodeExact(raw, &pay.tx); err != nil {
		return payment{}, reject(x402.ReasonInvalidPayload, "transaction is not a Conway transaction: %v", err)
	}
	pay.raw, pay.id, pay.ttl = raw, pay.tx.Id().String(), pay.tx.Body.TTL()

	body, wits := &pay.tx.Body, &pay.tx.WitnessSet
	if id := body.NetworkId(); id != nil && (*id == 1) != (f.Network == x402.Mainnet) {
		return payment{}, reject(ReasonInvalidNetwork, "transaction network id %d", *id)
	}
	switch {
	case len(body.Inputs()) == 0:
		return payment{}, reject(ReasonInvalidTransaction, "no inputs")
	case pay.ttl == 0:
		return payment{}, reject(ReasonInvalidTransaction, "no TTL; set the validity upper bound")
	case !pay.tx.TxIsValid || wits.WsRedeemers.Len() > 0 || len(body.Collateral()) > 0 ||
		len(wits.WsPlutusV1Scripts.Items()) > 0 || len(wits.WsPlutusV2Scripts.Items()) > 0 || len(wits.WsPlutusV3Scripts.Items()) > 0:
		return payment{}, reject(ReasonInvalidTransaction, "Plutus scripts are not supported; pay from a plain wallet")
	case body.AssetMint() != nil && len(body.AssetMint().Policies()) > 0:
		return payment{}, reject(ReasonInvalidTransaction, "minting is not supported")
	case len(body.Certificates()) > 0 || len(body.Withdrawals()) > 0 || len(body.ProposalProcedures()) > 0 ||
		len(body.VotingProcedures()) > 0 || body.Donation() != nil && body.Donation().Sign() > 0 || body.CurrentTreasuryValuePresent():
		return payment{}, reject(ReasonInvalidTransaction, "certificates, withdrawals, governance and donations are not supported")
	}

	nonce := false
	for _, in := range body.Inputs() {
		nonce = nonce || ref(in) == p.Payload.Nonce
	}
	if !nonce {
		return payment{}, reject(ReasonInvalidNonce, "nonce %q is not an input of the transaction", p.Payload.Nonce)
	}
	for _, out := range body.Outputs() {
		addr, err := out.Address().Bytes()
		if err == nil && bytes.Equal(addr, payToBytes) && out.Amount().Cmp(new(big.Int).SetUint64(amount)) >= 0 {
			pay.out = out
			return pay, nil
		}
	}
	return payment{}, reject(ReasonInvalidPayment, "no output pays %d lovelace to payTo", amount)
}

// checkLedger checks what the node would refuse, from chain data: every
// input unspent, value conserved, the fee floor, the validity interval and
// the payment output's minimum UTxO value.
func (f *Facilitator) checkLedger(ctx context.Context, pay payment, inputs map[string]Output, req x402.PaymentRequirements) error {
	params, err := f.Chain.Params(ctx)
	if err != nil {
		return err
	}
	tip, err := f.Chain.Tip(ctx)
	if err != nil {
		return err
	}

	in := value{lovelace: new(big.Int), assets: map[string]*big.Int{}}
	for _, r := range pay.inputRefs() {
		o, ok := inputs[r]
		switch {
		case !ok:
			return reject(ReasonInvalidTransaction, "input %s does not exist", r)
		case o.Spent:
			return reject(ReasonInvalidTransaction, "input %s is spent", r)
		}
		in.add(new(big.Int).SetUint64(o.Lovelace), nil)
		for unit, q := range o.Assets {
			in.addAsset(unit, new(big.Int).SetUint64(q))
		}
	}
	out := value{lovelace: new(big.Int).SetUint64(pay.tx.Body.TxFee), assets: map[string]*big.Int{}}
	for _, o := range pay.tx.Body.Outputs() {
		out.add(o.Amount(), o.Assets())
	}
	if !in.equal(out) {
		return reject(ReasonInvalidTransaction, "inputs do not balance outputs plus fee")
	}

	if floor := params.MinFeeB + params.MinFeeA*uint64(len(pay.raw)); pay.tx.Body.TxFee < floor {
		return reject(ReasonInvalidTransaction, "fee %d is below the minimum %d", pay.tx.Body.TxFee, floor)
	}

	slotLength := params.SlotLength
	if slotLength <= 0 {
		slotLength = time.Second
	}
	now := tip.Slot
	if since := time.Since(tip.Time); since > 0 {
		now += uint64(since / slotLength)
	}
	maxTTL := now + uint64(time.Duration(req.MaxTimeoutSeconds)*time.Second/slotLength)
	switch {
	case pay.ttl <= now:
		return reject(ReasonExpired, "TTL slot %d has passed", pay.ttl)
	case pay.ttl > maxTTL:
		return reject(ReasonInvalidTransaction, "TTL slot %d is later than slot %d, %ds from now", pay.ttl, maxTTL, req.MaxTimeoutSeconds)
	case pay.tx.Body.ValidityIntervalStart() > now:
		return reject(ReasonInvalidTransaction, "transaction is not valid before slot %d", pay.tx.Body.ValidityIntervalStart())
	}

	size, err := common.TransactionOutputCborSize(pay.out)
	if err != nil {
		return err
	}
	if minimum := (160 + size) * params.CoinsPerUTxOByte; pay.out.Amount().Cmp(new(big.Int).SetUint64(minimum)) < 0 {
		return reject(ReasonInvalidPayment, "payment output holds less than the minimum UTxO value %d", minimum)
	}
	return nil
}

// value sums lovelace and native assets.
type value struct {
	lovelace *big.Int
	assets   map[string]*big.Int
}

func (v value) add(lovelace *big.Int, assets *common.MultiAsset[common.MultiAssetTypeOutput]) {
	v.lovelace.Add(v.lovelace, lovelace)
	if assets == nil {
		return
	}
	for _, policy := range assets.Policies() {
		for _, name := range assets.Assets(policy) {
			v.addAsset(hex.EncodeToString(policy.Bytes())+hex.EncodeToString(name), assets.Asset(policy, name))
		}
	}
}

func (v value) addAsset(unit string, q *big.Int) {
	if v.assets[unit] == nil {
		v.assets[unit] = new(big.Int)
	}
	v.assets[unit].Add(v.assets[unit], q)
}

func (v value) equal(o value) bool {
	if v.lovelace.Cmp(o.lovelace) != 0 {
		return false
	}
	for _, m := range []struct{ a, b map[string]*big.Int }{{v.assets, o.assets}, {o.assets, v.assets}} {
		for unit, q := range m.a {
			if q.Sign() != 0 && (m.b[unit] == nil || m.b[unit].Cmp(q) != 0) {
				return false
			}
		}
	}
	return true
}
