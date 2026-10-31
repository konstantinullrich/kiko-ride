# x402

net/http middleware that charges for requests with x402 payments on Cardano and Bitcoin SV. One middleware can accept several networks, each with its own facilitator. See `doc.go` for the API.

| Network          | Id                                                   | Asset                         | `payTo`                              |
| ---------------- | ---------------------------------------------------- | ----------------------------- | ------------------------------------ |
| Cardano          | `cardano:mainnet`, `cardano:preprod`, `cardano:preview` | `lovelace`                    | bech32 address                       |
| Bitcoin SV       | `bsv:mainnet`, `bsv:testnet`                         | `satoshi`                     | P2PKH address or hex locking script  |

BSV uses `bsv:*` rather than CAIP-2's `bip122:<genesis hash>`, which BSV shares with BTC and BCH.

## Cardano

`cardanofacilitator` verifies and settles lovelace payments in process, reading the chain and submitting through Koios. It enforces the scheme's facilitator rules for address-to-address payments: amount and recipient, nonce and inputs unspent, value conservation, the live fee floor, a TTL within `maxTimeoutSeconds`, and the payment output's minimum UTxO value. Settlement waits for `confirmationPolicy.l1Confirmations` (default 1) and otherwise answers `settlement_pending`; settling the same payload again resumes watching the transaction and never submits it twice.

```go
mw, err := x402.New(x402.Config{Networks: []x402.NetworkConfig{{
	Network: x402.Preprod,
	Facilitator: &cardanofacilitator.Facilitator{
		Network: x402.Preprod,
		Chain:   &cardanofacilitator.Koios{URL: "https://preprod.koios.rest/api/v1"},
	},
}}})

mux.Handle("/report", mw.Require(x402.Fixed("addr_test1...", 2_000_000))(reportHandler)) // 2 tADA
```

It refuses transactions it cannot balance from UTxO data (minting, withdrawals, certificates, governance, donations) and Plutus scripts. An external facilitator that speaks the standard `/verify` and `/settle` API works through `HTTPFacilitator`; give its client a timeout of a few minutes, since settlement waits for confirmations.

## BSV

`bsvfacilitator` verifies and settles BSV payments in process and broadcasts through ARC. There is no public BSV facilitator for this scheme.
