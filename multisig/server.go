package multisig

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"

	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
)

// ErrMissingSignature is returned by Cosign when the counterparty has not
// signed the transaction yet.
var ErrMissingSignature = errors.New("transaction is not signed by the cosigner")

const (
	// serverAccount is the CIP-1854 account of the key Kiko adds to every
	// ride.
	serverAccount = 0
	// backupAccount is the CIP-1854 account of the key Kiko adds in place of
	// a rider who brings no key.
	backupAccount = 1
)

// Server is the cosigner run by the Kiko backend. Both of its keys come from
// one mnemonic, so nothing beyond it needs backing up:
//
//   - account 0 cosigns every ride wallet,
//   - account 1 stands in for a rider who does not provide a key.
//
// A ride wallet is therefore either 2-of-{server, driver, rider}, where the
// server alone cannot spend, or 2-of-{server, driver, backup}, where the
// server holds two keys and spends on the rider's behalf.
type Server struct {
	network Network
	primary *Signer
	backup  *Signer
}

// NewServer derives the server's two cosigner accounts from a BIP-39
// mnemonic.
func NewServer(network Network, mnemonic string) (*Server, error) {
	primary, err := NewSigner(mnemonic, serverAccount)
	if err != nil {
		return nil, err
	}
	backup, err := NewSigner(mnemonic, backupAccount)
	if err != nil {
		return nil, err
	}
	return &Server{network: network, primary: primary, backup: backup}, nil
}

// AccountKey returns the server's own "acct_shared_xvk", present in every
// ride wallet.
func (s *Server) AccountKey() string {
	return s.primary.AccountKey()
}

// CheckCosignerKey reports whether key is acceptable as a driver's or
// rider's cosigner key: a valid "acct_shared_xvk" that is not one of the
// server's own. Accepting the backup key from a driver or rider would hand
// the server a second key on a wallet it should not control alone.
func (s *Server) CheckCosignerKey(key string) error {
	xpub, err := ParseAccountKey(key)
	if err != nil {
		return err
	}
	for _, own := range []*Signer{s.primary, s.backup} {
		if bytes.Equal(xpub, own.account.Public()) {
			return errors.New("key belongs to the Kiko server")
		}
	}
	return nil
}

// RideWallet returns the 2-of-3 wallet of the server, the driver and the
// rider. If riderKey is empty the server's backup key takes the rider's
// place and custodial is true: the server then controls two of the three
// keys.
func (s *Server) RideWallet(driverKey, riderKey string) (w *Wallet, custodial bool, err error) {
	if err := s.CheckCosignerKey(driverKey); err != nil {
		return nil, false, fmt.Errorf("driver key: %w", err)
	}
	if riderKey == "" {
		riderKey, custodial = s.backup.AccountKey(), true
	} else if err := s.CheckCosignerKey(riderKey); err != nil {
		return nil, false, fmt.Errorf("rider key: %w", err)
	}
	w, err = NewWallet(s.network, s.primary.AccountKey(), driverKey, riderKey)
	if err != nil {
		return nil, false, err
	}
	return w, custodial, nil
}

// Sign returns the server's witnesses for a proposal from a ride wallet: the
// primary key's, plus the backup key's when the wallet is custodial. On a
// custodial wallet these alone satisfy the 2-of-3 scripts.
//
// Like Signer.Sign it does not judge the payment itself.
func (s *Server) Sign(p *Proposal) ([]common.VkeyWitness, error) {
	witnesses, err := s.primary.Sign(p)
	if err != nil {
		return nil, err
	}
	backup, err := s.backup.Sign(p)
	switch {
	case errors.Is(err, ErrNotCosigner):
		return witnesses, nil
	case err != nil:
		return nil, err
	}
	return append(witnesses, backup...), nil
}

// Cosign adds the server's signature to a transaction that spends the wallet
// address at index and that cosignerKey has already signed, returning the
// signed transaction CBOR and its id.
//
// The server never gives the first signature: the transaction must carry the
// address's script and a valid witness from cosignerKey's key at index, and
// every witness present must verify. Only the primary key signs, so a
// custodial wallet's backup key is never spent here; the counterparty's and
// the server's signatures make the two of three.
//
// The transaction body is kept byte for byte, so its id does not change. The
// counterparty must have paid the fee for the server's witness.
func (s *Server) Cosign(w *Wallet, index uint32, cosignerKey string, txCbor []byte) ([]byte, common.Blake2b256, error) {
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(txCbor, &tx); err != nil {
		return nil, common.Blake2b256{}, fmt.Errorf("invalid transaction: %w", err)
	}
	addr, err := w.Address(index)
	if err != nil {
		return nil, common.Blake2b256{}, err
	}
	scriptHash := addr.Script.Hash()
	if !slices.ContainsFunc(tx.WitnessSet.WsNativeScripts.Items(), func(ns common.NativeScript) bool {
		return ns.Hash() == scriptHash
	}) {
		return nil, common.Blake2b256{}, fmt.Errorf("transaction does not carry the script of address %d", index)
	}
	account, err := ParseAccountKey(cosignerKey)
	if err != nil {
		return nil, common.Blake2b256{}, err
	}
	cosigner, err := paymentKeyHash(account, index)
	if err != nil {
		return nil, common.Blake2b256{}, err
	}
	key, err := s.primary.paymentKey(index)
	if err != nil {
		return nil, common.Blake2b256{}, err
	}
	pub := key.PublicKey()
	server := common.Blake2b224Hash(pub)
	if !slices.Contains(addr.KeyHashes[:], cosigner) || !slices.Contains(addr.KeyHashes[:], server) {
		return nil, common.Blake2b256{}, fmt.Errorf("address %d is not cosigned by the server and %s", index, cosignerKey)
	}

	txID := tx.Body.Id()
	var witnesses []common.VkeyWitness
	signed := false
	for _, wit := range tx.WitnessSet.VkeyWitnesses.Items() {
		if len(wit.Vkey) != ed25519.PublicKeySize ||
			!ed25519.Verify(ed25519.PublicKey(wit.Vkey), txID.Bytes(), wit.Signature) {
			return nil, common.Blake2b256{}, errors.New("witness signature does not verify against this transaction")
		}
		hash := common.Blake2b224Hash(wit.Vkey)
		if hash == server {
			continue
		}
		signed = signed || hash == cosigner
		witnesses = append(witnesses, wit)
	}
	if !signed {
		return nil, common.Blake2b256{}, ErrMissingSignature
	}
	witnesses = append(witnesses, common.VkeyWitness{Vkey: pub, Signature: key.Sign(txID.Bytes())})

	out, err := replaceVkeyWitnesses(txCbor, witnesses)
	if err != nil {
		return nil, common.Blake2b256{}, err
	}
	return out, txID, nil
}

// replaceVkeyWitnesses swaps the vkey witnesses (witness set key 0) of a
// transaction, leaving the body, the other witnesses and the auxiliary data
// as they were encoded.
func replaceVkeyWitnesses(txCbor []byte, witnesses []common.VkeyWitness) ([]byte, error) {
	var parts []cbor.RawMessage
	if _, err := cbor.DecodeExact(txCbor, &parts); err != nil {
		return nil, fmt.Errorf("invalid transaction: %w", err)
	}
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid transaction: %d parts, expected 4", len(parts))
	}
	var set map[uint64]cbor.RawMessage
	if _, err := cbor.DecodeExact(parts[1], &set); err != nil {
		return nil, fmt.Errorf("invalid witness set: %w", err)
	}
	vkeys, err := cbor.Encode(witnesses)
	if err != nil {
		return nil, err
	}
	set[0] = vkeys
	if parts[1], err = cbor.Encode(set); err != nil {
		return nil, err
	}
	return cbor.Encode(parts)
}
