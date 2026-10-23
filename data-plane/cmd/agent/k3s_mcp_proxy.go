package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// The minimax-code workload reaches the homelab K3s MCP the way it reaches its
// model provider: through a helper pod container that holds the token (AC-G5,
// AC-G6). mcode expands no environment variable in a plugin's MCP headers, so a
// token the CLI could use would have to be written into HOME, which is
// archived. The proxy keeps that token out of the workload pod and out of every
// archive. It is not behind the session's approval gate: the K3s MCP gates its
// own writes, and a second gate would ask twice for the same action.
const (
	workloadK3SMCPProxy = "k3s-mcp-proxy"

	// Keep in sync with control-plane/internal/adapter/k8s (k3sMCPProxyPort).
	k3sMCPProxyPort    = 8093
	k3sMCPTokenEnv     = "K3S_MCP_TOKEN"
	k3sMCPURLEnv       = "K3S_MCP_URL"
	defaultK3SMCPURL   = "https://homelab-k3s-mcp.llkm.nl/mcp"
	k3sMCPProxyHealthz = "/healthz"
)

// k3sMCPProxyRequestHeaderAllowed is the MCP Streamable HTTP request surface.
// Anything else — above all a caller-supplied Authorization — is dropped before
// the proxy installs its own bearer.
func k3sMCPProxyRequestHeaderAllowed(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept",
		"Content-Type",
		"Last-Event-Id",
		"Mcp-Protocol-Version",
		"Mcp-Session-Id",
		"User-Agent":
		return true
	default:
		return false
	}
}

// newK3SMCPProxy serves the K3s MCP origin. An empty token is not a start
// failure: the Secret key is optional, and a deployment that has not set it
// keeps minimax-code sessions working without the K3s tools. The health route
// says which of the two it is, so the workload agent can leave the K3s server
// out rather than fail the pod.
func newK3SMCPProxy(rawURL, token string, logger *slog.Logger) (*k3sMCPProxy, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// As for the provider proxy: no ambient HTTP(S)_PROXY may redirect the token.
	transport.Proxy = nil
	return newK3SMCPProxyWithTransport(rawURL, token, logger, transport)
}

// k3sMCPProxy is the proxy's handler plus, when it holds a token, the
// marketplace bootstrap the caller starts (minimax_marketplace.go).
type k3sMCPProxy struct {
	http.Handler
	seeder *miniMaxMarketplaceSeeder
}

func newK3SMCPProxyWithTransport(rawURL, token string, logger *slog.Logger, transport http.RoundTripper) (*k3sMCPProxy, error) {
	origin, err := k3sMCPOrigin(rawURL)
	if err != nil {
		return nil, err
	}
	mcpPath, err := k3sMCPPath(rawURL)
	if err != nil {
		return nil, err
	}
	configured := token != ""
	var upstream http.Handler
	if configured {
		proxy, err := newHeaderScopedProxy(origin, token, logger, transport, k3sMCPProxyRequestHeaderAllowed)
		if err != nil {
			return nil, err
		}
		upstream = proxy
	}
	health, err := json.Marshal(map[string]any{
		"status":     "ok",
		"workload":   workloadK3SMCPProxy,
		"configured": configured,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(k3sMCPProxyHealthz, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(health)
	})
	result := &k3sMCPProxy{Handler: mux}
	if upstream != nil {
		result.seeder = newMiniMaxMarketplaceSeeder(upstream, mcpPath, logger)
		mux.HandleFunc(miniMaxPluginSeedPath, result.seeder.serveSeed)
	} else {
		mux.HandleFunc(miniMaxPluginSeedPath, serveUnconfiguredSeed)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if upstream == nil {
			http.Error(w, "k3s mcp proxy has no token configured", http.StatusServiceUnavailable)
			return
		}
		upstream.ServeHTTP(w, r)
	})
	return result, nil
}

// k3sMCPPath is the K3s MCP endpoint's path on its origin, which is where the
// marketplace bootstrap sends its own call.
func k3sMCPPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultK3SMCPURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("k3s mcp URL is invalid")
	}
	if parsed.EscapedPath() == "" {
		return "/", nil
	}
	return parsed.EscapedPath(), nil
}

// k3sMCPOrigin reduces the configured MCP URL to its origin. The client keeps
// the path (the K3s MCP's own path, on the proxy's address), so forwarding to the
// full URL would double it.
func k3sMCPOrigin(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultK3SMCPURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("k3s mcp URL is invalid")
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("k3s mcp URL must be an https URL without user info")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}
