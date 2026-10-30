// Package x402 is net/http middleware that charges for requests with x402
// payments (scheme "exact", address-to-address) on Cardano and Bitcoin SV.
//
// One middleware can accept several networks, each verified and settled by
// its own facilitator. A PriceFunc returns one Price per network the client
// may pay on; the 402 response lists them all in accepts, and the client's
// accepted entry picks the network and facilitator.
//
//	mw, _ := x402.New(x402.Config{Networks: []x402.NetworkConfig{
//		{Network: x402.Preprod, Facilitator: &x402.HTTPFacilitator{URL: "http://localhost:4022", Client: client}},
//		{Network: x402.BSVTestnet, Facilitator: bsvFacilitator},
//	}})
//
//	mux.Handle("/report", mw.Require(func(r *http.Request) ([]x402.Price, error) {
//		return []x402.Price{
//			{Network: x402.Preprod, PayTo: "addr_test1...", Amount: 2_000_000}, // 2 ADA
//			{Network: x402.BSVTestnet, PayTo: "mxyz...", Amount: 50_000},      // 50,000 satoshis
//		}, nil
//	})(reportHandler))
//
// With a single network, Price.Network may be left empty and Fixed works.
//
// On BSV, PayTo is a P2PKH address or a hex locking script, for outputs that
// have no address such as bare multisig, and the payload transaction is raw
// or Extended Format. The middleware checks only the shape of each network's
// payTo, asset and transaction; whether the transaction pays is the
// facilitator's call. Package bsvfacilitator is an in-process BSV
// facilitator.
//
// The flow follows the scheme's "authorization" model: an unpaid request gets
// a 402 with PAYMENT-REQUIRED; the paid retry's PAYMENT-SIGNATURE is checked
// against the prices, verified by the facilitator, the handler runs with its
// response held back, and the response is released with PAYMENT-RESPONSE only
// after the facilitator settles. Handler responses with status >= 400 are
// released without settling, so the client is not charged.
package x402
