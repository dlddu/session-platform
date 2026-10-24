package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	// miniMaxSessionPluginName is the plugin the agent writes for the session
	// MCP (AC-G6). It is named after its server so it cannot collide with a
	// marketplace plugin.
	miniMaxSessionPluginName = sessionMCPServerName
	miniMaxPluginServersFile = "mcp/servers.json"

	// The helper pod is Ready before this pod is created, but its marketplace
	// clone may still be running; the wait covers that and a container restart.
	miniMaxSeedWait         = 90 * time.Second
	miniMaxSeedPollInterval = 2 * time.Second
	miniMaxSeedTimeout      = 10 * time.Second
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

// fetchMiniMaxSeed reads the K3s MCP proxy's plugin bundle, waiting out its
// bootstrap. A proxy that never answers is a broken helper pod and fails the
// boot; a bundle in the failed or unconfigured state is a session without
// marketplace plugins (AC-G5).
func fetchMiniMaxSeed(baseURL string) (miniMaxSeedBundle, error) {
	client := &http.Client{Timeout: miniMaxSeedTimeout}
	seedURL := strings.TrimRight(baseURL, "/") + miniMaxPluginSeedPath
	deadline := time.Now().Add(miniMaxSeedWait)
	var lastErr error
	for {
		bundle, retry, err := fetchMiniMaxSeedOnce(client, seedURL)
		if err == nil {
			return bundle, nil
		}
		lastErr = err
		if !retry || time.Now().After(deadline) {
			return miniMaxSeedBundle{}, fmt.Errorf("K3s MCP proxy plugin seed: %w", lastErr)
		}
		time.Sleep(miniMaxSeedPollInterval)
	}
}

func fetchMiniMaxSeedOnce(client *http.Client, seedURL string) (bundle miniMaxSeedBundle, retry bool, err error) {
	resp, err := client.Get(seedURL)
	if err != nil {
		return miniMaxSeedBundle{}, true, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		bundle, err := decodeMiniMaxSeedBundle(resp.Body)
		return bundle, false, err
	case http.StatusServiceUnavailable:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return miniMaxSeedBundle{}, true, errors.New("marketplace bootstrap still running")
	default:
		return miniMaxSeedBundle{}, false, fmt.Errorf("status %d", resp.StatusCode)
	}
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
// whatever an earlier boot left there: the session MCP plugin, then every
// plugin of the proxy's bundle with its MCP servers re-pointed (AC-G5, AC-G6).
// No plugin carries auth of its own; the helper pod holds every credential.
func stageMiniMaxPlugins(seedDir, sessionMCPURL, k3sProxyURL string, bundle []miniMaxSeedPlugin, logger *slog.Logger) error {
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
	if len(bundle) == 0 {
		return nil
	}
	proxyMCPURL, err := k3sProxyMCPURL(k3sProxyURL)
	if err != nil {
		return err
	}
	upstream := env(k3sMCPURLEnv, defaultK3SMCPURL)
	for _, plugin := range bundle {
		if plugin.Name == miniMaxSessionPluginName {
			return fmt.Errorf("marketplace plugin %s collides with the session MCP plugin", plugin.Name)
		}
		if err := writeMiniMaxSeedPlugin(seedDir, plugin); err != nil {
			return err
		}
		if err := repointMiniMaxPluginServers(filepath.Join(seedDir, plugin.Name), upstream, proxyMCPURL, logger); err != nil {
			return fmt.Errorf("seed plugin %s: %w", plugin.Name, err)
		}
	}
	return nil
}

// repointMiniMaxPluginServers rewrites every MCP server file a staged plugin
// declares. The K3s MCP server goes through the proxy with its own auth fields
// removed — mcode rejects OAuth fields and sends ${VAR} headers literally. Any
// other server is dropped: the workload pod cannot reach it (AC-G6), and it
// must never be routed through a proxy that holds someone else's token.
func repointMiniMaxPluginServers(pluginDir, upstream, proxyMCPURL string, logger *slog.Logger) error {
	var manifest struct {
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	data, err := os.ReadFile(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	var files []string
	if len(manifest.MCPServers) > 0 {
		var one string
		if json.Unmarshal(manifest.MCPServers, &one) == nil {
			files = []string{one}
		} else if err := json.Unmarshal(manifest.MCPServers, &files); err != nil {
			return errors.New("mcpServers must be file paths")
		}
	}
	for _, rel := range files {
		file, err := insideTree(pluginDir, filepath.FromSlash(rel))
		if err != nil {
			return fmt.Errorf("mcpServers %s: %w", rel, err)
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		rewritten, dropped, err := repointMiniMaxServers(content, upstream, proxyMCPURL)
		if err != nil {
			return fmt.Errorf("mcpServers %s: %w", rel, err)
		}
		for _, name := range dropped {
			logger.Warn("dropping a marketplace MCP server the minimax-code pod cannot reach",
				"plugin", filepath.Base(pluginDir), "server", name)
		}
		if err := os.WriteFile(file, rewritten, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func repointMiniMaxServers(content []byte, upstream, proxyMCPURL string) ([]byte, []string, error) {
	var doc map[string]any
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	kept := map[string]any{}
	var dropped []string
	for name, raw := range servers {
		server, ok := raw.(map[string]any)
		target, _ := server["url"].(string)
		if !ok || strings.TrimRight(target, "/") != strings.TrimRight(upstream, "/") {
			dropped = append(dropped, name)
			continue
		}
		delete(server, "oauthClientId")
		server["url"] = proxyMCPURL
		if headers, ok := server["headers"].(map[string]any); ok {
			for key := range headers {
				if strings.EqualFold(key, "authorization") {
					delete(headers, key)
				}
			}
			if len(headers) == 0 {
				delete(server, "headers")
			}
		}
		kept[name] = server
	}
	doc["mcpServers"] = kept
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append(out, '\n'), dropped, nil
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
			// mcode's own MCP tool-call default is 120 s, shorter than an approval.
			server: map[string]any{"type": "http", "url": serverURL, "timeout": sessionMCPToolTimeout.Milliseconds()},
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
