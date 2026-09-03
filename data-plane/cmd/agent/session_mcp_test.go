package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postMCP(t *testing.T, srv *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+sessionMCPPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || resp.ContentLength == 0 {
		return resp.StatusCode, nil
	}
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, decoded
}

func newSessionMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(sessionMCPRoutes(testLogger()))
	t.Cleanup(srv.Close)
	return srv
}

// The helper pod cannot reach Ready without this, which is the whole reason an
// approval-gated session could not be created at all.
func TestSessionMCPReportsReady(t *testing.T) {
	srv := newSessionMCPServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}
}

func TestSessionMCPCompletesTheHandshake(t *testing.T) {
	srv := newSessionMCPServer(t)
	status, body := postMCP(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if status != http.StatusOK {
		t.Fatalf("initialize = %d, want 200", status)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize body = %v, want a result", body)
	}
	if got := result["protocolVersion"]; got != sessionMCPProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", got, sessionMCPProtocolVersion)
	}
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok || capabilities["tools"] == nil {
		t.Fatalf("capabilities = %v, want tools declared", result["capabilities"])
	}
	// Everything this server will ever offer has to pass the approval gate,
	// which is a tool-call boundary: no resources, no prompts (AC-F3).
	if capabilities["resources"] != nil || capabilities["prompts"] != nil {
		t.Fatalf("capabilities = %v, want tools only", capabilities)
	}
	if info, ok := result["serverInfo"].(map[string]any); !ok || info["name"] != sessionMCPServerName {
		t.Fatalf("serverInfo = %v, want %s", result["serverInfo"], sessionMCPServerName)
	}
}

// AC-F3 is not implemented, and this asserts that honestly rather than hiding
// it: an approval-gated agent that registers this server finds no tool at all,
// which is the safe end of the missing gate. When AC-F3 lands, this expectation
// is meant to change.
func TestSessionMCPOffersNoToolsUntilTheApprovalGateExists(t *testing.T) {
	srv := newSessionMCPServer(t)
	status, body := postMCP(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if status != http.StatusOK {
		t.Fatalf("tools/list = %d, want 200", status)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list body = %v, want a result", body)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %v, want a list", result["tools"])
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %v, want none while the approval gate is unimplemented", tools)
	}
}

func TestSessionMCPRejectsMalformedAndUnknownCalls(t *testing.T) {
	srv := newSessionMCPServer(t)
	for _, tc := range []struct {
		name string
		body string
		code float64
	}{
		{"not json", `{`, jsonRPCParseError},
		{"not jsonrpc 2.0", `{"jsonrpc":"1.0","id":1,"method":"initialize"}`, jsonRPCInvalidRequest},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, jsonRPCMethodNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postMCP(t, srv, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 with a JSON-RPC error body", status)
			}
			rpcErr, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("body = %v, want a JSON-RPC error", body)
			}
			if rpcErr["code"] != tc.code {
				t.Fatalf("error code = %v, want %v", rpcErr["code"], tc.code)
			}
			if body["result"] != nil {
				t.Fatalf("body carries both a result and an error: %v", body)
			}
		})
	}
}

// A notification has no id and must not be answered with a body, or the
// client's next read desynchronises.
func TestSessionMCPAcceptsNotificationsWithoutAnswering(t *testing.T) {
	srv := newSessionMCPServer(t)
	status, body := postMCP(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Fatalf("notification = %d, want 202", status)
	}
	if body != nil {
		t.Fatalf("notification answered with %v, want no body", body)
	}
}

func TestSessionMCPRejectsNonPost(t *testing.T) {
	srv := newSessionMCPServer(t)
	resp, err := http.Get(srv.URL + sessionMCPPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET %s = %d, want 405", sessionMCPPath, resp.StatusCode)
	}
}
