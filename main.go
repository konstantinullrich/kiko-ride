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
//	KIKO_COSIGNER_A/B/C        acct_shared_xvk keys of Kiko's 2-of-3 wallet
//	KIKO_FIRST_ADDRESS_INDEX   wallet index for the first ride (default 0)
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

	wallet, err := multisig.NewWallet(walletNetwork,
		os.Getenv("KIKO_COSIGNER_A"), os.Getenv("KIKO_COSIGNER_B"), os.Getenv("KIKO_COSIGNER_C"))
	if err != nil {
		return fmt.Errorf("KIKO_COSIGNER_A/B/C: %w", err)
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
		Wallet:            wallet,
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
	slog.Info("listening", "addr", addr, "network", network, "facilitator", facilitatorURL)
	return srv.ListenAndServe()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
