package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const maxCredentialProxyResponseBytes = int64(64 << 20)

var credentialProxyAuthHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"X-Api-Key",
	"X-Auth-Token",
}

type credentialProxy struct {
	handler http.Handler
}

// credentialProxyResponseWriter suppresses all upstream informational
// responses. net/http/httputil forwards 1xx headers before ModifyResponse, so
// letting them reach the real writer would bypass the credential scrub applied
// to the final response.
type credentialProxyResponseWriter struct {
	http.ResponseWriter
}

func (w *credentialProxyResponseWriter) WriteHeader(statusCode int) {
	if statusCode >= 100 && statusCode < 200 {
		clear(w.Header())
		return
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

// Unwrap preserves streaming and other optional ResponseWriter operations used
// by ReverseProxy through http.ResponseController.
func (w *credentialProxyResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (p *credentialProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.handler.ServeHTTP(w, r)
}

// providerCACertEnv optionally carries a PEM bundle the proxy must trust in
// addition to the system roots, for a deployment whose provider `base-url` is a
// private gateway issued by a CA no public store knows. It is an address-class
// setting, not a credential: a CA certificate is public by construction, and
// the platform Secret key behind it is optional exactly like `k3s-mcp-url` —
// omit it and the proxy keeps the system pool it has always used. The kind e2e
// SUT is the first such deployment (docs/test/e2e.md `CLAUDE-PROVIDER`).
const providerCACertEnv = "ANTHROPIC_CA_CERT"

func newCredentialProxy(rawUpstream, token, caPEM string, logger *slog.Logger) (*credentialProxy, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A provider endpoint is an explicitly configured security boundary. Never
	// let ambient HTTP_PROXY/HTTPS_PROXY variables redirect the real credential.
	transport.Proxy = nil
	if err := trustProviderCA(transport, caPEM); err != nil {
		return nil, err
	}
	return newCredentialProxyWithTransport(rawUpstream, token, logger, transport)
}

// trustProviderCA adds pem to the transport's roots. It *appends to* the system
// pool rather than replacing it: a private gateway is an addition to the set of
// trustworthy issuers, not a reason to stop trusting the public ones, and
// replacing the pool would silently break any deployment that later moves back
// to a publicly issued endpoint. An empty pem leaves the transport untouched so
// the default path keeps using the system pool the standard library resolves
// lazily. A non-empty pem that yields no certificate is a configuration error
// and fails loudly here rather than at the first request: a proxy that silently
// ignored it would fall back to system-only trust and reject every call to the
// gateway it was configured for.
func trustProviderCA(transport *http.Transport, pem string) error {
	if pem == "" {
		return nil
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("credential proxy could not load system CA pool: %w", err)
	}
	if !roots.AppendCertsFromPEM([]byte(pem)) {
		return errors.New("credential proxy CA bundle contains no certificate")
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.RootCAs = roots
	return nil
}

// newCredentialProxyWithTransport is the test seam for TLS servers with an
// ephemeral CA. Production always enters through newCredentialProxy above.
func newCredentialProxyWithTransport(
	rawUpstream string,
	token string,
	logger *slog.Logger,
	transport http.RoundTripper,
) (*credentialProxy, error) {
	upstream, err := parseCredentialProxyUpstream(rawUpstream)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("credential proxy auth token is required")
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("credential proxy auth token contains invalid characters")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if transport == nil {
		return nil, errors.New("credential proxy transport is required")
	}

	reverseProxy := &httputil.ReverseProxy{}
	reverseProxy.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		request := proxyRequest.Out
		// Rewrite runs after ReverseProxy removes hop-by-hop headers. This is
		// important for credentials: an inbound `Connection: Authorization`
		// must not remove the platform header after we install it.
		proxyRequest.SetURL(upstream)
		// The configured upstream is the only possible destination and virtual host.
		request.Host = upstream.Host
		for name := range request.Header {
			if !credentialProxyRequestHeaderAllowed(name) {
				request.Header.Del(name)
			}
		}
		// Claude API requests do not use HTTP trailers. In particular, never let
		// a caller smuggle a denied routing or authentication field in a trailer.
		request.Trailer = nil
		request.Header.Set("Authorization", "Bearer "+token)
		// Make literal response redaction possible even if the caller requested
		// compression. The fixed gateway is expected to honor identity encoding.
		request.Header.Set("Accept-Encoding", "identity")
	}
	reverseProxy.Transport = transport
	reverseProxy.ErrorLog = log.New(io.Discard, "", 0)
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		logger.Warn("credential proxy upstream request failed")
		http.Error(w, "credential proxy upstream unavailable", http.StatusBadGateway)
	}
	reverseProxy.ModifyResponse = func(response *http.Response) error {
		// An unsolicited protocol upgrade is never part of the Claude HTTP API.
		// Reject it before ReverseProxy can hijack the client connection.
		if response.StatusCode >= 100 && response.StatusCode < 200 {
			_ = response.Body.Close()
			return errors.New("credential proxy upstream returned an informational final response")
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			_ = response.Body.Close()
			return errors.New("credential proxy upstream returned unsupported content encoding")
		}
		if credentialProxyStreamsResponse(response) {
			scrubCredentialProxyResponseMetadata(response, token)
			response.Body = newCredentialProxyStreamingBody(
				response.Body,
				token,
				maxCredentialProxyResponseBytes,
				func() { scrubCredentialProxyResponseMetadata(response, token) },
			)
			response.ContentLength = -1
			response.Header.Del("Content-Length")
			response.Header.Del("Content-Encoding")
			return nil
		}

		body, err := io.ReadAll(io.LimitReader(response.Body, maxCredentialProxyResponseBytes+1))
		closeErr := response.Body.Close()
		if err != nil {
			return errors.New("credential proxy could not read upstream response")
		}
		if closeErr != nil {
			return errors.New("credential proxy could not close upstream response")
		}
		if int64(len(body)) > maxCredentialProxyResponseBytes {
			return errors.New("credential proxy upstream response is too large")
		}
		body = bytes.ReplaceAll(body, []byte(token), []byte(redactedLiteral))
		scrubCredentialProxyResponseMetadata(response, token)
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		response.Header.Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
		response.Header.Del("Content-Encoding")
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","workload":"credential-proxy"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Method, http.MethodConnect) || credentialProxyUpgradeRequested(r.Header) {
			http.Error(w, "credential proxy does not support tunnels or protocol upgrades", http.StatusMethodNotAllowed)
			return
		}
		reverseProxy.ServeHTTP(&credentialProxyResponseWriter{ResponseWriter: w}, r)
	})
	return &credentialProxy{handler: mux}, nil
}

// credentialProxyRequestHeaderAllowed is deliberately a positive, immutable
// list. The first group is the public Anthropic API contract, the second is the
// Claude Code gateway attribution contract, and the final group is the
// explicit metadata emitted by Claude Code's bundled Stainless client.
func credentialProxyRequestHeaderAllowed(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept",
		"Content-Type",
		"Anthropic-Version",
		"Anthropic-Beta",
		"Anthropic-Dangerous-Direct-Browser-Access",
		"User-Agent",
		"X-App",
		"X-Client-Request-Id",
		"X-Claude-Code-Session-Id",
		"X-Claude-Code-Agent-Id",
		"X-Claude-Code-Parent-Agent-Id",
		"X-Stainless-Arch",
		"X-Stainless-Helper-Method",
		"X-Stainless-Lang",
		"X-Stainless-Os",
		"X-Stainless-Package-Version",
		"X-Stainless-Read-Timeout",
		"X-Stainless-Retry-Count",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Timeout":
		return true
	default:
		return false
	}
}

func credentialProxyUpgradeRequested(header http.Header) bool {
	if header.Get("Upgrade") != "" {
		return true
	}
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func parseCredentialProxyUpstream(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("credential proxy upstream URL is required")
	}
	upstream, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("credential proxy upstream URL is invalid")
	}
	if upstream.Scheme != "https" {
		return nil, errors.New("credential proxy upstream URL must use https")
	}
	if upstream.Host == "" || upstream.User != nil || upstream.Opaque != "" ||
		upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("credential proxy upstream URL must be an absolute origin with an optional path")
	}
	return upstream, nil
}

func validateCredentialProxyBindAddr(addr string, placement credentialProxyPlacement) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("credential proxy address must include a host and port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("credential proxy port is invalid")
	}
	ip := net.ParseIP(host)
	switch placement {
	case proxyPlacementSidecar:
		if ip == nil || !ip.IsLoopback() {
			return errors.New("sidecar credential proxy must bind to a loopback IP")
		}
	case proxyPlacementHelper:
		// The helper placement's only client is the *workload* pod, one network
		// hop away (AC-F6), so this proxy binds the pod network instead. A
		// loopback bind here is not a safety win but an outage: nothing outside
		// the helper pod could reach it. What keeps the wider bind safe is
		// AC-F2's ingress policy.
		if host == "" {
			return nil // ":8091" — the unspecified address, every pod interface
		}
		if ip == nil {
			return errors.New("helper credential proxy must bind to an IP")
		}
		if ip.IsLoopback() {
			return errors.New("helper credential proxy must not bind to loopback; its client is a pod away")
		}
	default:
		return fmt.Errorf("unknown credential proxy placement %q", placement)
	}
	return nil
}

// credentialProxyPlacement is where this proxy container runs; one behaviour
// contract (AC-E6), two placements (AC-F6). It is a type so that opening the
// bind for the helper can never silently open it for a sidecar as well.
type credentialProxyPlacement string

const (
	proxyPlacementSidecar credentialProxyPlacement = "sidecar"
	proxyPlacementHelper  credentialProxyPlacement = "helper"
)

// credentialProxyPlacementFromEnv reads the placement the control plane
// declared. It fails closed: an unset value is the restrictive sidecar
// placement, and an unrecognised one is an error rather than a default.
func credentialProxyPlacementFromEnv() (credentialProxyPlacement, error) {
	switch value := os.Getenv(proxyPlacementEnv); value {
	case "", string(proxyPlacementSidecar):
		return proxyPlacementSidecar, nil
	case string(proxyPlacementHelper):
		return proxyPlacementHelper, nil
	default:
		return "", fmt.Errorf("unknown credential proxy placement %q", value)
	}
}

func validateClaudeProxyClientEnv() error {
	if os.Getenv("ANTHROPIC_BASE_URL") != claudeProxyBaseURL {
		return errors.New("claude-code must use the localhost credential proxy URL")
	}
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != claudeProxyPlaceholderToken {
		return errors.New("claude-code must use the non-secret credential proxy placeholder")
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return errors.New("claude-code container must not receive direct credentials")
	}
	return nil
}

// validateApprovalGatedClientEnv is the approval-gated counterpart: the workload
// pod holds no external credential at all (AC-F6). It returns the session MCP
// URL, which is this type's only tool surface that leaves the pod.
func validateApprovalGatedClientEnv() (string, error) {
	if err := validateHelperEndpoint(os.Getenv("ANTHROPIC_BASE_URL"), helperProxyPort); err != nil {
		return "", fmt.Errorf("approval-gated provider endpoint: %w", err)
	}
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != claudeProxyPlaceholderToken {
		return "", errors.New("approval-gated workload must use the non-secret credential proxy placeholder")
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return "", errors.New("approval-gated container must not receive direct credentials")
	}
	if os.Getenv("K3S_MCP_TOKEN") != "" {
		// AC-F6 (2026-09-03 decision): this type has no runtime plugin
		// bootstrap, so the token that bootstrap needed must not be here either.
		return "", errors.New("approval-gated container must not receive the K3s MCP token")
	}
	mcpURL := os.Getenv(sessionMCPURLEnv)
	if err := validateHelperEndpoint(mcpURL, sessionMCPPort); err != nil {
		return "", fmt.Errorf("approval-gated session MCP endpoint: %w", err)
	}
	return mcpURL, nil
}

// validateHelperEndpoint checks one of the two in-cluster addresses the control
// plane injects into an approval-gated workload pod. A loopback value would mean
// the proxy or MCP had been folded back into this pod, which is exactly the
// arrangement AC-F2 moved away from.
func validateHelperEndpoint(raw string, wantPort int) error {
	if raw == "" {
		return errors.New("must be set")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a URL")
	}
	if parsed.Scheme != "http" {
		return errors.New("must be an in-cluster http URL")
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return errors.New("must include a host and port")
	}
	if port, err := strconv.Atoi(portText); err != nil || port != wantPort {
		return fmt.Errorf("must use port %d", wantPort)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return errors.New("must not be loopback; the helper pod is a network hop away")
	}
	if host == "" {
		return errors.New("must include a host")
	}
	return nil
}

func redactCredentialProxyHeaders(header http.Header, token string) {
	for name, values := range header {
		for index, value := range values {
			values[index] = strings.ReplaceAll(value, token, redactedLiteral)
		}
		header[name] = values
	}
}
