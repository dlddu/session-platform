package main

import (
	"bytes"
	"errors"
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

func newCredentialProxy(rawUpstream, token string, logger *slog.Logger) (*credentialProxy, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A provider endpoint is an explicitly configured security boundary. Never
	// let ambient HTTP_PROXY/HTTPS_PROXY variables redirect the real credential.
	transport.Proxy = nil
	return newCredentialProxyWithTransport(rawUpstream, token, logger, transport)
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
		// The configured upstream is the only possible destination and virtual
		// host. Only documented Anthropic/Claude Code protocol and telemetry
		// headers survive; client-controlled routing/auth headers never reach it.
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

func validateCredentialProxyBindAddr(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("credential proxy address must include a loopback host and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("credential proxy must bind to a loopback IP")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("credential proxy port is invalid")
	}
	return nil
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

func redactCredentialProxyHeaders(header http.Header, token string) {
	for name, values := range header {
		for index, value := range values {
			values[index] = strings.ReplaceAll(value, token, redactedLiteral)
		}
		header[name] = values
	}
}
