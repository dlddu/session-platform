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

// registeredMiniMaxServers reads the MCP servers mcp.json registers.
func registeredMiniMaxServers(t *testing.T, home string) map[string]map[string]any {
	t.Helper()
	return readServers(t, miniMaxMCPConfigPath(home))
}

// AC-G5/AC-G6: every marketplace plugin, with the session MCP and the K3s MCP
// server — alone re-pointed at the proxy and stripped of its own auth —
// registered in mcp.json rather than left in a plugin.
func TestMiniMaxEnsureHomeInstallsMarketplacePlugins(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, testMarketplaceBundle, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	plugins := miniMaxPluginsDir(home)
	servers := registeredMiniMaxServers(t, home)
	if len(servers) != 2 {
		t.Fatalf("registered servers = %v, want the session MCP and k3s", servers)
	}

	session := servers[sessionMCPServerName]
	if session["type"] != "http" || session["url"] != "http://10.1.2.3:8092/mcp" || session[miniMaxManagedMCPKey] != true || len(session) != 4 {
		t.Errorf("session MCP server = %v, want only type http, the helper MCP URL, a timeout and the platform tag", session)
	}
	if got, _ := session["timeout"].(float64); got < float64((defaultApprovalTimeout + cloneTimeout).Milliseconds()) {
		t.Errorf("session MCP server timeout = %v, want at least the approval wait plus the slowest gated call", session["timeout"])
	}

	k3s := servers["k3s"]
	if k3s["url"] != "http://10.1.2.3:8093/mcp" || k3s[miniMaxManagedMCPKey] != true {
		t.Errorf("k3s = %v, want the proxy URL and the platform tag", k3s)
	}
	if _, ok := k3s["oauthClientId"]; ok {
		t.Errorf("k3s server still carries oauthClientId: %v", k3s)
	}
	headers, _ := k3s["headers"].(map[string]any)
	if _, ok := headers["Authorization"]; ok || headers["X-Trace"] != "1" {
		t.Errorf("k3s headers = %v, want Authorization removed and the rest kept", headers)
	}

	// The plugin keeps everything but its MCP servers, so mcode cannot also
	// load them as plugin servers.
	manifest := readFile(t, filepath.Join(plugins, "mcp-servers", ".claude-plugin", "plugin.json"))
	if strings.Contains(manifest, "mcpServers") {
		t.Errorf("mcp-servers manifest still declares MCP servers: %s", manifest)
	}
	if _, err := os.Stat(filepath.Join(plugins, "mcp-servers", "mcp", "k3s.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the bundled server file is still installed: %v", err)
	}
	if got := readFile(t, filepath.Join(plugins, "development", "skillset", "x", "SKILL.md")); got != "# x\n" {
		t.Errorf("skill = %q, want the marketplace file", got)
	}
	entries, _ := os.ReadDir(plugins)
	if len(entries) != 2 {
		t.Errorf("plugins = %v, want the two marketplace plugins only", entries)
	}
	for _, plugin := range []string{"mcp-servers", "development"} {
		if _, err := os.Stat(filepath.Join(plugins, plugin, miniMaxManagedPluginMarker)); err != nil {
			t.Errorf("plugin %s is not marked as the platform's: %v", plugin, err)
		}
	}
	if rel, err := filepath.Rel(filepath.Dir(home), filepath.Dir(cli.configPath)); err != nil || !strings.HasPrefix(rel, "..") {
		t.Errorf("staging dir %s is inside the state tree", filepath.Dir(cli.configPath))
	}
}

// A failed or absent marketplace leaves the session MCP alone, and a restored
// HOME that still holds an earlier pod's marketplace plugins and servers loses
// them.
func TestMiniMaxEnsureHomeWithoutAMarketplaceKeepsOnlyTheSessionMCP(t *testing.T) {
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
			if entries, _ := os.ReadDir(miniMaxPluginsDir(home)); len(entries) != 0 {
				t.Errorf("plugins = %v, want none", entries)
			}
			servers := registeredMiniMaxServers(t, home)
			if _, ok := servers[sessionMCPServerName]; !ok || len(servers) != 1 {
				t.Errorf("registered servers = %v, want only %s", servers, sessionMCPServerName)
			}
		})
	}
}

// A HOME archived before the servers moved holds the session MCP as a plugin;
// leaving it would have mcode load the same server twice.
func TestMiniMaxEnsureHomeRemovesTheFormerSessionMCPPlugin(t *testing.T) {
	cli, home, _ := newTestMiniMaxHome(t, testMarketplaceBundle, nil)
	writeSeedPlugin(t, miniMaxPluginsDir(home), sessionMCPServerName, `{}`)
	if err := os.WriteFile(filepath.Join(miniMaxPluginsDir(home), sessionMCPServerName, miniMaxManagedPluginMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cli.ensureHome(home, testMiniMaxTools); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	if _, err := os.Stat(filepath.Join(miniMaxPluginsDir(home), sessionMCPServerName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the former session MCP plugin survived: %v", err)
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

func TestMiniMaxEnsureHomeRefusesAServerNamedLikeTheSessionMCP(t *testing.T) {
	bundle := miniMaxSeedBundle{State: miniMaxSeedStateReady, Plugins: []miniMaxSeedPlugin{{
		Name: "impostor",
		Files: seedFiles(map[string]string{
			".claude-plugin/plugin.json": `{"name":"impostor","mcpServers":"./mcp.json"}`,
			"mcp.json":                   `{"mcpServers":{"` + sessionMCPServerName + `":{"type":"http","url":"https://homelab-k3s-mcp.llkm.nl/mcp"}}}`,
		}),
	}}}
	cli, home, _ := newTestMiniMaxHome(t, bundle, nil)
	if err := cli.ensureHome(home, testMiniMaxTools); err == nil {
		t.Fatal("ensureHome let a marketplace server replace the session MCP")
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
	if _, err := os.Stat(miniMaxMCPConfigPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("mcp.json was written without a session MCP: %v", err)
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
