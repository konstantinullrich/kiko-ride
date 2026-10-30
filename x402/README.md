# x402

net/http middleware that charges for requests with x402 payments on Cardano. See `doc.go` for the API.

## Demo: Cardano Foundation facilitator (preprod)

The Cardano Foundation hosts a facilitator on preprod. It holds no keys or funds; the buyer pays the network fee.

```go
mw, err := x402.New(x402.Config{
	Network: x402.Preprod,
	Facilitator: &x402.HTTPFacilitator{
		URL:    "https://x402.preprod.dev.ecosyseng.cf-deployments.org",
		Client: &http.Client{Timeout: 3 * time.Minute}, // settlement waits for confirmations
	},
})

mux.Handle("/report", mw.Require(x402.Fixed("addr_test1...", 2_000_000))(reportHandler)) // 2 tADA
```

For demos only. In production, run your own facilitator (e.g. on `@x402/cardano`) over HTTPS.
