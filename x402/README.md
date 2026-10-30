# x402

net/http middleware that charges for requests with x402 payments on Cardano and Bitcoin SV. One middleware can accept several networks, each with its own facilitator. See `doc.go` for the API.

| Network          | Id                                                   | Asset                         | `payTo`                              |
| ---------------- | ---------------------------------------------------- | ----------------------------- | ------------------------------------ |
| Cardano          | `cardano:mainnet`, `cardano:preprod`, `cardano:preview` | `lovelace` or `policyId.assetName` | bech32 address                       |
| Bitcoin SV       | `bsv:mainnet`, `bsv:testnet`                         | `satoshi`                     | P2PKH address or hex locking script  |

BSV uses `bsv:*` rather than CAIP-2's `bip122:<genesis hash>`, which BSV shares with BTC and BCH.

## Demo: Cardano Foundation facilitator (preprod)

The Cardano Foundation hosts a facilitator on preprod. It holds no keys or funds; the buyer pays the network fee.

```go
mw, err := x402.New(x402.Config{Networks: []x402.NetworkConfig{{
	Network: x402.Preprod,
	Facilitator: &x402.HTTPFacilitator{
		URL:    "https://x402.preprod.dev.ecosyseng.cf-deployments.org",
		Client: &http.Client{Timeout: 3 * time.Minute}, // settlement waits for confirmations
	},
}}})

mux.Handle("/report", mw.Require(x402.Fixed("addr_test1...", 2_000_000))(reportHandler)) // 2 tADA
```

For demos only. In production, run your own facilitator (e.g. on `@x402/cardano`) over HTTPS.

## BSV

There is no public BSV facilitator for this scheme. `bsv:*` networks need a `Facilitator` that speaks the standard `/verify` and `/settle` API, or your own implementation of the interface.
