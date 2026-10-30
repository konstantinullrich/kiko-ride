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
	accounts [Cosigners]*bip32.ExtendedKey
}

// NewWallet creates a wallet from the three cosigners' account keys, in any
// order.
func NewWallet(network Network, accountKeyA, accountKeyB, accountKeyC string) (*Wallet, error) {
	var accounts [Cosigners]*bip32.ExtendedKey
	for i, key := range []string{accountKeyA, accountKeyB, accountKeyC} {
		xpub, err := ParseAccountKey(network, key)
		if err != nil {
			return nil, fmt.Errorf("cosigner %c: %w", 'A'+i, err)
		}
		accounts[i] = xpub
	}
	slices.SortFunc(accounts[:], func(a, b *bip32.ExtendedKey) int { return bytes.Compare([]byte(a.String()), []byte(b.String())) })
	for i := 1; i < Cosigners; i++ {
		if accounts[i-1].String() == accounts[i].String() {
			return nil, errors.New("two cosigners use the same account key")
		}
	}
	return &Wallet{network: network, accounts: accounts}, nil
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
