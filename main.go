package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

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
//	KIKO_FIRST_ADDRESS_INDEX   address index for the first ride (default 0)
//	MAPBOX_ACCESS_TOKEN        Mapbox token for geocoding
//	MAPBOX_PERMANENT           "true" for permanent geocoding (needed to store results)
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
	firstIndex, err := strconv.ParseUint(envOr("KIKO_FIRST_ADDRESS_INDEX", "0"), 10, 32)
	if err != nil {
		return fmt.Errorf("KIKO_FIRST_ADDRESS_INDEX: %w", err)
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
		Network:           network,
		Cosigner:          cosigner,
		FirstAddressIndex: uint32(firstIndex),
		Geocoder:          geocoder,
		Payments:          payments,
	})
	if err != nil {
		return err
	}

	addr := envOr("KIKO_LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Paid requests wait for settlement, which can take minutes.
		WriteTimeout: 4 * time.Minute,
	}
	slog.Info("listening", "addr", addr, "network", network, "facilitator", facilitatorURL,
		"cosigner", cosigner.AccountKey())
	return srv.ListenAndServe()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
