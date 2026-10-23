package main

import (
	"encoding/base64"
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

func seedFiles(files map[string]string) map[string]string {
	encoded := make(map[string]string, len(files))
	for path, content := range files {
		encoded[path] = base64.StdEncoding.EncodeToString([]byte(content))
	}
	return encoded
}

// The shape the real marketplace ships: an OAuth client id mcode rejects, a
// bearer header with a ${VAR} mcode would send literally, and — to prove the
// re-pointing is per server — one server the pod cannot reach.
var testMarketplaceBundle = miniMaxSeedBundle{State: miniMaxSeedStateReady, Plugins: []miniMaxSeedPlugin{
	{Name: "mcp-servers", Files: seedFiles(map[string]string{
		".claude-plugin/plugin.json": `{"name":"mcp-servers","mcpServers":["./mcp/k3s.json"]}`,
		"mcp/k3s.json": `{"mcpServers":{
			"k3s":{"type":"http","url":"https://homelab-k3s-mcp.llkm.nl/mcp","oauthClientId":"homelab-k3s-mcp",
				"headers":{"Authorization":"Bearer ${K3S_MCP_TOKEN}","X-Trace":"1"}},
			"elsewhere":{"type":"http","url":"https://example.com/mcp"}}}`,
	})},
	{Name: "development", Files: seedFiles(map[string]string{
		".claude-plugin/plugin.json": `{"name":"development","skills":["./skillset/x"]}`,
		"skillset/x/SKILL.md":        "# x\n",
	})},
}}

func newTestMiniMaxHome(t *testing.T, bundle miniMaxSeedBundle, fetchErr error) (miniMaxCLI, string, *int) {
	t.Helper()
	dir := t.TempDir()
	cli, err := newMiniMaxCLI(filepath.Join(dir, "state"), "", "http://10.1.2.3:8091", claudeProxyPlaceholderToken)
	if err != nil {
		t.Fatal(err)
	}
	fetches := 0
	cli.k3sProxyURL = "http://10.1.2.3:8093"
	cli.fetchSeed = func(baseURL string) (miniMaxSeedBundle, error) {
		fetches++
		if baseURL != cli.k3sProxyURL {
			t.Errorf("fetched the seed from %q, want the K3s proxy %q", baseURL, cli.k3sProxyURL)
		}
		return bundle, fetchErr
	}
	return cli, filepath.Join(dir, "state", "home"), &fetches
}

var testMiniMaxTools = toolSurface{SessionMCP: "http://10.1.2.3:8092"}

func readServers(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.MCPServers
}

// AC-G5/AC-G6: the session MCP plugin plus every marketplace plugin, with the
// K3s MCP server alone re-pointed at the proxy and stripped of its own auth.
func TestMiniMaxEnsureHomeInstallsMarketplacePlugins(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, testMarketplaceBundle, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	plugins := miniMaxPluginsDir(home)

	name, server := pluginServer(t, home, miniMaxSessionPluginName)
	if name != sessionMCPServerName || server["url"] != "http://10.1.2.3:8092/mcp" || len(server) != 2 {
		t.Errorf("session MCP server %s = %v, want only type http and the helper MCP URL", name, server)
	}

	servers := readServers(t, filepath.Join(plugins, "mcp-servers", "mcp", "k3s.json"))
	k3s := servers["k3s"]
	if k3s["url"] != "http://10.1.2.3:8093/mcp" {
		t.Errorf("k3s url = %v, want the proxy", k3s["url"])
	}
	if _, ok := k3s["oauthClientId"]; ok {
		t.Errorf("k3s server still carries oauthClientId: %v", k3s)
	}
	headers, _ := k3s["headers"].(map[string]any)
	if _, ok := headers["Authorization"]; ok || headers["X-Trace"] != "1" {
		t.Errorf("k3s headers = %v, want Authorization removed and the rest kept", headers)
	}
	if _, ok := servers["elsewhere"]; ok {
		t.Error("a server the pod cannot reach was kept")
	}
	if got := readFile(t, filepath.Join(plugins, "development", "skillset", "x", "SKILL.md")); got != "# x\n" {
		t.Errorf("skill = %q, want the marketplace file", got)
	}
	for _, plugin := range []string{miniMaxSessionPluginName, "mcp-servers", "development"} {
		if _, err := os.Stat(filepath.Join(plugins, plugin, miniMaxManagedPluginMarker)); err != nil {
			t.Errorf("plugin %s is not marked as the platform's: %v", plugin, err)
		}
	}
	if rel, err := filepath.Rel(filepath.Dir(home), filepath.Dir(cli.configPath)); err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("staging dir %s is inside the state tree", filepath.Dir(cli.configPath))
	}
}

// A failed or absent marketplace leaves the session MCP plugin alone, and a
// restored HOME that still holds an earlier pod's marketplace plugins loses
// them.
func TestMiniMaxEnsureHomeWithoutAMarketplaceKeepsOnlyTheSessionPlugin(t *testing.T) {
	for _, state := range []string{miniMaxSeedStateFailed, miniMaxSeedStateUnconfigured} {
		t.Run(state, func(t *testing.T) {
			cli, home, _ := newTestMiniMaxHome(t, testMarketplaceBundle, nil)
			if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
				t.Fatalf("ensureHome: %v", err)
			}
			cli.fetchSeed = func(string) (miniMaxSeedBundle, error) {
				return miniMaxSeedBundle{State: state, Plugins: []miniMaxSeedPlugin{}}, nil
			}
			if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
				t.Fatalf("ensureHome: %v", err)
			}
			entries, _ := os.ReadDir(miniMaxPluginsDir(home))
			if len(entries) != 1 || entries[0].Name() != miniMaxSessionPluginName {
				t.Errorf("plugins = %v, want only %s", entries, miniMaxSessionPluginName)
			}
		})
	}
}

// An unreachable proxy is a broken helper pod, not a missing key: the boot
// fails rather than silently running without the tools.
func TestMiniMaxEnsureHomeFailsOnAnUnreachableK3SProxy(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, miniMaxSeedBundle{}, errors.New("connection refused"))
	if err := cli.ensureHome(home, testMiniMaxTools); err == nil {
		t.Fatal("ensureHome succeeded with an unreachable K3s proxy")
	}
}

func TestMiniMaxEnsureHomeRefusesAPluginNamedLikeTheSessionPlugin(t *testing.T) {
	bundle := miniMaxSeedBundle{State: miniMaxSeedStateReady, Plugins: []miniMaxSeedPlugin{{
		Name:  miniMaxSessionPluginName,
		Files: seedFiles(map[string]string{".claude-plugin/plugin.json": `{}`}),
	}}}
	cli, home, _ := newTestMiniMaxHome(t, bundle, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err == nil {
		t.Fatal("ensureHome let a marketplace plugin replace the session MCP plugin")
	}
}

func TestMiniMaxEnsureHomeRefusesAPathOutsideThePlugin(t *testing.T) {
	bundle := miniMaxSeedBundle{State: miniMaxSeedStateReady, Plugins: []miniMaxSeedPlugin{{
		Name:  "evil",
		Files: seedFiles(map[string]string{"../escape.json": `{}`}),
	}}}
	cli, home, _ := newTestMiniMaxHome(t, bundle, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err == nil {
		t.Fatal("ensureHome wrote a seed file outside its plugin")
	}
}

func TestMiniMaxEnsureHomeWithoutSessionMCPInstallsNothing(t *testing.T) {
	cli, home, fetches := newTestMiniMaxHome(t, testMarketplaceBundle, nil)
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
	if *fetches != 0 {
		t.Errorf("fetched the seed %d times without a session MCP", *fetches)
	}
}

// The agent waits out the proxy's bootstrap (503) and takes the bundle.
func TestFetchMiniMaxSeedWaitsForTheBootstrap(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != miniMaxPluginSeedPath {
			http.NotFound(w, r)
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(miniMaxSeedBundle{State: miniMaxSeedStateBootstrapping})
			return
		}
		_ = json.NewEncoder(w).Encode(testMarketplaceBundle)
	}))
	defer srv.Close()
	got, err := fetchMiniMaxSeed(srv.URL)
	if err != nil || got.State != miniMaxSeedStateReady || len(got.Plugins) != 2 || calls != 2 {
		t.Fatalf("seed = %+v, %v after %d calls; want the ready bundle on the second", got.State, err, calls)
	}
}
