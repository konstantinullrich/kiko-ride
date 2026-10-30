package x402

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	apollo "github.com/Salvionied/apollo/v2"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"
	base58 "github.com/bsv-blockchain/go-sdk/compat/base58"
	crypto "github.com/bsv-blockchain/go-sdk/primitives/hash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// chain holds what the middleware checks itself for one network; everything
// else is the facilitator's job.
type chain interface {
	// asset is the id of the network's native coin, which prices are in.
	asset() string
	checkPayTo(payTo string) error
	// transactionID returns the canonical id of a base64 payload
	// transaction, keying duplicate-settlement protection.
	transactionID(b64 string) (string, error)
}

func chainFor(n Network) (chain, error) {
	switch n {
	case Mainnet:
		return cardano{network: n, addressNet: common.AddressNetworkMainnet}, nil
	case Preprod, Preview:
		return cardano{network: n, addressNet: common.AddressNetworkTestnet}, nil
	case BSVMainnet:
		return bsv{network: n, p2pkhVersion: 0x00}, nil
	case BSVTestnet:
		return bsv{network: n, p2pkhVersion: 0x6f}, nil
	}
	return nil, fmt.Errorf("unsupported network %q", n)
}

func decodeTransaction(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("payload.transaction is not base64: %w", err)
	}
	return raw, nil
}

type cardano struct {
	network    Network
	addressNet uint
}

func (cardano) asset() string { return Lovelace }

func (c cardano) checkPayTo(payTo string) error {
	addr, err := apollo.ParseAddress(payTo)
	if err != nil {
		return err
	}
	if addr.NetworkId() != c.addressNet {
		return fmt.Errorf("payTo %s is not a %s address", payTo, c.network)
	}
	return nil
}

// transactionID hashes the body of a Conway transaction.
func (cardano) transactionID(b64 string) (string, error) {
	raw, err := decodeTransaction(b64)
	if err != nil {
		return "", err
	}
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(raw, &tx); err != nil {
		return "", fmt.Errorf("payload.transaction is not a Conway transaction: %w", err)
	}
	return tx.Body.Id().String(), nil
}

type bsv struct {
	network      Network
	p2pkhVersion byte
}

func (bsv) asset() string { return Satoshi }

// checkPayTo accepts a P2PKH address of the network, or a locking script in
// hex for outputs without an address, such as bare multisig.
func (b bsv) checkPayTo(payTo string) error {
	if raw, err := hex.DecodeString(payTo); err == nil && len(raw) > 0 {
		if _, err := script.NewFromBytes(raw).Chunks(); err != nil {
			return fmt.Errorf("payTo is not a valid locking script: %w", err)
		}
		return nil
	}
	decoded, err := base58.Decode(payTo)
	if err != nil || len(decoded) != 25 {
		return fmt.Errorf("payTo %s is neither a P2PKH address nor a hex locking script", payTo)
	}
	sum := crypto.Sha256d(decoded[:21])
	if !bytes.Equal(sum[:4], decoded[21:]) {
		return fmt.Errorf("payTo %s has a bad checksum", payTo)
	}
	if decoded[0] != b.p2pkhVersion {
		return fmt.Errorf("payTo %s is not a %s address", payTo, b.network)
	}
	return nil
}

// transactionID hashes a raw or Extended Format transaction; both give the
// same id.
func (bsv) transactionID(b64 string) (string, error) {
	raw, err := decodeTransaction(b64)
	if err != nil {
		return "", err
	}
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return "", fmt.Errorf("payload.transaction is not a BSV transaction: %w", err)
	}
	if len(tx.Inputs) == 0 {
		return "", errors.New("payload.transaction has no inputs")
	}
	return tx.TxID().String(), nil
}
