package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// minimax-code runs behind approval-gated's boundary (AC-G6): its workload pod
// reaches nothing but its session's helper pod, which serves the provider
// proxy, the session MCP and the K3s MCP proxy. This file is the agent's side
// of that arrangement — the three addresses it is given, and the MCP servers it
// registers with mcode from them.
const (
	// Keep in sync with control-plane/internal/adapter/k8s (K3SMCPProxyURLEnvVar).
	k3sMCPProxyURLEnv = "K3S_MCP_PROXY_URL"

	// The two platform plugins the agent stages for mcode (AC-G5, AC-G6). The
	// K3s one keeps claude-code's plugin and server names, so the model sees
	// the same mcp__k3s__* tool names in both types.
	miniMaxSessionPluginName = "session-platform"
	miniMaxK3SPluginName     = "mcp-servers"
	miniMaxK3SServerName     = "k3s"
	miniMaxPluginServersFile = "mcp/servers.json"

	// The helper pod is Ready before this pod is created, so the proxy answers
	// at once in practice; the retries only absorb a container restart.
	k3sProxyProbeAttempts = 15
	k3sProxyProbeInterval = time.Second
	k3sProxyProbeTimeout  = 3 * time.Second
)

// miniMaxEndpoints are the helper pod addresses the control plane injects.
type miniMaxEndpoints struct {
	provider   string
	sessionMCP string
	k3sProxy   string
}

// validateMiniMaxClientEnv is AC-G3's credential rule on AC-G6's placement:
// every address points at the helper pod, never loopback, and the CLI
// container holds no credential under any name either CLI would read.
func validateMiniMaxClientEnv() (miniMaxEndpoints, error) {
	endpoints := miniMaxEndpoints{
		provider:   os.Getenv("ANTHROPIC_BASE_URL"),
		sessionMCP: os.Getenv(sessionMCPURLEnv),
		k3sProxy:   os.Getenv(k3sMCPProxyURLEnv),
	}
	for _, check := range []struct {
		what, value string
		port        int
	}{
		{"provider endpoint", endpoints.provider, helperProxyPort},
		{"session MCP endpoint", endpoints.sessionMCP, sessionMCPPort},
		{"K3s MCP proxy endpoint", endpoints.k3sProxy, k3sMCPProxyPort},
	} {
		if err := validateHelperEndpoint(check.value, check.port); err != nil {
			return miniMaxEndpoints{}, fmt.Errorf("minimax-code %s: %w", check.what, err)
		}
	}
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != claudeProxyPlaceholderToken {
		return miniMaxEndpoints{}, errors.New("minimax-code must use the non-secret credential proxy placeholder")
	}
	for _, name := range miniMaxCredentialEnv {
		if os.Getenv(name) != "" {
			return miniMaxEndpoints{}, fmt.Errorf("minimax-code container must not receive %s", name)
		}
	}
	return endpoints, nil
}

// k3sProxyConfigured asks the K3s MCP proxy whether it holds a token. An
// unconfigured proxy is a deployment without the optional key, which means a
// session without the K3s tools, not a failed pod (AC-G5).
func k3sProxyConfigured(baseURL string) (bool, error) {
	client := &http.Client{Timeout: k3sProxyProbeTimeout}
	var lastErr error
	for attempt := 0; attempt < k3sProxyProbeAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(k3sProxyProbeInterval)
		}
		configured, err := probeK3SProxy(client, strings.TrimRight(baseURL, "/")+k3sMCPProxyHealthz)
		if err == nil {
			return configured, nil
		}
		lastErr = err
	}
	return false, fmt.Errorf("K3s MCP proxy health: %w", lastErr)
}

func probeK3SProxy(client *http.Client, healthURL string) (bool, error) {
	resp, err := client.Get(healthURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var health struct {
		Configured bool `json:"configured"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return health.Configured, nil
}

// k3sProxyMCPURL is the proxy address with the K3s MCP's own path, which the
// proxy forwards unchanged to the fixed upstream origin.
func k3sProxyMCPURL(proxyBase string) (string, error) {
	upstream, err := url.Parse(env(k3sMCPURLEnv, defaultK3SMCPURL))
	if err != nil {
		return "", fmt.Errorf("parse K3s MCP URL: %w", err)
	}
	path := upstream.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.TrimRight(proxyBase, "/") + path, nil
}

// stageMiniMaxPlugins writes the platform's plugins into seedDir, replacing
// whatever an earlier boot left there. Each is a Claude-format plugin with one
// HTTP MCP server and no auth of its own: the helper pod holds every
// credential. k3sURL is empty when the proxy has no token.
func stageMiniMaxPlugins(seedDir, sessionMCPURL, k3sURL string) error {
	if err := os.RemoveAll(seedDir); err != nil {
		return fmt.Errorf("clear minimax plugin seed: %w", err)
	}
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return fmt.Errorf("create minimax plugin seed: %w", err)
	}
	if err := writeMiniMaxPlugin(seedDir, miniMaxSessionPluginName,
		"Session MCP tools behind the session-platform approval gate.",
		sessionMCPServerName, sessionMCPURL); err != nil {
		return err
	}
	if k3sURL == "" {
		return nil
	}
	return writeMiniMaxPlugin(seedDir, miniMaxK3SPluginName,
		"Homelab K3s MCP tools through the session helper pod.",
		miniMaxK3SServerName, k3sURL)
}

func writeMiniMaxPlugin(seedDir, name, description, server, serverURL string) error {
	root := filepath.Join(seedDir, name)
	manifest := map[string]any{
		"name":        name,
		"description": description,
		"version":     "1.0.0",
		"author":      map[string]any{"name": "session-platform"},
		"mcpServers":  []string{"./" + miniMaxPluginServersFile},
	}
	servers := map[string]any{
		"mcpServers": map[string]any{
			server: map[string]any{"type": "http", "url": serverURL},
		},
	}
	for rel, doc := range map[string]any{
		filepath.Join(".claude-plugin", "plugin.json"): manifest,
		miniMaxPluginServersFile:                       servers,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create minimax plugin %s: %w", name, err)
		}
		content, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode minimax plugin %s: %w", name, err)
		}
		if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
			return fmt.Errorf("write minimax plugin %s: %w", name, err)
		}
	}
	return nil
}
