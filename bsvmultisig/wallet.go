package bsvmultisig

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	bip32 "github.com/bsv-blockchain/go-sdk/compat/bip32"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	chaincfg "github.com/bsv-blockchain/go-sdk/transaction/chaincfg"
)

// Network selects the extended key prefixes.
type Network uint8

const (
	// Mainnet account keys start with "xpub".
	Mainnet Network = iota
	// Testnet account keys start with "tpub".
	Testnet
)

func (n Network) params() *chaincfg.Params {
	if n == Mainnet {
		return &chaincfg.MainNet
	}
	return &chaincfg.TestNet
}

func (n Network) String() string {
	if n == Mainnet {
		return "mainnet"
	}
	return "testnet"
}

// DefaultGap is the discovery gap limit: discovery stops after this many
// consecutive scripts without UTXOs.
const DefaultGap = 20

// Cosigners is the number of cosigners in the wallet.
const Cosigners = 3

// Threshold is how many cosigners must sign a spend.
const Threshold = 2

// UTXO is an unspent output locked by one of the wallet's scripts.
type UTXO struct {
	TxID     string
	Vout     uint32
	Satoshis uint64
}

// Chain finds the UTXOs locked by a script, e.g. an indexer.
type Chain interface {
	Unspent(ctx context.Context, lockingScript *script.Script) ([]UTXO, error)
}

// Wallet is a watch-only 2-of-3 multisig wallet built from the cosigners'
// account public keys. It derives locking scripts and builds unsigned
// transactions; it cannot sign.
type Wallet struct {
	network  Network
	accounts []*bip32.ExtendedKey
	// key, when set, is the third cosigner's key at every index.
	key *ec.PublicKey
	// exclude holds accounts whose key at an index must not be key there.
	exclude []*bip32.ExtendedKey
}

// NewWallet creates a wallet from the three cosigners' account keys, in any
// order.
func NewWallet(network Network, accountKeyA, accountKeyB, accountKeyC string) (*Wallet, error) {
	accounts, err := parseAccounts(network, accountKeyA, accountKeyB, accountKeyC)
	if err != nil {
		return nil, err
	}
	return &Wallet{network: network, accounts: accounts}, nil
}

// NewKeyWallet creates a wallet from two cosigners' account keys and a
// third cosigner's fixed public key (compressed, hex), which locks every
// index alongside the two derived keys.
func NewKeyWallet(network Network, accountKeyA, accountKeyB, key string) (*Wallet, error) {
	accounts, err := parseAccounts(network, accountKeyA, accountKeyB)
	if err != nil {
		return nil, err
	}
	pub, err := ParseKey(key)
	if err != nil {
		return nil, err
	}
	return &Wallet{network: network, accounts: accounts, key: pub}, nil
}

func parseAccounts(network Network, keys ...string) ([]*bip32.ExtendedKey, error) {
	accounts := make([]*bip32.ExtendedKey, len(keys))
	for i, key := range keys {
		xpub, err := ParseAccountKey(network, key)
		if err != nil {
			return nil, fmt.Errorf("cosigner %c: %w", 'A'+i, err)
		}
		accounts[i] = xpub
	}
	for i, a := range accounts {
		for _, b := range accounts[i+1:] {
			if a.String() == b.String() {
				return nil, errors.New("two cosigners use the same account key")
			}
		}
	}
	return accounts, nil
}

// Address is one derived multisig locking script with its keys, in script
// order.
type Address struct {
	Index         uint32
	LockingScript *script.Script
	PubKeys       [Cosigners]*ec.PublicKey
}

// String returns the locking script hex.
func (a Address) String() string {
	return hex.EncodeToString(a.LockingScript.Bytes())
}

// keyPosition returns where pub sits in the script, or -1.
func (a Address) keyPosition(pub *ec.PublicKey) int {
	return slices.IndexFunc(a.PubKeys[:], func(k *ec.PublicKey) bool { return bytes.Equal(k.Compressed(), pub.Compressed()) })
}

// Address derives the multisig locking script at the given index.
func (w *Wallet) Address(index uint32) (Address, error) {
	if index > maxIndex {
		return Address{}, fmt.Errorf("index %d out of range", index)
	}
	var keys [Cosigners]*ec.PublicKey
	for i, account := range w.accounts {
		pub, err := paymentPubKey(account, index)
		if err != nil {
			return Address{}, err
		}
		keys[i] = pub
	}
	if w.key != nil {
		same := func(k *ec.PublicKey) bool { return k != nil && bytes.Equal(k.Compressed(), w.key.Compressed()) }
		if slices.ContainsFunc(keys[:len(w.accounts)], same) {
			return Address{}, fmt.Errorf("fixed key is another cosigner's key at index %d", index)
		}
		for _, account := range w.exclude {
			if own, err := paymentPubKey(account, index); err != nil || same(own) {
				return Address{}, fmt.Errorf("fixed key is an excluded key at index %d", index)
			}
		}
		keys[Cosigners-1] = w.key
	}
	slices.SortFunc(keys[:], func(a, b *ec.PublicKey) int { return bytes.Compare(a.Compressed(), b.Compressed()) })
	s := &script.Script{}
	if err := s.AppendOpcodes(script.Op1 + Threshold - 1); err != nil {
		return Address{}, err
	}
	for _, k := range keys {
		if err := s.AppendPushData(k.Compressed()); err != nil {
			return Address{}, err
		}
	}
	if err := s.AppendOpcodes(script.Op1+Cosigners-1, script.OpCHECKMULTISIG); err != nil {
		return Address{}, err
	}
	return Address{Index: index, LockingScript: s, PubKeys: keys}, nil
}

// FundedAddress is a derived address and the UTXOs currently locked by it.
type FundedAddress struct {
	Address
	Utxos []UTXO
}

// Discover scans addresses from index 0 and returns those holding UTXOs,
// stopping after gap consecutive empty addresses. A gap of 0 uses DefaultGap.
func (w *Wallet) Discover(ctx context.Context, chain Chain, gap uint32) ([]FundedAddress, error) {
	if gap == 0 {
		gap = DefaultGap
	}
	var funded []FundedAddress
	empty := uint32(0)
	for index := uint32(0); empty < gap && index <= maxIndex; index++ {
		f, err := w.load(ctx, chain, index)
		if err != nil {
			return nil, err
		}
		if len(f.Utxos) == 0 {
			empty++
			continue
		}
		empty = 0
		funded = append(funded, f)
	}
	return funded, nil
}

// funded loads the UTXOs at the given indexes, skipping empty addresses.
func (w *Wallet) funded(ctx context.Context, chain Chain, indexes []uint32) ([]FundedAddress, error) {
	var funded []FundedAddress
	for _, index := range indexes {
		f, err := w.load(ctx, chain, index)
		if err != nil {
			return nil, err
		}
		if len(f.Utxos) > 0 {
			funded = append(funded, f)
		}
	}
	return funded, nil
}

func (w *Wallet) load(ctx context.Context, chain Chain, index uint32) (FundedAddress, error) {
	addr, err := w.Address(index)
	if err != nil {
		return FundedAddress{}, err
	}
	utxos, err := chain.Unspent(ctx, addr.LockingScript)
	if err != nil {
		return FundedAddress{}, fmt.Errorf("load UTXOs for index %d: %w", index, err)
	}
	return FundedAddress{Address: addr, Utxos: utxos}, nil
}
