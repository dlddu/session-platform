package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedPlugin(t *testing.T, seed, name, serverJSON string) {
	t.Helper()
	root := filepath.Join(seed, name)
	for path, content := range map[string]string{
		".claude-plugin/plugin.json": `{"name":"` + name + `","mcpServers":["./mcp/k3s.json"]}`,
		"mcp/k3s.json":               serverJSON,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// AC-G5: the seed lands where mcode scans, tagged as the platform's.
func TestSyncMiniMaxPluginsInstallsTheSeed(t *testing.T) {
	home, seed := t.TempDir(), t.TempDir()
	writeSeedPlugin(t, seed, "mcp-servers", `{"v":1}`)

	if err := syncMiniMaxPlugins(home, seed); err != nil {
		t.Fatalf("sync: %v", err)
	}
	installed := filepath.Join(miniMaxPluginsDir(home), "mcp-servers")
	if got := readFile(t, filepath.Join(installed, "mcp", "k3s.json")); got != `{"v":1}` {
		t.Errorf("installed server file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(installed, miniMaxManagedPluginMarker)); err != nil {
		t.Errorf("installed plugin is not marked as managed: %v", err)
	}
}

// AC-G5: a restored HOME carries the previous pod's copy; the current seed
// replaces it whole, so a file the new seed no longer has does not linger.
func TestSyncMiniMaxPluginsReplacesAnArchivedCopy(t *testing.T) {
	home, seed := t.TempDir(), t.TempDir()
	writeSeedPlugin(t, seed, "mcp-servers", `{"v":1}`)
	if err := syncMiniMaxPlugins(home, seed); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(miniMaxPluginsDir(home), "mcp-servers", "mcp", "old.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(seed); err != nil {
		t.Fatal(err)
	}
	writeSeedPlugin(t, seed, "mcp-servers", `{"v":2}`)

	if err := syncMiniMaxPlugins(home, seed); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := readFile(t, filepath.Join(miniMaxPluginsDir(home), "mcp-servers", "mcp", "k3s.json")); got != `{"v":2}` {
		t.Errorf("server file = %q, want the new seed", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file survived the replace: %v", err)
	}
}

// AC-G5: a pod that staged nothing (no token) removes the platform's plugins
// from the archived HOME but leaves one the user installed by hand.
func TestSyncMiniMaxPluginsWithoutSeedRemovesOnlyManagedPlugins(t *testing.T) {
	home, seed := t.TempDir(), t.TempDir()
	writeSeedPlugin(t, seed, "mcp-servers", `{}`)
	if err := syncMiniMaxPlugins(home, seed); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(miniMaxPluginsDir(home), "users-own")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := syncMiniMaxPlugins(home, ""); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(miniMaxPluginsDir(home), "mcp-servers")); !os.IsNotExist(err) {
		t.Errorf("managed plugin survived an empty seed: %v", err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("the user's own plugin was removed: %v", err)
	}
}

func TestSyncMiniMaxPluginsRefusesALinkInTheSeed(t *testing.T) {
	home, seed := t.TempDir(), t.TempDir()
	writeSeedPlugin(t, seed, "mcp-servers", `{}`)
	if err := os.Symlink("/etc/passwd", filepath.Join(seed, "mcp-servers", "mcp", "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := syncMiniMaxPlugins(home, seed); err == nil {
		t.Fatal("sync copied a seed that contains a symlink")
	}
}

// pluginServer reads the one MCP server an installed platform plugin declares.
func pluginServer(t *testing.T, home, plugin string) (string, map[string]any) {
	t.Helper()
	var manifest struct {
		MCPServers []string `json:"mcpServers"`
	}
	root := filepath.Join(miniMaxPluginsDir(home), plugin)
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"))), &manifest); err != nil {
		t.Fatalf("plugin %s manifest: %v", plugin, err)
	}
	if len(manifest.MCPServers) != 1 {
		t.Fatalf("plugin %s mcpServers = %v, want one file", plugin, manifest.MCPServers)
	}
	var servers struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(root, manifest.MCPServers[0]))), &servers); err != nil {
		t.Fatalf("plugin %s servers: %v", plugin, err)
	}
	if len(servers.MCPServers) != 1 {
		t.Fatalf("plugin %s servers = %v, want one", plugin, servers.MCPServers)
	}
	for name, server := range servers.MCPServers {
		return name, server
	}
	return "", nil
}

func newTestMiniMaxHome(t *testing.T, configured bool, probeErr error) (miniMaxCLI, string, *int) {
	t.Helper()
	dir := t.TempDir()
	cli, err := newMiniMaxCLI(filepath.Join(dir, "state"), "", "http://10.1.2.3:8091", claudeProxyPlaceholderToken)
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	cli.k3sProxyURL = "http://10.1.2.3:8093"
	cli.k3sConfigured = func(baseURL string) (bool, error) {
		probes++
		if baseURL != cli.k3sProxyURL {
			t.Errorf("probed %q, want the K3s proxy %q", baseURL, cli.k3sProxyURL)
		}
		return configured, probeErr
	}
	return cli, filepath.Join(dir, "state", "home"), &probes
}

var testMiniMaxTools = toolSurface{SessionMCP: "http://10.1.2.3:8092"}

// AC-G5/AC-G6: both servers point at the helper pod, carry no auth of their
// own, and use the names claude-code's tools already have.
func TestMiniMaxEnsureHomeRegistersHelperServers(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, true, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	for _, tc := range []struct{ plugin, server, url string }{
		{miniMaxSessionPluginName, sessionMCPServerName, "http://10.1.2.3:8092/mcp"},
		{miniMaxK3SPluginName, "k3s", "http://10.1.2.3:8093/mcp"},
	} {
		name, server := pluginServer(t, home, tc.plugin)
		if name != tc.server || server["type"] != "http" || server["url"] != tc.url || len(server) != 2 {
			t.Errorf("plugin %s server %s = %v, want only type http and url %s", tc.plugin, name, server, tc.url)
		}
		if _, err := os.Stat(filepath.Join(miniMaxPluginsDir(home), tc.plugin, miniMaxManagedPluginMarker)); err != nil {
			t.Errorf("plugin %s is not marked as the platform's: %v", tc.plugin, err)
		}
	}
	if _, err := os.Stat(cli.configPath); err != nil {
		t.Errorf("ensureHome did not write the managed config: %v", err)
	}
	if rel, err := filepath.Rel(filepath.Dir(home), filepath.Dir(cli.configPath)); err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("staging dir %s is inside the state tree", filepath.Dir(cli.configPath))
	}
}

// AC-G5: a proxy without a token means no K3s server, and a restored HOME that
// still lists one from an earlier pod loses it.
func TestMiniMaxEnsureHomeOmitsAnUnconfiguredK3SProxy(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, true, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	cli.k3sConfigured = func(string) (bool, error) { return false, nil }
	if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	if _, err := os.Stat(filepath.Join(miniMaxPluginsDir(home), miniMaxK3SPluginName)); !os.IsNotExist(err) {
		t.Errorf("K3s plugin survived an unconfigured proxy: %v", err)
	}
	if name, _ := pluginServer(t, home, miniMaxSessionPluginName); name != sessionMCPServerName {
		t.Errorf("session MCP server = %q, want %q", name, sessionMCPServerName)
	}
}

// An unreachable proxy is a broken helper pod, not a missing key: the boot
// fails rather than silently running without the tools.
func TestMiniMaxEnsureHomeFailsOnAnUnreachableK3SProxy(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, false, errors.New("connection refused"))
	if err := cli.ensureHome(home, testMiniMaxTools); err == nil {
		t.Fatal("ensureHome succeeded with an unreachable K3s proxy")
	}
}

func TestMiniMaxEnsureHomeWithoutSessionMCPInstallsNothing(t *testing.T) {
	cli, home, probes := newTestMiniMaxHome(t, true, nil)
	writeSeedPlugin(t, miniMaxPluginsDir(home), "stale", `{}`)
	if err := os.WriteFile(filepath.Join(miniMaxPluginsDir(home), "stale", miniMaxManagedPluginMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cli.ensureHome(home, toolSurface{}); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	if entries, _ := os.ReadDir(miniMaxPluginsDir(home)); len(entries) != 0 {
		t.Errorf("plugins = %v, want none", entries)
	}
	if *probes != 0 {
		t.Errorf("probed the K3s proxy %d times without a session MCP", *probes)
	}
}

func TestK3SProxyConfiguredReadsHealth(t *testing.T) {
	for _, configured := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != k3sMCPProxyHealthz {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "configured": configured})
		}))
		got, err := k3sProxyConfigured(srv.URL)
		srv.Close()
		if err != nil || got != configured {
			t.Errorf("configured = %v, %v; want %v", got, err, configured)
		}
	}
}
