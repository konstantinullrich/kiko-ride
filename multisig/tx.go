package multisig

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	apollo "github.com/Salvionied/apollo/v2"
	"github.com/Salvionied/apollo/v2/backend"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
)

// ErrNotCosigner is returned by Sign when the signer's key is not part of
// the proposal's scripts.
var ErrNotCosigner = errors.New("not a cosigner of this transaction's scripts")

// vkeyWitnessSize is the CBOR size of one vkey witness (array header, 32-byte
// key and 64-byte signature with their headers) plus a few bytes of slack.
const vkeyWitnessSize = 110

// Payment describes an outgoing lovelace payment from the multisig wallet.
type Payment struct {
	To       common.Address
	Lovelace int64
	// ChangeIndex is the wallet address index that receives the change.
	ChangeIndex uint32
	// From limits the inputs to these address indexes. When empty, funds are
	// found with Discover, which only sees indexes within the gap limit.
	From []uint32
}

// Proposal is an unsigned transaction waiting for both cosigners' witnesses.
type Proposal struct {
	// TxCbor is the hex-encoded unsigned transaction.
	TxCbor string `json:"txCbor"`
	// Indexes are the wallet address indexes whose UTxOs the transaction
	// spends; each cosigner signs once per index.
	Indexes []uint32 `json:"indexes"`
}

// TxID returns the transaction hash both cosigners sign.
func (p *Proposal) TxID() (common.Blake2b256, error) {
	tx, err := p.decode()
	if err != nil {
		return common.Blake2b256{}, err
	}
	return tx.Body.Id(), nil
}

func (p *Proposal) decode() (*conway.ConwayTransaction, error) {
	raw, err := hex.DecodeString(p.TxCbor)
	if err != nil {
		return nil, fmt.Errorf("invalid transaction hex: %w", err)
	}
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(raw, &tx); err != nil {
		return nil, fmt.Errorf("invalid transaction: %w", err)
	}
	return &tx, nil
}

// BuildPayment discovers the wallet's funds and builds an unsigned
// transaction paying p.Lovelace to p.To.
func (w *Wallet) BuildPayment(ctx context.Context, chain backend.ChainContext, p Payment) (*Proposal, error) {
	if p.Lovelace <= 0 {
		return nil, errors.New("payment amount must be positive")
	}
	var funded []FundedAddress
	var err error
	if len(p.From) > 0 {
		funded, err = w.funded(ctx, chain, p.From)
	} else {
		funded, err = w.Discover(ctx, chain, 0)
	}
	if err != nil {
		return nil, err
	}
	if len(funded) == 0 {
		return nil, errors.New("wallet has no funds")
	}
	change, err := w.Address(p.ChangeIndex)
	if err != nil {
		return nil, err
	}

	// Pass 1: let Apollo select coins from every funded address. Attaching
	// every script over-estimates the fee, which is safe.
	var pool []common.Utxo
	byRef := make(map[string]Address)
	var all []Address
	for _, f := range funded {
		pool = append(pool, f.Utxos...)
		all = append(all, f.Address)
		for _, u := range f.Utxos {
			byRef[u.Id.String()] = f.Address
		}
	}
	draft, err := w.build(ctx, chain, p, change.Address, pool, nil, all)
	if err != nil {
		return nil, err
	}

	// Pass 2: rebuild with exactly the selected inputs and only their
	// scripts. The ledger rejects transactions carrying scripts no input
	// needs.
	var selected []common.Utxo
	var used []Address
	for _, in := range draft.Body.Inputs() {
		addr, ok := byRef[in.String()]
		if !ok {
			return nil, fmt.Errorf("selected input %s is not a wallet UTxO", in)
		}
		for _, u := range pool {
			if u.Id.String() == in.String() {
				selected = append(selected, u)
				break
			}
		}
		if !slices.ContainsFunc(used, func(a Address) bool { return a.Index == addr.Index }) {
			used = append(used, addr)
		}
	}
	tx, err := w.build(ctx, chain, p, change.Address, nil, selected, used)
	if err != nil {
		return nil, err
	}
	return newProposal(tx, used)
}

// BuildSweep builds an unsigned transaction that sends everything at the
// address index, tokens included, to `to`, less the fee.
func (w *Wallet) BuildSweep(ctx context.Context, chain backend.ChainContext, index uint32, to common.Address) (*Proposal, error) {
	funded, err := w.funded(ctx, chain, []uint32{index})
	if err != nil {
		return nil, err
	}
	if len(funded) == 0 {
		return nil, fmt.Errorf("address %d has no funds", index)
	}
	// With no payment the inputs, less the fee, all go to the change
	// address.
	f := funded[0]
	tx, err := w.build(ctx, chain, Payment{}, to, nil, f.Utxos, []Address{f.Address})
	if err != nil {
		return nil, err
	}
	return newProposal(tx, []Address{f.Address})
}

func newProposal(tx *conway.ConwayTransaction, used []Address) (*Proposal, error) {
	txCbor, err := cbor.Encode(tx)
	if err != nil {
		return nil, err
	}
	indexes := make([]uint32, 0, len(used))
	for _, a := range used {
		indexes = append(indexes, a.Index)
	}
	slices.Sort(indexes)
	return &Proposal{TxCbor: hex.EncodeToString(txCbor), Indexes: indexes}, nil
}

func (w *Wallet) build(
	ctx context.Context,
	chain backend.ChainContext,
	p Payment,
	change common.Address,
	pool []common.Utxo,
	inputs []common.Utxo,
	addrs []Address,
) (*conway.ConwayTransaction, error) {
	b := apollo.New(chain).
		WithContext(ctx).
		SetWallet(apollo.NewExternalWallet(change)).
		SetChangeAddress(change).
		AddLoadedUTxOs(pool...)
	for _, u := range inputs {
		b = b.AddInput(u)
	}
	for _, a := range addrs {
		b = b.AttachScript(a.Script)
	}
	// Apollo's fee estimate does not count vkey witnesses for script inputs.
	// Pad for Threshold signatures per script instead of declaring required
	// signers, which would force every cosigner to sign.
	pp, err := backend.ProtocolParamsContext(ctx, chain)
	if err != nil {
		return nil, fmt.Errorf("load protocol parameters: %w", err)
	}
	padding := int64(Threshold*len(addrs)) * vkeyWitnessSize * pp.MinFeeCoefficient
	b = b.SetFeePadding(padding)
	if p.Lovelace > 0 {
		b = b.PayToAddress(p.To, p.Lovelace)
	}
	if b, err = b.Complete(); err != nil {
		return nil, fmt.Errorf("build transaction: %w", err)
	}
	return b.GetTx(), nil
}

// Sign returns this cosigner's witnesses for every address index the
// proposal spends from. It refuses to sign with a key that none of the
// transaction's attached scripts mention.
//
// Sign does not judge the payment itself; inspect the proposal's outputs
// before calling it.
func (s *Signer) Sign(p *Proposal) ([]common.VkeyWitness, error) {
	tx, err := p.decode()
	if err != nil {
		return nil, err
	}
	var scriptKeys []common.Blake2b224
	for _, script := range tx.WitnessSet.WsNativeScripts.Items() {
		scriptKeys = append(scriptKeys, scriptKeyHashes(script)...)
	}
	txID := tx.Body.Id()
	witnesses := make([]common.VkeyWitness, 0, len(p.Indexes))
	for _, index := range p.Indexes {
		key, err := s.paymentKey(index)
		if err != nil {
			return nil, err
		}
		pub := key.PublicKey()
		if !slices.Contains(scriptKeys, common.Blake2b224Hash(pub)) {
			return nil, fmt.Errorf("key at index %d: %w", index, ErrNotCosigner)
		}
		witnesses = append(witnesses, common.VkeyWitness{
			Vkey:      pub,
			Signature: key.Sign(txID.Bytes()),
		})
	}
	return witnesses, nil
}

// Assemble verifies the witnesses against the proposal and returns the
// signed transaction CBOR. Every attached script must reach its threshold of
// valid signatures; only the signatures needed are included, so a third
// cosigner's signature does not grow the transaction past its fee.
func (w *Wallet) Assemble(p *Proposal, witnesses ...common.VkeyWitness) ([]byte, error) {
	tx, err := p.decode()
	if err != nil {
		return nil, err
	}
	txID := tx.Body.Id()
	signed := make(map[common.Blake2b224]common.VkeyWitness)
	for _, wit := range witnesses {
		if len(wit.Vkey) != ed25519.PublicKeySize ||
			!ed25519.Verify(ed25519.PublicKey(wit.Vkey), txID.Bytes(), wit.Signature) {
			return nil, errors.New("witness signature does not verify against this transaction")
		}
		signed[common.Blake2b224Hash(wit.Vkey)] = wit
	}
	b := apollo.New(nil)
	if b, err = b.LoadTxCbor(p.TxCbor); err != nil {
		return nil, err
	}
	isSigned := func(h common.Blake2b224) bool { _, ok := signed[h]; return ok }
	var include []common.Blake2b224
	for _, script := range tx.WitnessSet.WsNativeScripts.Items() {
		keys, ok := satisfyingKeys(script, isSigned)
		if !ok {
			return nil, fmt.Errorf("script %s needs %d cosigner signatures", script.Hash(), Threshold)
		}
		for _, k := range keys {
			if !slices.Contains(include, k) {
				include = append(include, k)
			}
		}
	}
	for _, hash := range include {
		if b, err = b.AddVerificationKeyWitness(signed[hash]); err != nil {
			return nil, err
		}
	}
	return b.GetTxCbor()
}
