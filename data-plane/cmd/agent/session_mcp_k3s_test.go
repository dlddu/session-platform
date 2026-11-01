package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// k3sMCP is a session MCP with a decision-controlling gateway in front of a
// counted K3s MCP proxy. The proxy's counter is the assertion that matters
// most: "the K3s MCP was not reached" is what an approval gate means.
type k3sMCP struct {
	server  *httptest.Server
	gateway *fakeGateway
	proxy   *httptest.Server

	calls atomic.Int32
	// authSeen records whether the session MCP ever sent an Authorization
	// header. The token is the proxy's to install, never this container's to
	// carry (AC-F6 C1, C4).
	authSeen atomic.Bool

	mu       sync.Mutex
	lastCall map[string]any
}

func newK3sMCP(t *testing.T, statuses ...string) *k3sMCP {
	t.Helper()
	m := &k3sMCP{gateway: newFakeGateway(t, statuses...)}
	gate := m.gateway.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }

	m.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			m.authSeen.Store(true)
		}
		body, _ := io.ReadAll(r.Body)
		var call map[string]any
		_ = json.Unmarshal(body, &call)
		m.mu.Lock()
		m.lastCall = call
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"items\":[]}"}]}}`))
	}))
	t.Cleanup(m.proxy.Close)

	config := sessionMCPConfig{
		gateway: gate,
		k3s:     newK3sMCPClient(m.proxy.URL + "/mcp"),
	}
	m.server = newSessionMCPServerWith(t, config)
	return m
}

func (m *k3sMCP) toolNames(t *testing.T) map[string]bool {
	t.Helper()
	_, body := postMCP(t, m.server, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ := body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}
	return names
}

func (m *k3sMCP) call(t *testing.T, tool string, arguments map[string]any) (bool, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, decoded := postMCP(t, m.server, string(body))
	if status != http.StatusOK {
		t.Fatalf("tools/call = %d, want 200", status)
	}
	_, isError, text := toolResult(t, decoded)
	return isError, text
}

// The re-exported read tools stand only where the K3s MCP proxy does.
func TestK3sReadToolsAreListedOnlyWhereTheProxyIs(t *testing.T) {
	m := newK3sMCP(t, "APPROVED")
	names := m.toolNames(t)
	if !names[k3sResourceListTool] || !names[k3sResourceGetTool] {
		t.Fatalf("tools = %v, want %s and %s", names, k3sResourceListTool, k3sResourceGetTool)
	}

	bare := sessionMCPConfig{gateway: newFakeGateway(t, "APPROVED").gate(t)}
	without := map[string]bool{}
	for _, raw := range bare.toolDefinitions() {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		without[name] = true
	}
	if without[k3sResourceListTool] || without[k3sResourceGetTool] {
		t.Fatalf("tools = %v, want no K3s tool without a proxy", without)
	}
}

// A wrong target must not reach a person's screen: the arguments are checked
// before the approval request exists (R3).
func TestK3sRefusesBadArgumentsBeforeAskingForApproval(t *testing.T) {
	m := newK3sMCP(t, "APPROVED")

	if isError, text := m.call(t, k3sResourceGetTool, map[string]any{"apiVersion": "v1", "kind": "Pod"}); !isError {
		t.Fatalf("a call without a name returned %q, want a tool failure", text)
	}
	if isError, text := m.call(t, k3sResourceListTool, map[string]any{"apiVersion": "v1"}); !isError {
		t.Fatalf("a call without a kind returned %q, want a tool failure", text)
	}
	if isError, text := m.call(t, k3sResourceGetTool, map[string]any{
		"apiVersion": "v1", "kind": "Pod", "name": "../../etc/passwd",
	}); !isError {
		t.Fatalf("a call with a bad name returned %q, want a tool failure", text)
	}
	if isError, text := m.call(t, k3sResourceListTool, map[string]any{
		"apiVersion": "v1", "kind": "Pod", "limit": 0,
	}); !isError {
		t.Fatalf("a call with a bad limit returned %q, want a tool failure", text)
	}
	if isError, text := m.call(t, k3sResourceListTool, map[string]any{
		"apiVersion": "v1", "kind": "Pod", "unexpected": true,
	}); !isError {
		t.Fatalf("a call with an unknown argument returned %q, want a tool failure", text)
	}

	if got := m.gateway.created.Load(); got != 0 {
		t.Fatalf("approval requests = %d, want 0 — the check comes before the request", got)
	}
	if got := m.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 — the K3s MCP was reached without approval", got)
	}
}

// An approved call delegates exactly one upstream call, with the arguments the
// caller gave, and the approver is shown what it would do (R4).
func TestK3sApprovedCallDelegatesOneUpstreamCall(t *testing.T) {
	m := newK3sMCP(t, "APPROVED")

	isError, text := m.call(t, k3sResourceGetTool, map[string]any{
		"apiVersion": "v1", "kind": "Pod", "namespace": "default", "name": "control-plane-abc",
	})
	if isError {
		t.Fatalf("the call failed: %s", text)
	}
	if got := m.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}

	m.mu.Lock()
	call := m.lastCall
	m.mu.Unlock()
	if call == nil {
		t.Fatal("the upstream received no call")
	}
	if call["method"] != "tools/call" {
		t.Fatalf("method = %v, want tools/call", call["method"])
	}
	params, _ := call["params"].(map[string]any)
	if params == nil {
		t.Fatalf("params = %v, want an object", call["params"])
	}
	if params["name"] != "resource_get" {
		t.Fatalf("upstream tool = %v, want resource_get", params["name"])
	}
	arguments, _ := params["arguments"].(map[string]any)
	if arguments["kind"] != "Pod" || arguments["name"] != "control-plane-abc" || arguments["namespace"] != "default" {
		t.Fatalf("arguments = %v, want the pod the caller named", arguments)
	}

	// The approver reads the context, and it names the target, the operation
	// and the resource — never a credential.
	context := m.gateway.lastBody["context"]
	approval, ok := context.(string)
	if !ok {
		t.Fatalf("approval context = %v, want a string", context)
	}
	for _, want := range []string{"k3s-mcp", "get", "Pod", "control-plane-abc", "default"} {
		if !strings.Contains(approval, want) {
			t.Fatalf("approval context = %s, want it to name %q", approval, want)
		}
	}
}

// A refusal never reaches the K3s MCP and comes back as a tool result, not as
// a protocol error (R7).
func TestK3sRefusalIsAToolResultAndTheUpstreamIsNotReached(t *testing.T) {
	m := newK3sMCP(t, "REJECTED")

	isError, text := m.call(t, k3sResourceListTool, map[string]any{"apiVersion": "v1", "kind": "Pod"})
	if !isError {
		t.Fatalf("the refused call returned %q, want a tool failure", text)
	}
	if got := m.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 — a refusal must not reach the K3s MCP", got)
	}
}

// The token is the proxy's to install. This container sends no credential of
// its own (AC-F6 C1, C4).
func TestK3sCallCarriesNoAuthorizationHeader(t *testing.T) {
	m := newK3sMCP(t, "APPROVED")

	if isError, text := m.call(t, k3sResourceListTool, map[string]any{"apiVersion": "v1", "kind": "Pod"}); isError {
		t.Fatalf("the call failed: %s", text)
	}
	if m.authSeen.Load() {
		t.Fatal("the session MCP sent an Authorization header — the token must stay in the proxy container")
	}
}

// The upstream may answer with an event stream instead of JSON; both carry the
// same tool result.
func TestK3sReadsAnEventStreamAnswer(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"streamed\"}]}}\n\n"))
	}))
	t.Cleanup(proxy.Close)

	gateway := newFakeGateway(t, "APPROVED")
	gate := gateway.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }
	server := newSessionMCPServerWith(t, sessionMCPConfig{gateway: gate, k3s: newK3sMCPClient(proxy.URL + "/mcp")})

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": k3sResourceListTool, "arguments": map[string]any{"apiVersion": "v1", "kind": "Pod"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, decoded := postMCP(t, server, string(body))
	if status != http.StatusOK {
		t.Fatalf("tools/call = %d, want 200", status)
	}
	_, isError, text := toolResult(t, decoded)
	if isError {
		t.Fatalf("the call failed: %s", text)
	}
	if !strings.Contains(text, "streamed") {
		t.Fatalf("the result = %s, want the upstream text", text)
	}
}
