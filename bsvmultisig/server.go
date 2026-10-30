package bsvmultisig

import (
	"bytes"
	"errors"
	"fmt"

	bip32 "github.com/bsv-blockchain/go-sdk/compat/bip32"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// ErrMissingSignature is returned by Cosign when the counterparty has not
// signed every input yet.
var ErrMissingSignature = errors.New("transaction is not signed by the cosigner")

const (
	// serverAccount is the account of the key Kiko adds to every ride.
	serverAccount = 0
	// backupAccount is the account of the key Kiko adds in place of a rider
	// who brings no key.
	backupAccount = 1
)

// Server is the cosigner run by the Kiko backend, as in multisig.Server:
//
//   - account 0 cosigns every ride wallet,
//   - account 1 stands in for a rider who does not provide a key.
type Server struct {
	network Network
	primary *Signer
	backup  *Signer
}

// NewServer derives the server's two cosigner accounts from a BIP-39
// mnemonic.
func NewServer(network Network, mnemonic string) (*Server, error) {
	primary, err := NewSigner(network, mnemonic, serverAccount)
	if err != nil {
		return nil, err
	}
	backup, err := NewSigner(network, mnemonic, backupAccount)
	if err != nil {
		return nil, err
	}
	return &Server{network: network, primary: primary, backup: backup}, nil
}

// AccountKey returns the server's own account key, present in every ride
// wallet.
func (s *Server) AccountKey() string {
	return s.primary.AccountKey()
}

// Network returns the network the server's keys are for.
func (s *Server) Network() Network {
	return s.network
}

// CheckCosignerKey reports whether key is acceptable as a driver's or
// rider's cosigner key: a valid account key that is not one of the server's
// own.
func (s *Server) CheckCosignerKey(key string) error {
	xpub, err := ParseAccountKey(s.network, key)
	if err != nil {
		return err
	}
	for _, own := range []*Signer{s.primary, s.backup} {
		if xpub.String() == own.AccountKey() {
			return errors.New("key belongs to the Kiko server")
		}
	}
	return nil
}

// RideWallet returns the 2-of-3 wallet of the server, the driver and the
// rider. riderKey is the rider's own compressed public key for the ride, in
// hex, used at every index as is. If riderKey is empty the server's backup
// key takes the rider's place and custodial is true.
func (s *Server) RideWallet(driverKey, riderKey string) (w *Wallet, custodial bool, err error) {
	if err := s.CheckCosignerKey(driverKey); err != nil {
		return nil, false, fmt.Errorf("driver key: %w", err)
	}
	if riderKey == "" {
		w, err = NewWallet(s.network, s.primary.AccountKey(), driverKey, s.backup.AccountKey())
		return w, true, err
	}
	w, err = NewKeyWallet(s.network, s.primary.AccountKey(), driverKey, riderKey)
	if err != nil {
		return nil, false, fmt.Errorf("rider key: %w", err)
	}
	// A rider key equal to the backup key would hand the server a second key
	// on the ride.
	w.exclude = []*bip32.ExtendedKey{s.backup.account}
	return w, false, nil
}

// Sign returns the server's witnesses for a proposal from a ride wallet: the
// primary key's, plus the backup key's when the wallet is custodial.
//
// Like Signer.Sign it does not judge the payment itself.
func (s *Server) Sign(p *Proposal) ([]Witness, error) {
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
// script at index and that cosignerKey has already signed, returning the
// signed transaction and its id.
//
// tx is hex Extended Format. Every input must spend the script at index and
// carry the unlocking script OP_0 <signature>, the signature being
// cosignerKey's ALL|FORKID signature at index. The server never gives the
// first signature, and only its primary key signs, so a custodial wallet's
// backup key is never spent here.
//
// The counterparty must have paid the fee for the server's signature.
// Adding it changes the transaction id.
func (s *Server) Cosign(w *Wallet, index uint32, cosignerKey, txEF string) (*transaction.Transaction, string, error) {
	tx, err := decodeEF(txEF)
	if err != nil {
		return nil, "", err
	}
	if len(tx.Inputs) == 0 {
		return nil, "", errors.New("transaction has no inputs")
	}
	addr, err := w.Address(index)
	if err != nil {
		return nil, "", err
	}
	account, err := ParseAccountKey(s.network, cosignerKey)
	if err != nil {
		return nil, "", err
	}
	cosigner, err := paymentPubKey(account, index)
	if err != nil {
		return nil, "", err
	}
	key, err := s.primary.paymentKey(index)
	if err != nil {
		return nil, "", err
	}
	server := key.PubKey()
	cosignerPos, serverPos := addr.keyPosition(cosigner), addr.keyPosition(server)
	if cosignerPos < 0 || serverPos < 0 || cosignerPos == serverPos {
		return nil, "", fmt.Errorf("index %d is not cosigned by the server and %s", index, cosignerKey)
	}

	for i, in := range tx.Inputs {
		if !bytes.Equal(in.SourceTxScript().Bytes(), addr.LockingScript.Bytes()) {
			return nil, "", fmt.Errorf("input %d does not spend the script of index %d", i, index)
		}
		sig, err := singleSignature(in.UnlockingScript)
		if err != nil {
			return nil, "", fmt.Errorf("input %d: %w", i, err)
		}
		if !verifyInput(tx, i, cosigner, sig) {
			return nil, "", fmt.Errorf("input %d: %w", i, ErrMissingSignature)
		}
		own, err := signInput(tx, i, key)
		if err != nil {
			return nil, "", err
		}
		var sigs [Cosigners][]byte
		sigs[cosignerPos], sigs[serverPos] = sig, own
		if in.UnlockingScript, err = unlockingScript(sigs); err != nil {
			return nil, "", err
		}
	}
	return tx, tx.TxID().String(), nil
}

// singleSignature returns the signature of a partial unlocking script
// OP_0 <signature>.
func singleSignature(unlock *script.Script) ([]byte, error) {
	if unlock == nil || len(*unlock) == 0 {
		return nil, ErrMissingSignature
	}
	chunks, err := unlock.Chunks()
	if err != nil {
		return nil, err
	}
	if len(chunks) != 2 || chunks[0].Op != script.Op0 || len(chunks[1].Data) == 0 {
		return nil, errors.New("unlocking script must be OP_0 <signature>")
	}
	return chunks[1].Data, nil
}
