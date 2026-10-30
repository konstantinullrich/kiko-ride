package bsvmultisig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	bip32 "github.com/bsv-blockchain/go-sdk/compat/bip32"
	"github.com/bsv-blockchain/go-sdk/compat/bip39"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

const (
	// purpose and coinType are the hardened levels above the account:
	// m/48'/236'/account'.
	purpose  = 48
	coinType = 236 // SLIP-44 Bitcoin SV
	// accountDepth is the depth of an account key below the master key.
	accountDepth = 3
	// rolePayment is the non-hardened chain below the account.
	rolePayment = 0
)

// maxIndex is the largest soft (non-hardened) derivation index.
const maxIndex = bip32.HardenedKeyStart - 1

// ParseAccountKey decodes an account extended public key for network.
func ParseAccountKey(network Network, s string) (*bip32.ExtendedKey, error) {
	key, err := bip32.NewKeyFromString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid account key: %w", err)
	}
	if key.IsPrivate() {
		return nil, errors.New("account key is private; share the extended public key")
	}
	if !key.IsForNet(network.params()) {
		return nil, fmt.Errorf("account key is not for %s", network)
	}
	if key.Depth() != accountDepth {
		return nil, fmt.Errorf("account key has depth %d, expected %d (m/%d'/%d'/account')", key.Depth(), accountDepth, purpose, coinType)
	}
	return key, nil
}

// ParseKey decodes a compressed public key in hex.
func ParseKey(s string) (*ec.PublicKey, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 33 {
		return nil, errors.New("key must be a compressed public key: 33 bytes in hex")
	}
	pub, err := ec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid key: %w", err)
	}
	return pub, nil
}

// paymentPubKey derives the public key at role 0, index i of an account.
func paymentPubKey(account *bip32.ExtendedKey, index uint32) (*ec.PublicKey, error) {
	role, err := account.Child(rolePayment)
	if err != nil {
		return nil, err
	}
	child, err := role.Child(index)
	if err != nil {
		return nil, err
	}
	return child.ECPubKey()
}

// Signer holds one cosigner's account private key. It never leaves the
// cosigner's device; only AccountKey and the witnesses from Sign are shared.
type Signer struct {
	network Network
	account *bip32.ExtendedKey
}

// NewSigner derives the account key m/48'/236'/account' from a BIP-39
// mnemonic.
func NewSigner(network Network, mnemonic string, account uint32) (*Signer, error) {
	if mnemonic == "" {
		return nil, errors.New("mnemonic is empty")
	}
	seed, err := bip39.NewSeedWithErrorChecking(mnemonic, "")
	if err != nil {
		return nil, fmt.Errorf("invalid mnemonic: %w", err)
	}
	key, err := bip32.NewMaster(seed, network.params())
	if err != nil {
		return nil, err
	}
	for _, i := range []uint32{purpose, coinType, account} {
		if key, err = key.Child(bip32.HardenedKeyStart + i); err != nil {
			return nil, err
		}
	}
	return &Signer{network: network, account: key}, nil
}

// AccountKey returns the account extended public key to share with the
// other cosigners and the backend.
func (s *Signer) AccountKey() string {
	pub, err := s.account.Neuter()
	if err != nil {
		// A private key always has a public counterpart.
		panic(err)
	}
	return pub.String()
}

// Key returns the signer's compressed public key at role 0, index i in hex,
// for use as a fixed key in NewKeyWallet.
func (s *Signer) Key(index uint32) (string, error) {
	key, err := s.paymentKey(index)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key.PubKey().Compressed()), nil
}

// paymentKey derives the signing key at role 0, index i.
func (s *Signer) paymentKey(index uint32) (*ec.PrivateKey, error) {
	role, err := s.account.Child(rolePayment)
	if err != nil {
		return nil, err
	}
	child, err := role.Child(index)
	if err != nil {
		return nil, err
	}
	return child.ECPrivKey()
}

// SignMessage signs the SHA-256 of msg with the account key itself, to
// authenticate the cosigner off chain, returning a DER signature.
// Transactions are only ever signed with the keys derived below it, so
// these signatures cannot unlock a ride address.
func (s *Signer) SignMessage(msg []byte) ([]byte, error) {
	priv, err := s.account.ECPrivKey()
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(msg)
	sig, err := priv.Sign(hash[:])
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}

// VerifyMessage checks that sig is SignMessage's signature over msg by the
// holder of accountKey on network.
func VerifyMessage(network Network, accountKey string, msg, sig []byte) error {
	key, err := ParseAccountKey(network, accountKey)
	if err != nil {
		return err
	}
	pub, err := key.ECPubKey()
	if err != nil {
		return err
	}
	parsed, err := ec.ParseDERSignature(sig)
	if err != nil {
		return fmt.Errorf("invalid signature: %w", err)
	}
	hash := sha256.Sum256(msg)
	if !parsed.Verify(hash[:], pub) {
		return errors.New("signature does not verify")
	}
	return nil
}
