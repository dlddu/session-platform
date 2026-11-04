package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeK3sMCP is the K3s MCP the session MCP delegates to, and the counter that
// says whether the delegation happened at all: "the upstream was not reached"
// is what an approval gate means.
type fakeK3sMCP struct {
	server *httptest.Server
	calls  atomic.Int32
	names  []string
}

func newFakeK3sMCP(t *testing.T, answer string) *fakeK3sMCP {
	t.Helper()
	f := &fakeK3sMCP{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var call struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		f.names = append(f.names, call.Params.Name)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":` +
			strconv.Quote(answer) + `}],"isError":false}}`))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeK3sMCP) config(t *testing.T, gateway *fakeGateway) sessionMCPConfig {
	t.Helper()
	return sessionMCPConfig{
		gateway: gateway.gate(t),
		k3sMCP:  newK3sMCPClient(f.server.URL + "/mcp"),
		notices: newNoticeFeed(),
	}
}

// mustJSON is the listing a test asserts on.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The re-export's tools are on the surface only when the deployment has a K3s
// MCP proxy beside the session MCP.
func TestK3sMCPToolsAreListedOnlyWithAProxy(t *testing.T) {
	gateway := newFakeGateway(t, "APPROVED")
	without := newSessionMCPServerWith(t, sessionMCPConfig{gateway: gateway.gate(t), notices: newNoticeFeed()})
	_, body := postMCP(t, without, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	for _, tool := range []string{k3sAPIResourcesTool, k3sResourceListTool, k3sResourceGetTool} {
		if strings.Contains(mustJSON(t, body), tool) {
			t.Fatalf("tools/list names %s without a K3s MCP proxy", tool)
		}
	}

	upstream := newFakeK3sMCP(t, "pods")
	with := newSessionMCPServerWith(t, upstream.config(t, newFakeGateway(t, "APPROVED")))
	_, body = postMCP(t, with, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listed := mustJSON(t, body)
	for _, tool := range []string{k3sAPIResourcesTool, k3sResourceListTool, k3sResourceGetTool} {
		if !strings.Contains(listed, tool) {
			t.Fatalf("tools/list does not name %s: %s", tool, listed)
		}
	}
}

// An approved call reaches the K3s MCP, and the answer comes back.
func TestK3sMCPResourceListDelegatesAfterApproval(t *testing.T) {
	upstream := newFakeK3sMCP(t, "name: pod-a")
	server := newSessionMCPServerWith(t, upstream.config(t, newFakeGateway(t, "APPROVED")))

	_, body := postMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sResourceListTool+`","arguments":{"apiVersion":"v1","kind":"Pod","namespace":"default"}}}`)
	_, isError, text := toolResult(t, body)
	if isError {
		t.Fatalf("%s failed: %s", k3sResourceListTool, text)
	}
	if !strings.Contains(text, "pod-a") {
		t.Fatalf("result = %s, want the K3s MCP answer", text)
	}
	if upstream.calls.Load() != 1 {
		t.Fatalf("the K3s MCP was called %d times, want 1", upstream.calls.Load())
	}
	if len(upstream.names) != 1 || upstream.names[0] != k3sMCPResourceListTool {
		t.Fatalf("the K3s MCP saw %v, want the resource_list tool", upstream.names)
	}
}

// A refused call never reaches the K3s MCP.
func TestK3sMCPResourceListStaysInsideTheGate(t *testing.T) {
	upstream := newFakeK3sMCP(t, "pod-a")
	server := newSessionMCPServerWith(t, upstream.config(t, newFakeGateway(t, "REFUSED")))

	_, body := postMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sResourceListTool+`","arguments":{"apiVersion":"v1","kind":"Pod"}}}`)
	_, isError, text := toolResult(t, body)
	if !isError {
		t.Fatalf("a refused %s answered as a success: %s", k3sResourceListTool, text)
	}
	if upstream.calls.Load() != 0 {
		t.Fatalf("the K3s MCP was called %d times after a refusal, want 0", upstream.calls.Load())
	}
}

// The arguments are checked before the gate, so a request that does not fit
// the tool never becomes something a person is asked about (R3).
func TestK3sMCPResourceListChecksItsArgumentsFirst(t *testing.T) {
	upstream := newFakeK3sMCP(t, "pod-a")
	gateway := newFakeGateway(t, "APPROVED")
	server := newSessionMCPServerWith(t, upstream.config(t, gateway))

	_, body := postMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sResourceListTool+`","arguments":{"apiVersion":"v1"}}}`)
	_, isError, text := toolResult(t, body)
	if !isError {
		t.Fatalf("a %s without a kind answered as a success: %s", k3sResourceListTool, text)
	}
	if upstream.calls.Load() != 0 {
		t.Fatalf("the K3s MCP was called %d times, want 0", upstream.calls.Load())
	}
	if gateway.created.Load() != 0 {
		t.Fatalf("the gate was asked %d times for an invalid request, want 0", gateway.created.Load())
	}
}

// The read slice stays a read slice: a write subresource is not on the
// surface, and the client cannot ask for one.
func TestK3sMCPResourceGetRefusesAWriteSubresource(t *testing.T) {
	upstream := newFakeK3sMCP(t, "scaled")
	gateway := newFakeGateway(t, "APPROVED")
	server := newSessionMCPServerWith(t, upstream.config(t, gateway))

	_, body := postMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sResourceGetTool+`","arguments":{"apiVersion":"autoscaling","kind":"Deployment","name":"web","subresource":"scale"}}}`)
	_, isError, text := toolResult(t, body)
	if !isError {
		t.Fatalf("a write subresource answered as a success: %s", text)
	}
	if upstream.calls.Load() != 0 {
		t.Fatalf("the K3s MCP was called %d times, want 0", upstream.calls.Load())
	}
}

// The client refuses the upstream's own refusal as a tool failure, so the
// agent reads one shape.
func TestK3sMCPReportsAnUpstreamRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"no such kind"}],"isError":true}}`))
	}))
	t.Cleanup(server.Close)
	gateway := newFakeGateway(t, "APPROVED")
	mcp := newSessionMCPServerWith(t, sessionMCPConfig{
		gateway: gateway.gate(t),
		k3sMCP:  newK3sMCPClient(server.URL + "/mcp"),
		notices: newNoticeFeed(),
	})

	_, body := postMCP(t, mcp, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sAPIResourcesTool+`","arguments":{}}}`)
	_, isError, text := toolResult(t, body)
	if !isError {
		t.Fatalf("an upstream refusal answered as a success: %s", text)
	}
	if !strings.Contains(text, "no such kind") {
		t.Fatalf("result = %s, want the upstream reason", text)
	}
}

// The tool takes no arguments, so an argument is refused before the gate.
func TestK3sMCPAPIResourcesTakesNoArguments(t *testing.T) {
	upstream := newFakeK3sMCP(t, "Pod")
	gateway := newFakeGateway(t, "APPROVED")
	server := newSessionMCPServerWith(t, upstream.config(t, gateway))

	_, body := postMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sAPIResourcesTool+`","arguments":{"apiVersion":"v1"}}}`)
	_, isError, _ := toolResult(t, body)
	if !isError {
		t.Fatal("an argument the tool does not have answered as a success")
	}
	if upstream.calls.Load() != 0 || gateway.created.Load() != 0 {
		t.Fatalf("the call reached the K3s MCP (%d) or the gate (%d), want neither",
			upstream.calls.Load(), gateway.created.Load())
	}
}

// The answer is bounded by the same rule every other tool result follows.
func TestK3sMCPResultNamesTheBodyItCarries(t *testing.T) {
	upstream := newFakeK3sMCP(t, "pod-a")
	server := newSessionMCPServerWith(t, upstream.config(t, newFakeGateway(t, "APPROVED")))

	_, body := postMCP(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sAPIResourcesTool+`","arguments":{}}}`)
	_, isError, text := toolResult(t, body)
	if isError {
		t.Fatalf("%s failed: %s", k3sAPIResourcesTool, text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("the result is not JSON: %v", err)
	}
	if payload["tool"] != k3sAPIResourcesTool {
		t.Fatalf("result = %v, want it to name the tool", payload)
	}
	if payload["body"] != "pod-a" {
		t.Fatalf("result body = %v, want the upstream answer", payload["body"])
	}
}

// The K3s MCP is called with the arguments the tool was given, not with the
// ones the client sent.
func TestK3sMCPForwardsOnlyItsOwnArguments(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		seen = call.Params.Arguments
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"logs"}],"isError":false}}`))
	}))
	t.Cleanup(server.Close)
	gateway := newFakeGateway(t, "APPROVED")
	mcp := newSessionMCPServerWith(t, sessionMCPConfig{
		gateway: gateway.gate(t),
		k3sMCP:  newK3sMCPClient(server.URL + "/mcp"),
		notices: newNoticeFeed(),
	})

	_, body := postMCP(t, mcp, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+
		k3sResourceGetTool+`","arguments":{"apiVersion":"v1","kind":"Pod","namespace":"default","name":"web","subresource":"log","tailLines":50}}}`)
	if _, isError, text := toolResult(t, body); isError {
		t.Fatalf("%s failed: %s", k3sResourceGetTool, text)
	}
	if seen["name"] != "web" || seen["subresource"] != "log" || seen["tailLines"] != float64(50) {
		t.Fatalf("the K3s MCP saw %v, want the read arguments", seen)
	}
	if _, ok := seen["previous"]; ok {
		t.Fatalf("the K3s MCP saw %v, want no argument the tool did not take", seen)
	}
}

// The client's context is cancelled with the call, so a hung upstream does
// not outlive the session.
func TestK3sMCPClientHonoursTheContext(t *testing.T) {
	upstream := newFakeK3sMCP(t, "pods")
	client := newK3sMCPClient(upstream.server.URL + "/mcp")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.call(ctx, k3sMCPAPIResourcesTool, nil); err == nil {
		t.Fatal("a cancelled call answered as a success")
	}
}
