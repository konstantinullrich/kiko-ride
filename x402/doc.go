// Package x402 is net/http middleware that charges for requests with x402
// payments on Cardano (scheme "exact", address-to-address).
//
// You choose the recipient address and amount per request with a PriceFunc.
// Transaction checks and broadcasting are delegated to an x402 facilitator
// that supports a cardano:* network, such as the @x402/cardano facilitator.
//
//	mw, _ := x402.New(x402.Config{
//		Network:     x402.Preprod,
//		Facilitator: &x402.HTTPFacilitator{URL: "http://localhost:4022", Client: &http.Client{Timeout: 3 * time.Minute}},
//	})
//
//	mux.Handle("/report", mw.Require(func(r *http.Request) (x402.Price, error) {
//		return x402.Price{PayTo: "addr_test1...", Amount: 2_000_000}, nil // 2 ADA
//	})(reportHandler))
//
// The flow follows the scheme's "authorization" model: an unpaid request gets
// a 402 with PAYMENT-REQUIRED; the paid retry's PAYMENT-SIGNATURE is checked
// against the price, verified by the facilitator, the handler runs with its
// response held back, and the response is released with PAYMENT-RESPONSE only
// after the facilitator settles. Handler responses with status >= 400 are
// released without settling, so the client is not charged.
package x402
