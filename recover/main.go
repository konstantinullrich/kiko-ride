// Command recover sends everything at a custodial ride's address, less the
// fee, to another address by hand. On a custodial ride the server holds two
// of the three keys, so it can spend the ride address without the driver,
// e.g. to refund a rider or pay a driver whose wallet cannot sign.
//
//	go run ./recover -ride <id> -to <addr>          # dry run
//	go run ./recover -ride <id> -to <addr> -submit  # submit
//
// The ride's status in the database is not touched.
//
// It reads the same environment as the server: KIKO_SERVER_MNEMONIC,
// CARDANO_NETWORK, KIKO_DB_PATH, KOIOS_URL and KOIOS_API_TOKEN.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Salvionied/apollo/v2/backend"
	"github.com/Salvionied/apollo/v2/backend/koios"
	"github.com/Salvionied/apollo/v2/constants"
	"github.com/blinklabs-io/gouroboros/cbor"
	"github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/ledger/conway"

	"kiko-backend/api"
	"kiko-backend/multisig"
)

func main() {
	if err := run(); err != nil {
		slog.Error("recover", "err", err)
		os.Exit(1)
	}
}

func run() error {
	rideID := flag.String("ride", "", "id of the custodial ride to recover")
	to := flag.String("to", "", "bech32 address that receives the funds")
	submit := flag.Bool("submit", false, "submit the transaction instead of only printing it")
	flag.Parse()
	if *rideID == "" || *to == "" {
		flag.Usage()
		return errors.New("-ride and -to are required")
	}

	network := envOr("CARDANO_NETWORK", "preprod")
	walletNetwork := multisig.Preprod
	if network == "mainnet" {
		walletNetwork = multisig.Mainnet
	}
	dest, err := common.NewAddress(*to)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	if dest.NetworkId() != uint(walletNetwork) {
		return fmt.Errorf("-to is not a %s address", network)
	}

	server, err := multisig.NewServer(walletNetwork, os.Getenv("KIKO_SERVER_MNEMONIC"))
	if err != nil {
		return fmt.Errorf("KIKO_SERVER_MNEMONIC: %w", err)
	}
	db, err := api.OpenDB(envOr("KIKO_DB_PATH", "kiko.db"))
	if err != nil {
		return fmt.Errorf("KIKO_DB_PATH: %w", err)
	}
	defer db.Close()

	var index uint32
	var status, riderKey, driverKey, payTo string
	err = db.QueryRow(`SELECT address_index, status, rider_key, driver_key, pay_to FROM rides WHERE id = ?`, *rideID).
		Scan(&index, &status, &riderKey, &driverKey, &payTo)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("ride %s not found", *rideID)
	case err != nil:
		return err
	case riderKey != "":
		return fmt.Errorf("ride %s is not custodial: the rider holds a key", *rideID)
	case driverKey == "":
		return fmt.Errorf("ride %s has no driver, so it has no payment address", *rideID)
	}

	w, _, err := server.RideWallet(driverKey, "")
	if err != nil {
		return err
	}
	addr, err := w.Address(index)
	if err != nil {
		return err
	}
	if addr.String() != payTo {
		return fmt.Errorf("derived address %s does not match the ride's %s; wrong mnemonic or network?", addr, payTo)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	chain, err := chainContext(network, walletNetwork)
	if err != nil {
		return err
	}
	p, err := w.BuildSweep(ctx, chain, index, dest)
	if err != nil {
		return err
	}
	witnesses, err := server.Sign(p)
	if err != nil {
		return err
	}
	signed, err := w.Assemble(p, witnesses...)
	if err != nil {
		return err
	}
	var tx conway.ConwayTransaction
	if _, err := cbor.DecodeExact(signed, &tx); err != nil {
		return err
	}
	fmt.Printf("ride  %s (%s), address %s (index %d)\nsend  %s lovelace to %s, fee %s\ntx    %s\ncbor  %x\n",
		*rideID, status, addr, index, tx.Body.Outputs()[0].Amount(), dest, tx.Body.Fee(), tx.Body.Id(), signed)

	if !*submit {
		fmt.Println("dry run; pass -submit to submit")
		return nil
	}
	if _, err := backend.SubmitTxContext(ctx, chain, signed); err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	fmt.Println("submitted")
	return nil
}

// chainContext connects to Koios, the public instance for the network unless
// KOIOS_URL is set.
func chainContext(network string, walletNetwork multisig.Network) (backend.ChainContext, error) {
	url := os.Getenv("KOIOS_URL")
	if url == "" {
		switch strings.TrimPrefix(network, "cardano:") {
		case "mainnet":
			url = constants.KoiosBaseUrlMainnet
		case "preprod":
			url = constants.KoiosBaseUrlPreprod
		case "preview":
			url = constants.KoiosBaseUrlPreview
		default:
			return nil, fmt.Errorf("KOIOS_URL is required for network %q", network)
		}
	}
	return koios.NewKoiosChainContext(url, uint8(walletNetwork), os.Getenv("KOIOS_API_TOKEN")), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
