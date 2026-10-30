package bsvmultisig

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/util"
)

// ErrNotCosigner is returned by Sign when the signer's key is not part of
// the scripts the proposal spends.
var ErrNotCosigner = errors.New("not a cosigner of this transaction's scripts")

// DefaultSatoshisPerKB is the fee rate used when a Payment sets none. Check
// it against the rate your broadcaster's miners currently accept.
const DefaultSatoshisPerKB = 100

// sigHashFlag is the only signature hash type this wallet signs or accepts.
const sigHashFlag = sighash.AllForkID

// unlockingSize is the most an input's unlocking script takes: OP_0 and
// Threshold pushes of a DER signature (at most 72 bytes) plus its sighash
// byte.
const unlockingSize = 1 + Threshold*(1+73)

// Payment describes an outgoing payment from the multisig wallet.
type Payment struct {
	// To is the recipient's locking script, e.g. P2PKH.
	To       *script.Script
	Satoshis uint64
	// ChangeIndex is the wallet address index that receives the change.
	ChangeIndex uint32
	// From limits the inputs to these address indexes. When empty, funds are
	// found with Discover, which only sees indexes within the gap limit.
	From []uint32
	// SatoshisPerKB defaults to DefaultSatoshisPerKB.
	SatoshisPerKB uint64
}

// Proposal is an unsigned transaction waiting for the cosigners'
// witnesses.
type Proposal struct {
	// Tx is the hex Extended Format transaction, carrying each input's amount
	// and locking script.
	Tx string `json:"tx"`
	// Indexes holds, per input, the wallet address index it spends from.
	Indexes []uint32 `json:"indexes"`
}

func (p *Proposal) decode() (*transaction.Transaction, error) {
	tx, err := decodeEF(p.Tx)
	if err != nil {
		return nil, err
	}
	if len(p.Indexes) != len(tx.Inputs) {
		return nil, fmt.Errorf("proposal has %d indexes for %d inputs", len(p.Indexes), len(tx.Inputs))
	}
	return tx, nil
}

// decodeEF parses an Extended Format transaction and checks that every input
// carries its source output.
func decodeEF(efHex string) (*transaction.Transaction, error) {
	raw, err := hex.DecodeString(efHex)
	if err != nil {
		return nil, fmt.Errorf("invalid transaction hex: %w", err)
	}
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid transaction: %w", err)
	}
	for i, in := range tx.Inputs {
		if in.SourceTxOutput() == nil || in.SourceTxScript() == nil {
			return nil, fmt.Errorf("input %d has no source output; send the transaction in Extended Format", i)
		}
	}
	return tx, nil
}

// BuildPayment collects the wallet's funds and builds an unsigned
// transaction paying p.Satoshis to p.To. Inputs are taken largest first;
// change below one satoshi is left to the fee.
func (w *Wallet) BuildPayment(ctx context.Context, chain Chain, p Payment) (*Proposal, error) {
	if p.Satoshis == 0 {
		return nil, errors.New("payment amount must be positive")
	}
	if p.To == nil || len(*p.To) == 0 {
		return nil, errors.New("payment needs a locking script to pay to")
	}
	rate := p.SatoshisPerKB
	if rate == 0 {
		rate = DefaultSatoshisPerKB
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
	change, err := w.Address(p.ChangeIndex)
	if err != nil {
		return nil, err
	}

	type coin struct {
		UTXO
		addr Address
	}
	var coins []coin
	for _, f := range funded {
		for _, u := range f.Utxos {
			coins = append(coins, coin{u, f.Address})
		}
	}
	slices.SortFunc(coins, func(a, b coin) int { return cmp.Compare(b.Satoshis, a.Satoshis) })

	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: p.Satoshis, LockingScript: p.To})
	changeOut := &transaction.TransactionOutput{LockingScript: change.LockingScript, Change: true}
	var indexes []uint32
	var in uint64
	for _, c := range coins {
		txid, err := chainhash.NewHashFromHex(c.TxID)
		if err != nil {
			return nil, fmt.Errorf("UTXO %s:%d: %w", c.TxID, c.Vout, err)
		}
		tx.AddInputWithOutput(&transaction.TransactionInput{
			SourceTXID:       txid,
			SourceTxOutIndex: c.Vout,
			SequenceNumber:   transaction.DefaultSequenceNumber,
		}, &transaction.TransactionOutput{Satoshis: c.Satoshis, LockingScript: c.addr.LockingScript})
		indexes = append(indexes, c.addr.Index)
		in += c.Satoshis

		withChange := fee(tx, changeOut, rate)
		if in >= p.Satoshis+withChange {
			if left := in - p.Satoshis - withChange; left > 0 {
				changeOut.Satoshis = left
				tx.AddOutput(changeOut)
			}
			ef, err := tx.EFHex()
			if err != nil {
				return nil, err
			}
			return &Proposal{Tx: ef, Indexes: indexes}, nil
		}
	}
	if len(coins) == 0 {
		return nil, errors.New("wallet has no funds")
	}
	return nil, fmt.Errorf("wallet holds %d satoshis, not enough for %d plus fees", in, p.Satoshis)
}

// fee estimates the fee of tx once signed and with the change output added.
func fee(tx *transaction.Transaction, change *transaction.TransactionOutput, satsPerKB uint64) uint64 {
	size := 4 + util.VarInt(len(tx.Inputs)).Length()
	size += len(tx.Inputs) * (40 + util.VarInt(unlockingSize).Length() + unlockingSize)
	outs := append(slices.Clone(tx.Outputs), change)
	size += util.VarInt(len(outs)).Length()
	for _, o := range outs {
		size += 8 + util.VarInt(len(*o.LockingScript)).Length() + len(*o.LockingScript)
	}
	size += 4
	return (uint64(size)*satsPerKB + 999) / 1000
}

// Witness is one cosigner's signature for one input. Signature is DER
// followed by the sighash byte, as it appears in the unlocking script.
type Witness struct {
	Input     int    `json:"input"`
	PubKey    []byte `json:"pubKey"`
	Signature []byte `json:"signature"`
}

// multisigKeys parses a 2-of-3 locking script into its public keys.
func multisigKeys(s *script.Script) ([Cosigners]*ec.PublicKey, error) {
	var keys [Cosigners]*ec.PublicKey
	chunks, err := s.Chunks()
	if err != nil {
		return keys, err
	}
	if len(chunks) != Cosigners+3 ||
		chunks[0].Op != script.Op1+Threshold-1 ||
		chunks[Cosigners+1].Op != script.Op1+Cosigners-1 ||
		chunks[Cosigners+2].Op != script.OpCHECKMULTISIG {
		return keys, errors.New("not a 2-of-3 multisig script")
	}
	for i := range keys {
		if keys[i], err = ec.ParsePubKey(chunks[i+1].Data); err != nil {
			return keys, err
		}
	}
	return keys, nil
}

// signInput signs input i of tx with key.
func signInput(tx *transaction.Transaction, i int, key *ec.PrivateKey) ([]byte, error) {
	hash, err := tx.CalcInputSignatureHash(uint32(i), sigHashFlag)
	if err != nil {
		return nil, err
	}
	sig, err := key.Sign(hash)
	if err != nil {
		return nil, err
	}
	return append(sig.Serialize(), byte(sigHashFlag)), nil
}

// verifyInput reports whether sig is pub's ALL|FORKID signature of input i.
func verifyInput(tx *transaction.Transaction, i int, pub *ec.PublicKey, sig []byte) bool {
	if len(sig) < 2 || sighash.Flag(sig[len(sig)-1]) != sigHashFlag {
		return false
	}
	parsed, err := ec.ParseDERSignature(sig[:len(sig)-1])
	if err != nil {
		return false
	}
	hash, err := tx.CalcInputSignatureHash(uint32(i), sigHashFlag)
	if err != nil {
		return false
	}
	// Not pub.Verify, which hashes its message again.
	return parsed.Verify(hash, pub)
}

// Sign returns this cosigner's witness for every input of the proposal. It
// refuses to sign an input whose locking script does not hold this
// cosigner's key at the input's index.
//
// Sign does not judge the payment itself; inspect the proposal's outputs
// before calling it.
func (s *Signer) Sign(p *Proposal) ([]Witness, error) {
	tx, err := p.decode()
	if err != nil {
		return nil, err
	}
	witnesses := make([]Witness, 0, len(tx.Inputs))
	for i, in := range tx.Inputs {
		keys, err := multisigKeys(in.SourceTxScript())
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		key, err := s.paymentKey(p.Indexes[i])
		if err != nil {
			return nil, err
		}
		pub := key.PubKey()
		if !slices.ContainsFunc(keys[:], func(k *ec.PublicKey) bool { return bytes.Equal(k.Compressed(), pub.Compressed()) }) {
			return nil, fmt.Errorf("input %d, index %d: %w", i, p.Indexes[i], ErrNotCosigner)
		}
		sig, err := signInput(tx, i, key)
		if err != nil {
			return nil, err
		}
		witnesses = append(witnesses, Witness{Input: i, PubKey: pub.Compressed(), Signature: sig})
	}
	return witnesses, nil
}

// Assemble verifies the witnesses against the proposal and returns the
// signed transaction. Every input must have Threshold valid signatures from
// its script's keys; extra signatures are left out.
func (w *Wallet) Assemble(p *Proposal, witnesses ...Witness) (*transaction.Transaction, error) {
	tx, err := p.decode()
	if err != nil {
		return nil, err
	}
	sigs := make([][Cosigners][]byte, len(tx.Inputs))
	for _, wit := range witnesses {
		if wit.Input < 0 || wit.Input >= len(tx.Inputs) {
			return nil, fmt.Errorf("witness for input %d, transaction has %d", wit.Input, len(tx.Inputs))
		}
		pub, err := ec.ParsePubKey(wit.PubKey)
		if err != nil {
			return nil, fmt.Errorf("witness for input %d: %w", wit.Input, err)
		}
		if !verifyInput(tx, wit.Input, pub, wit.Signature) {
			return nil, fmt.Errorf("witness for input %d does not verify against this transaction", wit.Input)
		}
		addr, err := w.Address(p.Indexes[wit.Input])
		if err != nil {
			return nil, err
		}
		pos := addr.keyPosition(pub)
		if pos < 0 || !bytes.Equal(addr.LockingScript.Bytes(), tx.Inputs[wit.Input].SourceTxScript().Bytes()) {
			return nil, fmt.Errorf("witness for input %d: %w", wit.Input, ErrNotCosigner)
		}
		sigs[wit.Input][pos] = wit.Signature
	}
	for i, in := range tx.Inputs {
		unlock, err := unlockingScript(sigs[i])
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		in.UnlockingScript = unlock
	}
	return tx, nil
}

// unlockingScript builds OP_0 followed by the first Threshold signatures in
// script key order, as OP_CHECKMULTISIG requires.
func unlockingScript(sigs [Cosigners][]byte) (*script.Script, error) {
	s := &script.Script{}
	if err := s.AppendOpcodes(script.Op0); err != nil {
		return nil, err
	}
	n := 0
	for _, sig := range sigs {
		if sig == nil || n == Threshold {
			continue
		}
		if err := s.AppendPushData(sig); err != nil {
			return nil, err
		}
		n++
	}
	if n < Threshold {
		return nil, fmt.Errorf("needs %d cosigner signatures, has %d", Threshold, n)
	}
	return s, nil
}

// Submit assembles the signed transaction and broadcasts it.
func (w *Wallet) Submit(ctx context.Context, chain Chain, p *Proposal, witnesses ...Witness) (string, error) {
	tx, err := w.Assemble(p, witnesses...)
	if err != nil {
		return "", err
	}
	return broadcast(ctx, chain, tx)
}

func broadcast(ctx context.Context, chain Chain, tx *transaction.Transaction) (string, error) {
	ok, fail := chain.BroadcastCtx(ctx, tx)
	if fail != nil {
		return "", fmt.Errorf("broadcast: %s %s", fail.Code, fail.Description)
	}
	return ok.Txid, nil
}
