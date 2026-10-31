# kiko-backend

Backend for Kiko, a ride-hailing service paid in ADA on Cardano.

Riders request rides, drivers accept them, and the rider pays the fare with
[x402](https://x402.org) into a per-ride 2-of-3 multisig address held by the
Kiko server, the driver and the rider (or a second server key if the rider
brings none). The driver is paid out by signing a spend from that address,
which the server cosigns and submits.

- `api/` — HTTP API ([endpoints](api/endpoints.md)), and the rider endpoints
  as an MCP server at `/mcp` ([tools](api/endpoints.md#mcp-server))
- `multisig/` — CIP-1854 multisig wallets and the server cosigner
- `bsvmultisig/` — the same for Bitcoin SV (bare 2-of-3 multisig)
- `x402/` — x402 payment middleware
- `geo/` — geocoding (Mapbox) and distances

## Run

```sh
cp .env.example .env   # fill in the secrets
./start.sh             # exports .env and runs go run .
```

Drivers and rides are stored in SQLite at `KIKO_DB_PATH` (`kiko.db` by
default). Or with Docker, keeping the database in a volume:

```sh
docker build -t kiko-backend .
docker run -p 8080:8080 --env-file .env -v kiko-data:/data kiko-backend
```

## Configuration

| Variable                   | Required | Description                                                       |
| -------------------------- | -------- | ----------------------------------------------------------------- |
| `KIKO_SERVER_MNEMONIC`     | yes      | BIP-39 mnemonic of the server's cosigner keys. Keep it secret.    |
| `MAPBOX_ACCESS_TOKEN`      | yes      | Mapbox token for geocoding                                        |
| `CARDANO_NETWORK`          | no       | `preprod` (default), `preview` or `mainnet`                       |
| `X402_FACILITATOR_URL`     | no       | Required outside preprod; defaults to the Cardano Foundation's    |
| `KOIOS_URL`                | no       | Koios base URL; defaults to the public instance for the network   |
| `KOIOS_API_TOKEN`          | no       | Koios bearer token; the public tier is used when unset            |
| `BSV_ARC_URL`              | no       | ARC endpoint ending in `/v1`; enables BSV drivers and riders and broadcasts their transactions (`BSV_ARC_API_KEY` optional) |
| `BSV_FACILITATOR_URL`      | no       | External x402 facilitator for BSV fares; the built-in one is used when unset |
| `KIKO_LISTEN_ADDR`         | no       | Default `:8080`                                                   |
| `KIKO_DB_PATH`             | no       | SQLite database file, default `kiko.db` (`/data/kiko.db` in Docker) |
| `MAPBOX_PERMANENT`         | no       | `true` for permanent geocoding                                    |

## Development

```sh
go test ./...
go run .
```
