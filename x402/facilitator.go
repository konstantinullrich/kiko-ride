package x402

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Facilitator verifies and settles payments. HTTPFacilitator is the standard
// implementation; tests can substitute their own.
type Facilitator interface {
	Verify(ctx context.Context, p PaymentPayload, req PaymentRequirements) (VerifyResponse, error)
	Settle(ctx context.Context, p PaymentPayload, req PaymentRequirements) (SettleResponse, error)
}

// HTTPFacilitator talks to an x402 facilitator's /verify and /settle
// endpoints. The facilitator must list a cardano:* network in /supported.
type HTTPFacilitator struct {
	// URL is the facilitator base URL, e.g. "http://localhost:4022".
	URL string
	// Header is added to every request, e.g. for an API key.
	Header http.Header
	// Client defaults to http.DefaultClient. Settlement waits for block
	// confirmations, so give it a timeout well above a minute.
	Client *http.Client
}

type facilitatorRequest struct {
	X402Version         int                 `json:"x402Version"`
	PaymentPayload      PaymentPayload      `json:"paymentPayload"`
	PaymentRequirements PaymentRequirements `json:"paymentRequirements"`
}

func (f *HTTPFacilitator) Verify(ctx context.Context, p PaymentPayload, req PaymentRequirements) (VerifyResponse, error) {
	var out VerifyResponse
	err := f.post(ctx, "/verify", facilitatorRequest{Version, p, req}, &out)
	return out, err
}

func (f *HTTPFacilitator) Settle(ctx context.Context, p PaymentPayload, req PaymentRequirements) (SettleResponse, error) {
	var out SettleResponse
	err := f.post(ctx, "/settle", facilitatorRequest{Version, p, req}, &out)
	return out, err
}

func (f *HTTPFacilitator) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.URL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, vs := range f.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("facilitator %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("facilitator %s: %w", path, err)
	}
	// Facilitators report invalid payments with a JSON body, sometimes on a
	// 4xx status, so decode those; server errors are failures.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("facilitator %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("facilitator %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	return nil
}
