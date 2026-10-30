package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Salvionied/apollo/v2/backend"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"kiko-backend/bsvmultisig"
	"kiko-backend/multisig"
	"kiko-backend/x402"
)

// Chain is the blockchain a driver is paid on, decided by the format of the
// public key they register with.
type Chain string

const (
	// ChainCardano keys are CIP-1854 "acct_shared_xvk1…"; fares are in
	// lovelace.
	ChainCardano Chain = "cardano"
	// ChainBSV keys are BIP-32 account keys "xpub…" or "tpub…"; fares are
	// in satoshis.
	ChainBSV Chain = "bsv"
)

// MinFareSatoshi is the lowest BSV fare charged.
const MinFareSatoshi = 1

// BSVConfig enables Bitcoin SV drivers and riders.
type BSVConfig struct {
	// Network is x402.BSVMainnet or x402.BSVTestnet.
	Network x402.Network
	// Cosigner is Kiko's BSV server cosigner.
	Cosigner *bsvmultisig.Server
	// Broadcaster submits driver payouts, e.g. an ARC endpoint.
	Broadcaster transaction.Broadcaster
}

// errChainDisabled is returned for BSV keys when BSV is not configured.
var errChainDisabled = errors.New("BSV is not enabled on this server")

// parseKey classifies a cosigner key by its format and normalizes it:
// bech32 is case insensitive, so Cardano keys are lowercased, while base58
// BSV keys are kept as given.
func parseKey(key string) (Chain, string, error) {
	key = strings.TrimSpace(key)
	switch {
	case strings.HasPrefix(strings.ToLower(key), "acct_shared_xvk"):
		return ChainCardano, strings.ToLower(key), nil
	case strings.HasPrefix(key, "xpub"), strings.HasPrefix(key, "tpub"):
		return ChainBSV, key, nil
	}
	return "", key, errors.New(`must be a Cardano "acct_shared_xvk1…" or a BSV "xpub…"/"tpub…" account key`)
}

// normalizeKey normalizes a key for lookup, leaving unknown formats as
// they are.
func normalizeKey(key string) string {
	_, key, _ = parseKey(key)
	return key
}

// checkCosignerKey parses a driver's or rider's key and checks it with the
// chain's server cosigner.
func (s *Server) checkCosignerKey(key string) (Chain, string, error) {
	chain, key, err := parseKey(key)
	if err != nil {
		return "", key, err
	}
	switch chain {
	case ChainCardano:
		err = s.cfg.Cosigner.CheckCosignerKey(key)
	case ChainBSV:
		if s.cfg.BSV == nil {
			return "", key, errChainDisabled
		}
		err = s.cfg.BSV.Cosigner.CheckCosignerKey(key)
	}
	return chain, key, err
}

// network returns the x402 network fares on chain are paid on.
func (s *Server) network(chain Chain) x402.Network {
	if chain == ChainBSV && s.cfg.BSV != nil {
		return s.cfg.BSV.Network
	}
	return s.cfg.Network
}

// rideAddress derives where a ride on chain is paid and the script the
// driver needs to spend it: a bech32 address and hex CBOR native script on
// Cardano; on BSV the hex bare-multisig locking script for both.
func (s *Server) rideAddress(chain Chain, driverKey, riderKey string, index uint32) (payTo, script string, err error) {
	switch chain {
	case ChainCardano:
		wallet, _, err := s.cfg.Cosigner.RideWallet(driverKey, riderKey)
		if err != nil {
			return "", "", err
		}
		addr, err := wallet.Address(index)
		if err != nil {
			return "", "", err
		}
		raw, err := cbor.Encode(&addr.Script)
		if err != nil {
			return "", "", err
		}
		return addr.String(), hex.EncodeToString(raw), nil
	case ChainBSV:
		if s.cfg.BSV == nil {
			return "", "", errChainDisabled
		}
		wallet, _, err := s.cfg.BSV.Cosigner.RideWallet(driverKey, riderKey)
		if err != nil {
			return "", "", err
		}
		addr, err := wallet.Address(index)
		if err != nil {
			return "", "", err
		}
		return addr.String(), addr.String(), nil
	}
	return "", "", fmt.Errorf("unknown chain %q", chain)
}

// errMissingSignature is returned by cosignPayout when the driver has not
// signed the payout.
var errMissingSignature = errors.New("transaction must be signed by the driver first")

// cosignPayout adds the server's signature to the driver's payout of ride,
// returning the final transaction id, the signed transaction as hex and a
// function that submits it.
// Errors that are the driver's to fix are errMissingSignature or wrap
// errBadPayout or errCannotCosign.
func (s *Server) cosignPayout(ride Ride, txHex string) (txID, signedHex string, submit func(context.Context) error, err error) {
	switch ride.Chain {
	case ChainCardano:
		txCbor, err := hex.DecodeString(txHex)
		if err != nil || len(txCbor) == 0 {
			return "", "", nil, fmt.Errorf("%w: transaction must be hex CBOR", errBadPayout)
		}
		wallet, _, err := s.cfg.Cosigner.RideWallet(ride.DriverKey, ride.RiderKey)
		if err != nil {
			return "", "", nil, err
		}
		signed, txID, err := s.cfg.Cosigner.Cosign(wallet, ride.AddressIndex, ride.DriverKey, txCbor)
		if errors.Is(err, multisig.ErrMissingSignature) {
			return "", "", nil, errMissingSignature
		} else if err != nil {
			return "", "", nil, fmt.Errorf("%w: %w", errCannotCosign, err)
		}
		return txID.String(), hex.EncodeToString(signed), func(ctx context.Context) error {
			_, err := backend.SubmitTxContext(ctx, s.cfg.Chain, signed)
			return err
		}, nil
	case ChainBSV:
		if s.cfg.BSV == nil {
			return "", "", nil, errChainDisabled
		}
		if _, err := hex.DecodeString(txHex); err != nil || txHex == "" {
			return "", "", nil, fmt.Errorf("%w: transaction must be hex Extended Format", errBadPayout)
		}
		wallet, _, err := s.cfg.BSV.Cosigner.RideWallet(ride.DriverKey, ride.RiderKey)
		if err != nil {
			return "", "", nil, err
		}
		signed, txID, err := s.cfg.BSV.Cosigner.Cosign(wallet, ride.AddressIndex, ride.DriverKey, txHex)
		if errors.Is(err, bsvmultisig.ErrMissingSignature) {
			return "", "", nil, errMissingSignature
		} else if err != nil {
			return "", "", nil, fmt.Errorf("%w: %w", errCannotCosign, err)
		}
		return txID, signed.Hex(), func(ctx context.Context) error {
			if _, fail := s.cfg.BSV.Broadcaster.BroadcastCtx(ctx, signed); fail != nil {
				return fmt.Errorf("%s %s", fail.Code, fail.Description)
			}
			return nil
		}, nil
	}
	return "", "", nil, fmt.Errorf("unknown chain %q", ride.Chain)
}

var (
	errBadPayout    = errors.New("bad payout")
	errCannotCosign = errors.New("cannot cosign")
)
