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
	"github.com/Salvionied/apollo/v2/backend/koios"
	"github.com/Salvionied/apollo/v2/constants"
	"github.com/bsv-blockchain/go-sdk/transaction/broadcaster"

	"kiko-backend/api"
	"kiko-backend/bsvmultisig"
	"kiko-backend/geo"
	"kiko-backend/multisig"
	"kiko-backend/x402"
	"kiko-backend/x402/bsvfacilitator"
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
//	KOIOS_URL                  Koios base URL for submitting payouts (defaults to the public instance for the network)
//	KOIOS_API_TOKEN            optional Koios bearer token; the public tier is used when unset
//	BSV_ARC_URL, BSV_ARC_API_KEY  ARC endpoint ending in /v1 (and optional key); set it to accept BSV.
//	                           Rider payments and driver payouts are broadcast through it.
//	BSV_FACILITATOR_URL        optional external x402 facilitator for BSV fares; by default the
//	                           built-in one (x402/bsvfacilitator) verifies and settles them
//
// BSV runs on mainnet when CARDANO_NETWORK is mainnet, otherwise on testnet.
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
	networks := []x402.NetworkConfig{{Network: network, Facilitator: facilitator(facilitatorURL)}}
	bsv, err := bsvConfig(network)
	if err != nil {
		return err
	}
	if bsv != nil {
		var f x402.Facilitator = &bsvfacilitator.Facilitator{Network: bsv.Network, Broadcaster: bsv.Broadcaster}
		if url := os.Getenv("BSV_FACILITATOR_URL"); url != "" {
			f = facilitator(url)
		}
		networks = append(networks, x402.NetworkConfig{Network: bsv.Network, Facilitator: f})
	}
	payments, err := x402.New(x402.Config{Networks: networks})
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

	chain, koiosURL, err := chainContext(string(network), walletNetwork)
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
		BSV:      bsv,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", server.Handler())
	mux.Handle("/mcp", server.MCPHandler())

	addr := envOr("KIKO_LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.LogRequests(slog.Default(), mux),
		ReadHeaderTimeout: 10 * time.Second,
		// Paid requests wait for settlement, which can take minutes.
		WriteTimeout: 4 * time.Minute,
	}
	slog.Info("listening", "addr", addr, "network", network, "bsv", bsv != nil, "db", dbPath, "facilitator", facilitatorURL,
		"koios", koiosURL, "cosigner", cosigner.AccountKey())
	return srv.ListenAndServe()
}

// chainContext connects to Koios, the public instance for the network unless
// KOIOS_URL is set.
func chainContext(network string, walletNetwork multisig.Network) (backend.ChainContext, string, error) {
	url := os.Getenv("KOIOS_URL")
	if url == "" {
		// network is "cardano:mainnet", "cardano:preprod" or "cardano:preview".
		switch strings.TrimPrefix(network, "cardano:") {
		case "mainnet":
			url = constants.KoiosBaseUrlMainnet
		case "preprod":
			url = constants.KoiosBaseUrlPreprod
		case "preview":
			url = constants.KoiosBaseUrlPreview
		default:
			return nil, "", fmt.Errorf("KOIOS_URL is required for network %q", network)
		}
	}
	return koios.NewKoiosChainContext(url, uint8(walletNetwork), os.Getenv("KOIOS_API_TOKEN")), url, nil
}

// bsvConfig enables BSV when BSV_ARC_URL is set, on mainnet alongside
// Cardano mainnet and on testnet otherwise.
func bsvConfig(cardano x402.Network) (*api.BSVConfig, error) {
	arcURL := os.Getenv("BSV_ARC_URL")
	if arcURL == "" {
		if os.Getenv("BSV_FACILITATOR_URL") != "" {
			return nil, errors.New("BSV_ARC_URL is required with BSV_FACILITATOR_URL")
		}
		return nil, nil
	}
	network, walletNetwork := x402.BSVTestnet, bsvmultisig.Testnet
	if cardano == x402.Mainnet {
		network, walletNetwork = x402.BSVMainnet, bsvmultisig.Mainnet
	}
	cosigner, err := bsvmultisig.NewServer(walletNetwork, os.Getenv("KIKO_SERVER_MNEMONIC"))
	if err != nil {
		return nil, fmt.Errorf("KIKO_SERVER_MNEMONIC: %w", err)
	}
	return &api.BSVConfig{
		Network:     network,
		Cosigner:    cosigner,
		Broadcaster: &broadcaster.Arc{ApiUrl: arcURL, ApiKey: os.Getenv("BSV_ARC_API_KEY"), Client: &http.Client{Timeout: time.Minute}},
	}, nil
}

func facilitator(url string) *x402.HTTPFacilitator {
	// Settlement waits for confirmations, which can take minutes.
	return &x402.HTTPFacilitator{URL: url, Client: &http.Client{Timeout: 3 * time.Minute}}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
