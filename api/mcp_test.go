package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"kiko-backend/x402"
)

func (e *env) mcpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(e.s.MCPHandler())
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// callTool returns the text of the tool result's first content.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, wantError bool) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError != wantError {
		t.Fatalf("%s: expected error=%v, got %q", name, wantError, text)
	}
	return text
}

func TestMCPRideLifecycle(t *testing.T) {
	e := newEnv(t)
	cs := e.mcpSession(t)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "get_ride,quote_ride,request_ride,search_address" {
		t.Fatalf("unexpected tools: %s", got)
	}

	if text := callTool(t, cs, "search_address", map[string]any{"q": "Alexander"}, false); !strings.Contains(text, "Alexanderplatz") {
		t.Fatalf("unexpected search: %s", text)
	}
	callTool(t, cs, "search_address", map[string]any{"q": "Alexander", "limit": 50}, true)

	d := e.registerDriver(t)
	var q quote
	json.Unmarshal([]byte(callTool(t, cs, "quote_ride", map[string]any{
		"start_address": "Somewhere", "end_address": "Alexanderplatz",
		"start": map[string]any{"lat": 52.5251, "lng": 13.3694},
	}, false)), &q)
	if q.FareLovelace == nil || q.FareLovelace.Min != fare(d, q.DistanceKm) {
		t.Fatalf("unexpected quote: %+v", q)
	}
	if text := callTool(t, cs, "quote_ride", map[string]any{"start_address": "Nowhere", "end_address": "Alexanderplatz"}, true); !strings.HasPrefix(text, "422 ") {
		t.Fatalf("expected 422, got %s", text)
	}

	var ride rideView
	json.Unmarshal([]byte(callTool(t, cs, "request_ride", map[string]any{
		"start_address": "Berlin Hbf", "end_address": "Alexanderplatz", "pickup_time": "2026-11-01T09:00:00Z",
	}, false)), &ride)
	if ride.Status != StatusRequested || !ride.Custodial {
		t.Fatalf("unexpected ride: %+v", ride)
	}
	callTool(t, cs, "get_ride", map[string]any{"id": ride.ID}, false)
	expect(t, e.do(t, http.MethodPost, "/api/v1/driver/drive/"+ride.ID, d.PublicKey, map[string]any{"eta_minutes": 4}), http.StatusOK)

	// Accepted: get_ride answers with the x402 requirements until paid.
	text := callTool(t, cs, "get_ride", map[string]any{"id": ride.ID}, true)
	body, ok := strings.CutPrefix(text, "402 Payment Required: ")
	if !ok {
		t.Fatalf("expected 402, got %s", text)
	}
	var pr x402.PaymentRequired
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(pr.Resource.URL, "/api/v1/ride/"+ride.ID) || strings.HasPrefix(pr.Resource.URL, "http://localhost/") {
		t.Fatalf("resource should name the MCP server's origin: %s", pr.Resource.URL)
	}

	raw, _ := json.Marshal(x402.PaymentPayload{
		X402Version: x402.Version,
		Accepted:    pr.Accepts[0],
		Payload:     x402.CardanoPayload{Transaction: e.txB64, Nonce: hex.EncodeToString(make([]byte, 32)) + "#0"},
	})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_ride", Arguments: map[string]any{
		"id": ride.ID, "payment_signature": base64.StdEncoding.EncodeToString(raw),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("expected the paid ride and its settlement, got %+v", res.Content)
	}
	var paid rideView
	json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &paid)
	if paid.Status != StatusPaid || paid.Car == nil {
		t.Fatalf("unexpected paid ride: %+v", paid)
	}
	if s := res.Content[1].(*mcp.TextContent).Text; !strings.HasPrefix(s, "x402 settlement: ") || !strings.Contains(s, `"success":true`) {
		t.Fatalf("unexpected settlement: %s", s)
	}
	if paid.AddressIndex != nil || paid.Script != "" {
		t.Fatalf("driver-only fields leaked to the rider: %+v", paid)
	}
}
