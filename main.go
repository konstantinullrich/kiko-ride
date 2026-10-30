package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Salvionied/apollo/v2/backend"
	"github.com/Salvionied/apollo/v2/backend/blockfrost"
	"github.com/Salvionied/apollo/v2/backend/ogmios"

	"kiko-backend/api"
	"kiko-backend/geo"
	"kiko-backend/multisig"
	"kiko-backend/x402"
)

// cfPreprodFacilitator is the Cardano Foundation's hosted preprod facilitator.
const cfPreprodFacilitator = "https://x402.preprod.dev.ecosyseng.cf-deployments.org"

// Environment:
//
//	KIKO_LISTEN_ADDR           default ":8080"
//	CARDANO_NETWORK            mainnet | preprod (default) | preview
//	X402_FACILITATOR_URL       defaults to the Cardano Foundation facilitator on preprod
//	KIKO_SERVER_MNEMONIC       BIP-39 mnemonic of Kiko's server cosigner keys
//	KIKO_DB_PATH               SQLite database of drivers and rides (default "kiko.db")
//	MAPBOX_ACCESS_TOKEN        Mapbox token for geocoding
//	MAPBOX_PERMANENT           "true" for permanent geocoding (needed to store results)
//	OGMIOS_URL, KUPO_URL       chain backend for submitting payouts (KUPO_URL optional), or
//	BLOCKFROST_PROJECT_ID      Blockfrost instead; BLOCKFROST_URL overrides its default base URL
func main() {
	if err := run(); err != nil {
		slog.Error("kiko-backend", "err", err)
		os.Exit(1)
	}
}

func run() error {
	network := x402.Network("cardano:" + envOr("CARDANO_NETWORK", "preprod"))
	walletNetwork := multisig.Preprod
	if network == x402.Mainnet {
		walletNetwork = multisig.Mainnet
	}

	facilitatorURL := os.Getenv("X402_FACILITATOR_URL")
	if facilitatorURL == "" {
		if network != x402.Preprod {
			return errors.New("X402_FACILITATOR_URL is required outside preprod")
		}
		facilitatorURL = cfPreprodFacilitator
	}
	payments, err := x402.New(x402.Config{
		Network: network,
		Facilitator: &x402.HTTPFacilitator{
			URL:    facilitatorURL,
			Client: &http.Client{Timeout: 3 * time.Minute}, // settlement waits for confirmations
		},
	})
	if err != nil {
		return err
	}

	cosigner, err := multisig.NewServer(walletNetwork, os.Getenv("KIKO_SERVER_MNEMONIC"))
	if err != nil {
		return fmt.Errorf("KIKO_SERVER_MNEMONIC: %w", err)
	}
	dbPath := envOr("KIKO_DB_PATH", "kiko.db")
	db, err := api.OpenDB(dbPath)
	if err != nil {
		return fmt.Errorf("KIKO_DB_PATH: %w", err)
	}
	defer db.Close()

	chain, err := chainContext(string(network), walletNetwork)
	if err != nil {
		return err
	}

	token := os.Getenv("MAPBOX_ACCESS_TOKEN")
	if token == "" {
		return errors.New("MAPBOX_ACCESS_TOKEN is required")
	}
	geocoder := &geo.Mapbox{
		Token:     token,
		Permanent: os.Getenv("MAPBOX_PERMANENT") == "true",
		Client:    &http.Client{Timeout: 10 * time.Second},
	}

	server, err := api.New(api.Config{
		Network:  network,
		Cosigner: cosigner,
		DB:       db,
		Geocoder: geocoder,
		Payments: payments,
		Chain:    chain,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", server.Handler())

	addr := envOr("KIKO_LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Paid requests wait for settlement, which can take minutes.
		WriteTimeout: 4 * time.Minute,
	}
	slog.Info("listening", "addr", addr, "network", network, "db", dbPath, "facilitator", facilitatorURL,
		"cosigner", cosigner.AccountKey())
	return srv.ListenAndServe()
}

// chainContext connects to Ogmios when OGMIOS_URL is set, otherwise to
// Blockfrost.
func chainContext(network string, walletNetwork multisig.Network) (backend.ChainContext, error) {
	if url := os.Getenv("OGMIOS_URL"); url != "" {
		chain, err := ogmios.NewOgmiosChainContext(ogmios.Config{
			OgmiosEndpoint: url,
			KupoEndpoint:   os.Getenv("KUPO_URL"),
			NetworkId:      uint8(walletNetwork),
		})
		if err != nil {
			return nil, fmt.Errorf("OGMIOS_URL: %w", err)
		}
		return chain, nil
	}
	projectID := os.Getenv("BLOCKFROST_PROJECT_ID")
	if projectID == "" {
		return nil, errors.New("OGMIOS_URL or BLOCKFROST_PROJECT_ID is required")
	}
	// network is "cardano:mainnet", "cardano:preprod" or "cardano:preview".
	name := strings.TrimPrefix(network, "cardano:")
	url := envOr("BLOCKFROST_URL", "https://cardano-"+name+".blockfrost.io/api/v0")
	return blockfrost.NewBlockFrostChainContext(url, uint8(walletNetwork), projectID), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
