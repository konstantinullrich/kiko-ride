package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kiko-backend/geo"
	"kiko-backend/x402"
)

// mcpOriginHeader carries the scheme and host the MCP request came in on, so
// the rider endpoints called on its behalf name the same x402 resource URL
// as a direct call would.
const mcpOriginHeader = "X-Kiko-Mcp-Origin"

// MCPHandler serves the rider endpoints as MCP tools over the streamable
// HTTP transport. Each tool call runs the matching /api/v1 route in-process,
// so validation, fares and x402 payments behave exactly as over REST.
func (s *Server) MCPHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "kiko", Title: "Kiko rides", Version: "v1"}, &mcp.ServerOptions{
		Instructions: "Book Kiko rides paid in ADA or BSV. Look up addresses with search_address, price a trip with quote_ride, " +
			"book it with request_ride, then poll get_ride. Once a driver accepts, get_ride answers 402 with x402 payment " +
			"requirements; pay by calling get_ride again with payment_signature. A ride is paid on its driver's chain: amounts are " +
			"in lovelace on cardano:* networks (1 ADA = 1,000,000), in satoshis on bsv:*. " +
			"Paying on bsv:* differs from usual x402: payTo is not an address but a hex locking script (a bare 2-of-3 multisig), " +
			"so add an output locked by exactly that script; that output must pay exactly amount; payload.transaction is the " +
			"signed transaction in Extended Format (BRC-30, each input carrying the output it spends), base64; and nonce is " +
			"<txid>:<vout> of one of its inputs. A raw transaction is rejected. The ride counts as paid once ARC accepts the " +
			"transaction; there is no need to wait for a block. " +
			"If paying answers 402 with an x402 settlement whose errorReason is settlement_pending or unexpected_settle_error, " +
			"the transaction may still land: call get_ride again with the same payment_signature and never sign a new " +
			"transaction, or the rider could pay twice. " +
			"For privacy every ride is paid to its own fresh 2-of-3 multisig address (a script address addr1w... or " +
			"addr_test1w... on Cardano, a bare-multisig script on BSV), so payTo differs from ride to ride and has no history " +
			"on chain. This is expected and not a sign of fraud; do not compare payTo with earlier rides. Instead check that " +
			"the requirements name this ride (\"Kiko ride <id>\") and that amount matches the ride's fare.",
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_address",
		Description: "List addresses matching free text, best first, so the rider can check which place an address resolves to. Pass the lat/lng of the chosen result to quote_ride and request_ride.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.mcpSearch)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "quote_ride",
		Description: "Quote a ride: the straight-line distance and, per network, the fare range over its registered drivers in that network's smallest unit.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.mcpQuote)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "request_ride",
		Description: "Request a ride. Returns the ride in status requested; keep its id to poll it with get_ride. A cosigner_key, a fresh public key for this ride, ties the ride to its chain's drivers, and the ride's payTo will contain it: check that before paying. Without it the ride is custodial: the server holds two of the three keys of the ride's payment address, and drivers on any chain can take it.",
	}, s.mcpRequestRide)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_ride",
		Description: "Get a ride. Free while it is requested, paid or paid_out. Once a driver accepts it, the fare must be paid once with x402 v2 " +
			"(exact scheme) on the ride's network: without payment_signature this answers 402 with the requirements. Build and sign a " +
			"transaction paying exactly amount to payTo, then call again with payment_signature. On cardano:* payTo is a script address, " +
			"payload.transaction is the Conway transaction CBOR in base64 and nonce is <txHash>#<index>. On bsv:* payTo is a hex locking " +
			"script to lock an output with, payload.transaction is the transaction in Extended Format (BRC-30) in base64, never raw, and " +
			"nonce is <txid>:<vout> of one of its inputs. The paid response includes the car; settlement can take minutes on Cardano. " +
			"payTo is new and unused, unique to this ride, for privacy; a payTo you have not seen before is expected. " +
			"On cardano:* set a TTL within maxTimeoutSeconds and pay from plain wallet inputs, no Plutus scripts. " +
			"An x402 settlement of settlement_pending or unexpected_settle_error means the payment may still land: retry " +
			"with the same payment_signature, never a new transaction.",
	}, s.mcpGetRide)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		r.Header.Set(mcpOriginHeader, scheme+"://"+r.Host)
		h.ServeHTTP(w, r)
	})
}

type searchInput struct {
	Query string `json:"q" jsonschema:"free-form address"`
	Limit int    `json:"limit,omitempty" jsonschema:"number of results, 1-10, default 5"`
}

func (s *Server) mcpSearch(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{"q": {in.Query}}
	if in.Limit != 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	return s.callRoute(ctx, req, http.MethodGet, "/api/v1/search?"+q.Encode(), nil, "")
}

type quoteInput struct {
	StartAddress string     `json:"start_address" jsonschema:"pickup address"`
	EndAddress   string     `json:"end_address" jsonschema:"drop-off address"`
	Start        *geo.Point `json:"start,omitempty" jsonschema:"coordinates of the pickup; skips geocoding start_address"`
	End          *geo.Point `json:"end,omitempty" jsonschema:"coordinates of the drop-off; skips geocoding end_address"`
}

func (s *Server) mcpQuote(ctx context.Context, req *mcp.CallToolRequest, in quoteInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{"start_address": {in.StartAddress}, "end_address": {in.EndAddress}}
	for name, p := range map[string]*geo.Point{"start": in.Start, "end": in.End} {
		if p != nil {
			q.Set(name, strconv.FormatFloat(p.Lat, 'f', -1, 64)+","+strconv.FormatFloat(p.Lng, 'f', -1, 64))
		}
	}
	return s.callRoute(ctx, req, http.MethodGet, "/api/v1/ride?"+q.Encode(), nil, "")
}

// requestRideInput is POST /api/v1/ride's body; its JSON names match
// createRideRequest.
type requestRideInput struct {
	StartAddress string     `json:"start_address" jsonschema:"pickup address"`
	EndAddress   string     `json:"end_address" jsonschema:"drop-off address"`
	PickupTime   string     `json:"pickup_time" jsonschema:"pickup time, RFC 3339"`
	CosignerKey  string     `json:"cosigner_key,omitempty" jsonschema:"the rider's public key for this ride, which cosigns the ride address: Cardano addr_shared_vk1... or a BSV compressed public key in hex. Not an account key; use a fresh key per ride"`
	Start        *geo.Point `json:"start,omitempty" jsonschema:"coordinates of the pickup; skips geocoding start_address"`
	End          *geo.Point `json:"end,omitempty" jsonschema:"coordinates of the drop-off; skips geocoding end_address"`
}

func (s *Server) mcpRequestRide(ctx context.Context, req *mcp.CallToolRequest, in requestRideInput) (*mcp.CallToolResult, any, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, nil, err
	}
	return s.callRoute(ctx, req, http.MethodPost, "/api/v1/ride", body, "")
}

type getRideInput struct {
	ID string `json:"id" jsonschema:"ride id from request_ride"`
	// PaymentSignature is sent as the PAYMENT-SIGNATURE header.
	PaymentSignature string `json:"payment_signature,omitempty" jsonschema:"base64 x402 PaymentPayload paying the fare, as in the PAYMENT-SIGNATURE header; on bsv:* its payload.transaction must be Extended Format (BRC-30)"`
}

func (s *Server) mcpGetRide(ctx context.Context, req *mcp.CallToolRequest, in getRideInput) (*mcp.CallToolResult, any, error) {
	if in.ID == "" {
		return toolError("id is required"), nil, nil
	}
	return s.callRoute(ctx, req, http.MethodGet, "/api/v1/ride/"+url.PathEscape(in.ID), nil, in.PaymentSignature)
}

// callRoute runs an API route in-process and turns its response into a tool
// result: the JSON body, flagged as an error from status 400 on, followed by
// the decoded x402 settlement when there is one.
func (s *Server) callRoute(ctx context.Context, req *mcp.CallToolRequest, method, target string, body []byte, paymentSignature string) (*mcp.CallToolResult, any, error) {
	origin := "http://localhost"
	if req.Extra != nil && req.Extra.Header.Get(mcpOriginHeader) != "" {
		origin = req.Extra.Header.Get(mcpOriginHeader)
	}
	r, err := http.NewRequestWithContext(ctx, method, origin+target, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if paymentSignature != "" {
		r.Header.Set(x402.HeaderPaymentSignature, paymentSignature)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)

	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: rec.Body.String()}}}
	if rec.Code >= 400 {
		res.IsError = true
		res.Content[0] = &mcp.TextContent{Text: fmt.Sprintf("%d %s: %s", rec.Code, http.StatusText(rec.Code), rec.Body.String())}
	}
	if v := rec.Header().Get(x402.HeaderPaymentResponse); v != "" {
		if settlement, err := base64.StdEncoding.DecodeString(v); err == nil {
			res.Content = append(res.Content, &mcp.TextContent{Text: "x402 settlement: " + string(settlement)})
		}
	}
	return res, nil, nil
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}
