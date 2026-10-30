package multisig

import (
	"errors"
	"fmt"

	"github.com/blinklabs-io/bursa"
	"github.com/blinklabs-io/bursa/bip32"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/btcsuite/btcd/btcutil/bech32"
)

// accountKeyPrefix is the CIP-5 bech32 prefix for a CIP-1854 extended
// account public key.
const accountKeyPrefix = "acct_shared_xvk"

// rolePayment is the CIP-1854 role for payment keys.
const rolePayment = 0

// maxIndex is the largest soft (non-hardened) derivation index.
const maxIndex = 0x7FFFFFFF

// ParseAccountKey decodes a bech32 "acct_shared_xvk" account public key.
func ParseAccountKey(s string) (bip32.XPub, error) {
	hrp, data, err := bech32.DecodeNoLimit(s)
	if err != nil {
		return nil, fmt.Errorf("invalid account key: %w", err)
	}
	if hrp != accountKeyPrefix {
		return nil, fmt.Errorf("invalid account key prefix %q, expected %q", hrp, accountKeyPrefix)
	}
	raw, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return nil, fmt.Errorf("invalid account key: %w", err)
	}
	if len(raw) != 64 {
		return nil, fmt.Errorf("invalid account key length %d, expected 64", len(raw))
	}
	return bip32.XPub(raw), nil
}

// EncodeAccountKey encodes an account public key as bech32 "acct_shared_xvk".
func EncodeAccountKey(xpub bip32.XPub) (string, error) {
	if len(xpub) != 64 {
		return "", fmt.Errorf("invalid account key length %d, expected 64", len(xpub))
	}
	return bech32.EncodeFromBase256(accountKeyPrefix, xpub)
}

// paymentKeyHash derives the payment key hash at role 0, index i from an
// account public key.
func paymentKeyHash(account bip32.XPub, index uint32) (common.Blake2b224, error) {
	role, err := account.Derive(rolePayment)
	if err != nil {
		return common.Blake2b224{}, err
	}
	child, err := role.Derive(index)
	if err != nil {
		return common.Blake2b224{}, err
	}
	return common.Blake2b224Hash(child.PublicKey()), nil
}

// Signer holds one cosigner's CIP-1854 account private key. It never leaves
// the cosigner's device; only AccountKey and the witnesses from Sign are
// shared.
type Signer struct {
	account bip32.XPrv
}

// NewSigner derives the CIP-1854 account key m/1854'/1815'/account' from a
// BIP-39 mnemonic.
func NewSigner(mnemonic string, account uint32) (*Signer, error) {
	if mnemonic == "" {
		return nil, errors.New("mnemonic is empty")
	}
	root, err := bursa.GetRootKeyFromMnemonic(mnemonic, "")
	if err != nil {
		return nil, fmt.Errorf("invalid mnemonic: %w", err)
	}
	acct, err := bursa.GetMultiSigAccountKey(root, account)
	if err != nil {
		return nil, err
	}
	return &Signer{account: acct}, nil
}

// AccountKey returns the bech32 account public key to share with the other
// cosigner and the backend.
func (s *Signer) AccountKey() string {
	key, err := EncodeAccountKey(s.account.Public())
	if err != nil {
		// The account key is always 64 bytes, so encoding cannot fail.
		panic(err)
	}
	return key
}

// paymentKey derives the payment signing key at role 0, index i.
func (s *Signer) paymentKey(index uint32) (bip32.XPrv, error) {
	return bursa.GetMultiSigPaymentKey(s.account, index)
}
