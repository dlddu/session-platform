package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestK3SMCPOriginKeepsOnlySchemeAndHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "https://homelab-k3s-mcp.llkm.nl"},
		{"https://k3s.example/mcp", "https://k3s.example"},
		{"https://k3s.example:8443/a/b?q=1", "https://k3s.example:8443"},
	} {
		got, err := k3sMCPOrigin(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("k3sMCPOrigin(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"http://k3s.example/mcp", "https://user:pw@k3s.example/mcp", "://", "https:///mcp"} {
		if _, err := k3sMCPOrigin(bad); err == nil {
			t.Errorf("k3sMCPOrigin(%q) accepted; the token must only travel over https to a named host", bad)
		}
	}
}

// AC-G5: with the optional Secret key unset the sidecar still starts and
// reports itself unconfigured, so the pod runs and the entrypoint skips the
// plugin instead of failing.
func TestK3SMCPProxyWithoutTokenReportsUnconfigured(t *testing.T) {
	handler, err := newK3SMCPProxy("", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newK3SMCPProxy: %v", err)
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil || health.Code != http.StatusOK {
		t.Fatalf("healthz = %d %q (%v)", health.Code, health.Body.String(), err)
	}
	if body["configured"] != false || body["workload"] != workloadK3SMCPProxy {
		t.Fatalf("healthz body = %v, want unconfigured %s", body, workloadK3SMCPProxy)
	}
	call := httptest.NewRecorder()
	handler.ServeHTTP(call, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")))
	if call.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured MCP call = %d, want 503", call.Code)
	}
}

func TestK3SMCPProxyInjectsBearerAndKeepsTheMCPSurface(t *testing.T) {
	const token = "k3s-secret-token"
	var got *http.Request
	var gotBody string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-1")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"echo":"` + token + `"}}`))
	}))
	defer upstream.Close()

	handler, err := newK3SMCPProxyWithTransport(upstream.URL+"/ignored-path", token,
		slog.New(slog.NewTextHandler(io.Discard, nil)), upstream.Client().Transport)
	if err != nil {
		t.Fatalf("newK3SMCPProxyWithTransport: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Session-Id", "session-1")
	request.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	request.Header.Set("Authorization", "Bearer caller-supplied")
	request.Header.Set("X-Forwarded-Host", "elsewhere.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body %q", response.Code, response.Body.String())
	}
	if got == nil {
		t.Fatal("upstream was never called")
	}
	if got.URL.Path != "/mcp" {
		t.Errorf("upstream path = %q, want the client's /mcp (the configured path must not be prepended)", got.URL.Path)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer "+token {
		t.Errorf("upstream Authorization = %q, want the sidecar's bearer", auth)
	}
	for _, name := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version", "Accept", "Content-Type"} {
		if got.Header.Get(name) == "" {
			t.Errorf("upstream lost MCP header %s", name)
		}
	}
	if got.Header.Get("X-Forwarded-Host") != "" {
		t.Error("a header outside the MCP allowlist reached the upstream")
	}
	if gotBody != `{"jsonrpc":"2.0","id":1}` {
		t.Errorf("upstream body = %q", gotBody)
	}
	if response.Header().Get("Mcp-Session-Id") != "session-1" {
		t.Error("the MCP session id did not come back to the client")
	}
	if strings.Contains(response.Body.String(), token) {
		t.Error("the token reached the client in the response body")
	}
}
