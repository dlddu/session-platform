package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gatedMCP is a session MCP with a decision-controlling gateway behind it and a
// counted upstream in front. The upstream counter is the assertion that matters
// most: "the network was not reached" is what an approval gate means.
type gatedMCP struct {
	server   *httptest.Server
	gateway  *fakeGateway
	upstream atomic.Int32
	fetched  []string
}

func newGatedMCP(t *testing.T, statuses ...string) *gatedMCP {
	t.Helper()
	g := &gatedMCP{gateway: newFakeGateway(t, statuses...)}
	gate := g.gateway.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }

	config := sessionMCPConfig{
		gateway: gate,
		fetch: func(_ context.Context, target string) (*http.Response, error) {
			g.upstream.Add(1)
			g.fetched = append(g.fetched, target)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"rate":1.07}`)),
			}, nil
		},
	}
	g.server = newSessionMCPServerWith(t, config)
	return g
}

func (g *gatedMCP) call(t *testing.T, rawURL string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": webFetchGetTool, "arguments": map[string]any{"url": rawURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, decoded := postMCP(t, g.server, string(body))
	if status != http.StatusOK {
		t.Fatalf("tools/call = %d, want 200", status)
	}
	return decoded
}

// toolResult unwraps a successful JSON-RPC response into the MCP tool result.
func toolResult(t *testing.T, body map[string]any) (result map[string]any, isError bool, text string) {
	t.Helper()
	if body["error"] != nil {
		t.Fatalf("tools/call returned a JSON-RPC error, not a tool result: %v", body["error"])
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call body = %v, want a result", body)
	}
	isError, _ = result["isError"].(bool)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tool result has no content: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("tool content[0] = %v, want an object", content[0])
	}
	text, _ = first["text"].(string)
	return result, isError, text
}

// The gate exists, so the tool exists — exactly one, and the one the reference
// implementation and the mockup both name.
func TestSessionMCPOffersTheGatedToolWhenTheGateExists(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	_, body := postMCP(t, g.server, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list body = %v, want a result", body)
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want exactly one gated tool", result["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != webFetchGetTool {
		t.Fatalf("tool = %v, want %q", tools[0], webFetchGetTool)
	}
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("tool has no input schema: %v", tool)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || props["url"] == nil {
		t.Fatalf("input schema = %v, want a url argument", schema)
	}
}

// Scenario 3 of docs/test/approval-gated-workload.md, minus the parts that need
// a cluster: the outbound call happens once, and only after the decision.
func TestSessionMCPFetchesOnlyAfterApproval(t *testing.T) {
	g := newGatedMCP(t, "PENDING", "PENDING", "APPROVED")
	body := g.call(t, "https://rates.vendor.example/v1/latest")
	_, isError, text := toolResult(t, body)
	if isError {
		t.Fatalf("approved call returned a tool error: %s", text)
	}
	if got := g.upstream.Load(); got != 1 {
		t.Fatalf("upstream reached %d times, want exactly 1", got)
	}
	if len(g.fetched) != 1 || g.fetched[0] != "https://rates.vendor.example/v1/latest" {
		t.Fatalf("fetched %v, want the approved URL", g.fetched)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool result text is not the JSON payload: %v", err)
	}
	if payload["success"] != true || payload["status"] != float64(http.StatusOK) {
		t.Fatalf("payload = %v, want a successful fetch result", payload)
	}
	if payload["body"] != `{"rate":1.07}` {
		t.Errorf("payload body = %v, want the upstream body", payload["body"])
	}
}

// Scenario 4: every refusal path reaches the model as a tool failure and leaves
// the network untouched.
func TestSessionMCPRefusalsNeverReachTheNetwork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     string
	}{
		{"rejected", []string{"REJECTED"}, "rejected"},
		{"expired", []string{"EXPIRED"}, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedMCP(t, tc.statuses...)
			body := g.call(t, "https://rates.vendor.example/v1/latest")
			_, isError, text := toolResult(t, body)
			if !isError {
				t.Fatalf("%s decision produced a success result: %s", tc.name, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("failure text = %q, want it to name the decision %q", text, tc.want)
			}
			if got := g.upstream.Load(); got != 0 {
				t.Fatalf("upstream reached %d times after a %s decision, want 0", got, tc.name)
			}
		})
	}
}

// A gateway that never decides ends as a timeout, and a timeout is still a
// refusal as far as the network is concerned.
func TestSessionMCPTimeoutIsARefusal(t *testing.T) {
	gw := newFakeGateway(t, "PENDING")
	gate := gw.gate(t)
	// A clock that jumps an hour per read exhausts the wait without waiting.
	clock := time.Now()
	gate.now = func() time.Time { clock = clock.Add(time.Hour); return clock }
	gate.sleep = func(context.Context, time.Duration) error { return nil }

	var upstream atomic.Int32
	srv := newSessionMCPServerWith(t, sessionMCPConfig{
		gateway: gate,
		fetch: func(context.Context, string) (*http.Response, error) {
			upstream.Add(1)
			return nil, io.EOF
		},
	})
	_, decoded := postMCP(t, srv,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"`+webFetchGetTool+`","arguments":{"url":"https://example.test/x"}}}`)
	_, isError, text := toolResult(t, decoded)
	if !isError {
		t.Fatalf("timed-out approval produced a success result: %s", text)
	}
	if !strings.Contains(text, "timeout") {
		t.Errorf("failure text = %q, want it to name the timeout", text)
	}
	if got := upstream.Load(); got != 0 {
		t.Fatalf("upstream reached %d times after a timeout, want 0", got)
	}
}

// A gateway outage is not a decision, but the agent still has to hear it as a
// tool failure (AC-F3).
func TestSessionMCPGatewayOutageIsAToolFailure(t *testing.T) {
	g := newGatedMCP(t, "PENDING")
	g.gateway.createStatus = http.StatusServiceUnavailable
	body := g.call(t, "https://rates.vendor.example/v1/latest")
	_, isError, text := toolResult(t, body)
	if !isError {
		t.Fatalf("gateway outage produced a success result: %s", text)
	}
	if got := g.upstream.Load(); got != 0 {
		t.Fatalf("upstream reached %d times without an approval, want 0", got)
	}
}

// Arguments a human could not meaningfully approve are refused before any
// request is created, so a bad URL never becomes a notification.
func TestSessionMCPRejectsUnapprovableTargets(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"empty", ""},
		{"plaintext http", "http://rates.vendor.example/v1/latest"},
		{"file scheme", "file:///etc/passwd"},
		{"no host", "https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedMCP(t, "APPROVED")
			body := g.call(t, tc.url)
			_, isError, text := toolResult(t, body)
			if !isError {
				t.Fatalf("%q was accepted: %s", tc.url, text)
			}
			if got := g.gateway.created.Load(); got != 0 {
				t.Errorf("created %d approval requests for an invalid target, want 0", got)
			}
			if got := g.upstream.Load(); got != 0 {
				t.Errorf("upstream reached %d times for an invalid target, want 0", got)
			}
		})
	}
}

// AC-F3's uniqueness clause, observed end to end: two calls in the same session
// produce two different external identifiers.
func TestSessionMCPGivesEachCallItsOwnExternalID(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		g.call(t, "https://rates.vendor.example/v1/latest")
		id, _ := g.gateway.lastBody["externalId"].(string)
		if id == "" {
			t.Fatal("approval request carried no external id")
		}
		if !strings.HasPrefix(id, "sess-abc:"+requestIDPrefix) {
			t.Fatalf("external id = %q, want {sessionID}:req-… (AC-F3)", id)
		}
		if seen[id] {
			t.Fatalf("external id %q repeated between two calls of one session", id)
		}
		seen[id] = true
	}
}

// Without a gate there is no tool, and a client that calls one anyway is told
// no rather than served.
func TestSessionMCPUngatedContainerRefusesToolCalls(t *testing.T) {
	srv := newSessionMCPServer(t)
	_, decoded := postMCP(t, srv,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+webFetchGetTool+`","arguments":{"url":"https://example.test/"}}}`)
	_, isError, _ := toolResult(t, decoded)
	if !isError {
		t.Fatal("an ungated container served a tool call")
	}
}

// A tool this server never listed is a protocol fault, not a tool outcome.
func TestSessionMCPUnknownToolIsAProtocolError(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	_, decoded := postMCP(t, g.server,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"shell_exec","arguments":{}}}`)
	rpcErr, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown tool returned %v, want a JSON-RPC error", decoded)
	}
	if rpcErr["code"] != float64(jsonRPCInvalidParams) {
		t.Errorf("error code = %v, want %d", rpcErr["code"], jsonRPCInvalidParams)
	}
	if got := g.gateway.created.Load(); got != 0 {
		t.Errorf("created %d approval requests for an unknown tool, want 0", got)
	}
}
