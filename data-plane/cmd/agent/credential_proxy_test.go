package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type credentialProxyRequest struct {
	headers            http.Header
	host               string
	path               string
	query              string
	body               string
	authorization      string
	apiKey             string
	proxyAuthorization string
	forwardedHost      string
	originalURL        string
	acceptEncoding     string
}

func credentialProxyServer(
	t *testing.T,
	upstream string,
	token string,
	logs *bytes.Buffer,
	transport http.RoundTripper,
) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	var proxy *credentialProxy
	var err error
	if transport == nil {
		proxy, err = newCredentialProxy(upstream, token, "", logger)
	} else {
		proxy, err = newCredentialProxyWithTransport(upstream, token, logger, transport)
	}
	if err != nil {
		t.Fatalf("new credential proxy: %v", err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server
}

func TestCredentialProxyHealthDoesNotReachUpstream(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamHits.Add(1)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, "real-secret", &logs, upstream.Client().Transport)

	resp, err := http.Get(proxy.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d err=%v", resp.StatusCode, readErr)
	}
	if !strings.Contains(string(body), `"credential-proxy"`) || upstreamHits.Load() != 0 {
		t.Fatalf("health body=%q upstream hits=%d", body, upstreamHits.Load())
	}
}

func TestCredentialProxyPinsTargetOverwritesAuthAndRedactsResponse(t *testing.T) {
	const token = "real-platform-secret"
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		evilHits.Add(1)
	}))
	defer evil.Close()

	observed := make(chan credentialProxyRequest, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- credentialProxyRequest{
			headers:            r.Header.Clone(),
			host:               r.Host,
			path:               r.URL.Path,
			query:              r.URL.RawQuery,
			body:               string(body),
			authorization:      r.Header.Get("Authorization"),
			apiKey:             r.Header.Get("X-Api-Key"),
			proxyAuthorization: r.Header.Get("Proxy-Authorization"),
			forwardedHost:      r.Header.Get("X-Forwarded-Host"),
			originalURL:        r.Header.Get("X-Original-URL"),
			acceptEncoding:     r.Header.Get("Accept-Encoding"),
		}
		w.Header().Set("Authorization", "Bearer "+token)
		w.Header().Set("X-Api-Key", token)
		w.Header().Set("X-Auth-Token", token)
		w.Header().Set("X-Upstream-Echo", "prefix-"+token)
		_, _ = io.WriteString(w, "upstream echoed "+token)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL+"/anthropic", token, &logs, upstream.Client().Transport)

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages?beta=1", strings.NewReader("request-body"))
	if err != nil {
		t.Fatal(err)
	}
	evilURL, _ := url.Parse(evil.URL)
	req.Host = evilURL.Host
	req.Header.Set("Authorization", "Bearer attacker")
	req.Header.Set("X-Api-Key", "attacker-key")
	req.Header.Set("Proxy-Authorization", "attacker-proxy")
	req.Header.Set("X-Forwarded-Host", evilURL.Host)
	req.Header.Set("X-Forwarded-Uri", "/steal")
	req.Header.Set("X-Original-URL", evil.URL+"/steal")
	req.Header.Set("X-Envoy-Original-Path", "/steal")
	req.Header.Set("X-Host", evilURL.Host)
	req.Header.Set("X-HTTP-Method-Override", "CONNECT")
	req.Header.Set("X-Custom-Api-Key", "attacker-key")
	req.Header.Set("Cookie", "credential=attacker")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("User-Agent", "claude-cli/test")
	req.Header.Set("X-Claude-Code-Session-Id", "session-id")
	req.Header.Set("X-Claude-Code-Agent-Id", "agent-id")
	req.Header.Set("X-Claude-Code-Parent-Agent-Id", "parent-agent-id")
	req.Header.Set("X-Stainless-Retry-Count", "1")
	req.Header.Set("Connection", "Authorization, X-Api-Key")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status=%d err=%v body=%q", resp.StatusCode, readErr, body)
	}

	got := <-observed
	upstreamURL, _ := url.Parse(upstream.URL)
	if got.host != upstreamURL.Host || got.path != "/anthropic/v1/messages" || got.query != "beta=1" {
		t.Fatalf("upstream target = host %q path %q query %q", got.host, got.path, got.query)
	}
	if got.body != "request-body" {
		t.Fatalf("upstream body=%q", got.body)
	}
	if got.authorization != "Bearer "+token {
		t.Fatalf("upstream authorization=%q", got.authorization)
	}
	for _, name := range []string{
		"Accept", "Content-Type", "Anthropic-Version", "Anthropic-Beta", "User-Agent",
		"X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id",
		"X-Claude-Code-Parent-Agent-Id", "X-Stainless-Retry-Count",
	} {
		if got.headers.Get(name) == "" {
			t.Errorf("allowed request header %s was stripped", name)
		}
	}
	for _, name := range []string{
		"X-Api-Key", "Proxy-Authorization", "X-Forwarded-Host", "X-Forwarded-Uri",
		"X-Original-URL", "X-Envoy-Original-Path", "X-Host", "X-HTTP-Method-Override",
		"X-Custom-Api-Key", "Cookie",
	} {
		if value := got.headers.Get(name); value != "" {
			t.Errorf("denied request header %s survived: %q", name, value)
		}
	}
	for name := range got.headers {
		if name == "Authorization" || name == "Accept-Encoding" || name == "Content-Length" ||
			credentialProxyRequestHeaderAllowed(name) {
			continue
		}
		t.Errorf("header outside positive allowlist reached upstream: %s", name)
	}
	if got.apiKey != "" || got.proxyAuthorization != "" || got.forwardedHost != "" || got.originalURL != "" {
		t.Fatalf("client-controlled headers survived: %+v", got)
	}
	if got.acceptEncoding != "identity" {
		t.Fatalf("upstream Accept-Encoding=%q, want identity", got.acceptEncoding)
	}
	if evilHits.Load() != 0 {
		t.Fatalf("attacker target received %d requests", evilHits.Load())
	}
	if strings.Contains(string(body), token) || strings.Contains(resp.Header.Get("X-Upstream-Echo"), token) {
		t.Fatalf("credential leaked in response body=%q header=%q", body, resp.Header.Get("X-Upstream-Echo"))
	}
	for _, name := range credentialProxyAuthHeaders {
		if value := resp.Header.Get(name); value != "" {
			t.Fatalf("credential response header %s survived: %q", name, value)
		}
	}
	if !strings.Contains(string(body), redactedLiteral) ||
		!strings.Contains(resp.Header.Get("X-Upstream-Echo"), redactedLiteral) {
		t.Fatalf("credential was not redacted body=%q header=%q", body, resp.Header.Get("X-Upstream-Echo"))
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("credential leaked in proxy logs: %s", logs.String())
	}
}

func TestCredentialProxySuppressesInformationalResponseHeaders(t *testing.T) {
	const token = "interim-response-secret"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Authorization", "Bearer "+token)
		w.Header().Set("X-Echo", token)
		w.WriteHeader(http.StatusEarlyHints)
		// net/http deliberately retains headers after a 1xx response. Clear the
		// interim values so this test isolates the early-response leak path.
		clear(w.Header())
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, token, &logs, upstream.Client().Transport)
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /v1/messages HTTP/1.1\r\nHost: proxy\r\nConnection: close\r\n\r\n"); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	rawResponse, readErr := io.ReadAll(conn)
	conn.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Contains(rawResponse, []byte("200 OK")) || !bytes.HasSuffix(rawResponse, []byte("ok")) {
		t.Fatalf("final response missing: %q", rawResponse)
	}
	if bytes.Contains(rawResponse, []byte("103 Early Hints")) || bytes.Contains(rawResponse, []byte(token)) {
		t.Fatalf("informational response credential reached client: %q", rawResponse)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("credential leaked in proxy logs: %s", logs.String())
	}
}

func TestCredentialProxyRejectsConnectAndUpgrade(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamHits.Add(1)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, "real-secret", &logs, upstream.Client().Transport)

	tests := []struct {
		name   string
		method string
		header http.Header
	}{
		{name: "connect", method: http.MethodConnect},
		{name: "upgrade header", method: http.MethodGet, header: http.Header{
			"Connection": []string{"keep-alive, Upgrade"},
			"Upgrade":    []string{"websocket"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, proxy.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = tc.header.Clone()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
			}
		})
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("rejected tunnel requests reached upstream %d times", hits)
	}
}

func TestCredentialProxyUpstreamFailureDoesNotLeakCredential(t *testing.T) {
	const token = "failure-path-secret"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "https://" + listener.Addr().String()
	_ = listener.Close()
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, target, token, &logs, nil)

	resp, err := http.Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if strings.Contains(string(body), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("credential leaked on failure body=%q logs=%q", body, logs.String())
	}
}

func TestCredentialProxyConfigurationValidation(t *testing.T) {
	for _, raw := range []string{
		"",
		"relative/path",
		"ftp://gateway.example",
		"http://gateway.example",
		"http://127.0.0.1:8443",
		"http://user:secret@gateway.example",
		"https://gateway.example?token=secret",
		"https://gateway.example#fragment",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := newCredentialProxy(raw, "real-secret", "", testLogger()); err == nil {
				t.Fatalf("upstream %q was accepted", raw)
			} else if strings.Contains(err.Error(), "real-secret") || strings.Contains(err.Error(), "user:secret") {
				t.Fatalf("validation error leaked configuration: %v", err)
			}
		})
	}
	for _, addr := range []string{"127.0.0.1:8091", "[::1]:8091"} {
		if err := validateCredentialProxyBindAddr(addr, proxyPlacementSidecar); err != nil {
			t.Fatalf("loopback address %q rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{":8091", "0.0.0.0:8091", "192.0.2.10:8091", "localhost:8091", "127.0.0.1:0"} {
		if err := validateCredentialProxyBindAddr(addr, proxyPlacementSidecar); err == nil {
			t.Fatalf("unsafe bind address %q accepted", addr)
		}
	}
}

func TestClaudeProxyClientEnvironmentRejectsDirectCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", claudeProxyBaseURL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if err := validateClaudeProxyClientEnv(); err != nil {
		t.Fatalf("proxy-only environment rejected: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-claude")
	if err := validateClaudeProxyClientEnv(); err == nil {
		t.Fatal("direct credential was accepted in claude-code container")
	}
}
