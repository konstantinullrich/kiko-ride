package multisig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	apollo "github.com/Salvionied/apollo/v2"
	"github.com/Salvionied/apollo/v2/backend"
	"github.com/blinklabs-io/bursa/bip32"
	"github.com/blinklabs-io/gouroboros/ledger/common"
)

// Network selects the address network.
type Network uint8

const (
	// Mainnet addresses start with "addr1".
	Mainnet Network = common.AddressNetworkMainnet
	// Preprod and Preview share the testnet address prefix "addr_test1".
	Preprod Network = common.AddressNetworkTestnet
	Preview Network = common.AddressNetworkTestnet
)

// DefaultGap is the CIP-1854 gap limit: discovery stops after this many
// consecutive addresses without UTxOs.
const DefaultGap = 20

// Cosigners is the number of cosigners in the wallet.
const Cosigners = 3

// Threshold is how many cosigners must sign a spend.
const Threshold = 2

// Wallet is a watch-only 2-of-3 multisig wallet built from the cosigners'
// account public keys. It derives addresses and builds unsigned transactions;
// it cannot sign.
type Wallet struct {
	network  Network
	accounts [Cosigners]bip32.XPub
}

// NewWallet creates a wallet from the three cosigners' "acct_shared_xvk"
// keys. The keys are sorted, so every party derives the same addresses
// regardless of the order they are passed in.
func NewWallet(network Network, accountKeyA, accountKeyB, accountKeyC string) (*Wallet, error) {
	var accounts [Cosigners]bip32.XPub
	for i, key := range []string{accountKeyA, accountKeyB, accountKeyC} {
		xpub, err := ParseAccountKey(key)
		if err != nil {
			return nil, fmt.Errorf("cosigner %c: %w", 'A'+i, err)
		}
		accounts[i] = xpub
	}
	slices.SortFunc(accounts[:], func(a, b bip32.XPub) int { return bytes.Compare(a, b) })
	for i := 1; i < Cosigners; i++ {
		if bytes.Equal(accounts[i-1], accounts[i]) {
			return nil, errors.New("two cosigners use the same account key")
		}
	}
	return &Wallet{network: network, accounts: accounts}, nil
}

// Address is one derived multisig address together with the script that
// locks it.
type Address struct {
	Index     uint32
	Address   common.Address
	Script    common.NativeScript
	KeyHashes [Cosigners]common.Blake2b224
}

// String returns the bech32 address.
func (a Address) String() string {
	return a.Address.String()
}

// Address derives the multisig address at the given index.
func (w *Wallet) Address(index uint32) (Address, error) {
	if index > maxIndex {
		return Address{}, fmt.Errorf("index %d out of range", index)
	}
	var hashes [Cosigners]common.Blake2b224
	sigs := make([]common.NativeScript, 0, len(w.accounts))
	for i, account := range w.accounts {
		hash, err := paymentKeyHash(account, index)
		if err != nil {
			return Address{}, err
		}
		sig, err := apollo.NewNativeScriptPubkey(hash)
		if err != nil {
			return Address{}, err
		}
		hashes[i] = hash
		sigs = append(sigs, sig)
	}
	script, err := apollo.NewNativeScriptNofK(Threshold, sigs)
	if err != nil {
		return Address{}, err
	}
	scriptHash := script.Hash()
	addr, err := common.NewAddressFromParts(
		common.AddressTypeScriptNone,
		uint8(w.network),
		scriptHash.Bytes(),
		nil,
	)
	if err != nil {
		return Address{}, err
	}
	return Address{Index: index, Address: addr, Script: script, KeyHashes: hashes}, nil
}

// FundedAddress is a derived address and the UTxOs currently sitting at it.
type FundedAddress struct {
	Address
	Utxos []common.Utxo
}

// Discover scans addresses from index 0 and returns those holding UTxOs,
// stopping after gap consecutive empty addresses. A gap of 0 uses DefaultGap.
//
// Only unspent outputs are visible, so an address that was used and then
// emptied counts as empty.
func (w *Wallet) Discover(ctx context.Context, chain backend.ChainContext, gap uint32) ([]FundedAddress, error) {
	if gap == 0 {
		gap = DefaultGap
	}
	var funded []FundedAddress
	empty := uint32(0)
	for index := uint32(0); empty < gap && index <= maxIndex; index++ {
		addr, err := w.Address(index)
		if err != nil {
			return nil, err
		}
		utxos, err := backend.UtxosContext(ctx, chain, addr.Address)
		if err != nil {
			return nil, fmt.Errorf("load UTxOs for index %d: %w", index, err)
		}
		if len(utxos) == 0 {
			empty++
			continue
		}
		empty = 0
		funded = append(funded, FundedAddress{Address: addr, Utxos: utxos})
	}
	return funded, nil
}

// funded loads the UTxOs at the given indexes, skipping empty addresses.
func (w *Wallet) funded(ctx context.Context, chain backend.ChainContext, indexes []uint32) ([]FundedAddress, error) {
	var funded []FundedAddress
	for _, index := range indexes {
		addr, err := w.Address(index)
		if err != nil {
			return nil, err
		}
		utxos, err := backend.UtxosContext(ctx, chain, addr.Address)
		if err != nil {
			return nil, fmt.Errorf("load UTxOs for index %d: %w", index, err)
		}
		if len(utxos) > 0 {
			funded = append(funded, FundedAddress{Address: addr, Utxos: utxos})
		}
	}
	return funded, nil
}
