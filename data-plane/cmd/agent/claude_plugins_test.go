package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testClaudePlugin is a managed plugin the tests enable; the platform no longer
// enables any by default (AC-L1).
const testClaudePlugin = "session-platform@dlddu-plugins"

// AC-L1: unset means no plugins; a selection is qualified with the platform
// marketplace, kept in order, and refused when malformed.
func TestClaudePluginsFromEnv(t *testing.T) {
	t.Run("unset is no plugins", func(t *testing.T) {
		// t.Setenv restores the variable once the test ends.
		t.Setenv(claudePluginsEnv, "")
		os.Unsetenv(claudePluginsEnv)
		got, err := claudePluginsFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("plugins = %q, want none", got)
		}
	})
	t.Run("selection", func(t *testing.T) {
		t.Setenv(claudePluginsEnv, "development,dear-baby")
		got, err := claudePluginsFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"development@dlddu-plugins", "dear-baby@dlddu-plugins"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("plugins = %q, want %q", got, want)
		}
	})
	for _, raw := range []string{"", "a,,b", "a,a", "dev@other", "../x", " dev"} {
		t.Run("rejects "+raw, func(t *testing.T) {
			t.Setenv(claudePluginsEnv, raw)
			if got, err := claudePluginsFromEnv(); err == nil {
				t.Fatalf("plugins = %q, want an error", got)
			}
		})
	}
}

// AC-L1: a pod normalises the archived HOME to its own selection — a restore
// never brings back a plugin the session did not choose, and a several-plugin
// surface is as valid as the single platform plugin.
func TestClaudeManagedSettingsFollowThePodsSelection(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugins: []string{testClaudePlugin}}); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	selection := []string{"development@dlddu-plugins", "learning@dlddu-plugins"}
	if err := validateClaudeManagedSettings(homeDir); err != nil {
		t.Fatalf("archive validation before restore: %v", err)
	}
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugins: selection}); err != nil {
		t.Fatalf("restore boot: %v", err)
	}
	if err := validateClaudeManagedSettings(homeDir); err != nil {
		t.Fatalf("restored settings are invalid: %v", err)
	}
	settings, err := loadClaudeManagedSettings(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"development@dlddu-plugins": true, "learning@dlddu-plugins": true}
	if !reflect.DeepEqual(settings.EnabledPlugins, want) {
		t.Fatalf("enabled plugins = %v, want exactly %v", settings.EnabledPlugins, want)
	}
}

// A session that chose no plugins boots, archives and restores with none, and
// a restore drops the plugins an older archive enabled (AC-L1).
func TestClaudeManagedSettingsWithoutPlugins(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugins: []string{testClaudePlugin}}); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{}); err != nil {
		t.Fatalf("restore boot without plugins: %v", err)
	}
	if err := validateClaudeManagedSettings(homeDir); err != nil {
		t.Fatalf("settings without plugins are invalid: %v", err)
	}
	settings, err := loadClaudeManagedSettings(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.EnabledPlugins) != 0 {
		t.Fatalf("enabled plugins = %v, want none", settings.EnabledPlugins)
	}
	if err := ensureClaudeManagedSettings(t.TempDir(), toolSurface{}); err != nil {
		t.Fatalf("first boot without plugins: %v", err)
	}
}

// Plugins from another marketplace are still not the platform's to enable,
// whatever the session selected.
func TestClaudeManagedSettingsStillRejectOtherMarketplaces(t *testing.T) {
	for _, name := range []string{"development@elsewhere", "@dlddu-plugins", "a/b@dlddu-plugins"} {
		homeDir := t.TempDir()
		settingsDir := filepath.Join(homeDir, claudeSettingsDir)
		if err := os.MkdirAll(settingsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		settings := claudeManagedSettings{EnabledPlugins: map[string]bool{name: true}}
		settings.Permissions.Allow = append([]string(nil), claudeManagedTools...)
		if err := storeClaudeManagedSettings(settingsDir, filepath.Join(settingsDir, claudeSettingsFile), settings); err != nil {
			t.Fatal(err)
		}
		if err := validateClaudeManagedSettings(homeDir); err == nil {
			t.Errorf("%s: archive with that plugin validated, want rejection", name)
		}
	}
}

// AC-L1 on minimax-code: a selection of several plugins that each declare the
// K3s server registers it once instead of failing the boot; a plugin that
// declares it differently loses to the first, with a warning, rather than
// costing the session its agent.
func TestMiniMaxStagingSharesAServerAcrossSelectedPlugins(t *testing.T) {
	k3s := func(name, header string) miniMaxSeedPlugin {
		return miniMaxSeedPlugin{Name: name, Files: seedFiles(map[string]string{
			".claude-plugin/plugin.json": `{"name":"` + name + `","mcpServers":["./mcp/k3s.json"]}`,
			"mcp/k3s.json":               `{"mcpServers":{"k3s":{"type":"http","url":"https://homelab-k3s-mcp.llkm.nl/mcp","headers":{"X-Trace":"` + header + `"}}}}`,
		})}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name   string
		bundle []miniMaxSeedPlugin
	}{
		{"identical", []miniMaxSeedPlugin{k3s("mcp-servers", "1"), k3s("session-platform", "1")}},
		{"different", []miniMaxSeedPlugin{k3s("mcp-servers", "1"), k3s("session-platform", "2")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers, err := stageMiniMaxPlugins(t.TempDir(), "http://10.1.2.3:8092/mcp", "http://10.1.2.3:8093", tc.bundle, logger)
			if err != nil {
				t.Fatalf("stageMiniMaxPlugins: %v", err)
			}
			got, ok := servers["k3s"]
			if !ok {
				t.Fatalf("servers = %v, want k3s registered", servers)
			}
			headers, _ := got["headers"].(map[string]any)
			if headers["X-Trace"] != "1" {
				t.Errorf("k3s headers = %v, want the first plugin's", headers)
			}
		})
	}
}

const testK3SUpstream = "https://homelab-k3s-mcp.llkm.nl/mcp"

var testClaudeSeed = []miniMaxSeedPlugin{
	seedPlugin("development", map[string]string{
		".claude-plugin/plugin.json":        `{"name":"development","skills":["./skillset/git-repo-cloner"]}`,
		"skillset/git-repo-cloner/SKILL.md": "---\nname: git-repo-cloner\n---\n",
	}),
	seedPlugin("mcp-servers", map[string]string{
		".claude-plugin/plugin.json": `{"name":"mcp-servers","mcpServers":["./mcp/k3s.json"]}`,
		"mcp/k3s.json": `{"mcpServers":{"k3s":{"type":"http","url":"` + testK3SUpstream + `",` +
			`"oauthClientId":"homelab-k3s-mcp","headers":{"Authorization":"Bearer ${K3S_MCP_TOKEN}"}}}}`,
	}),
	seedPlugin("mixed", map[string]string{
		".claude-plugin/plugin.json": `{"name":"mixed","mcpServers":"./mcp/servers.json"}`,
		"mcp/servers.json": `{"mcpServers":{"k3s":{"type":"http","url":"` + testK3SUpstream + `/"},` +
			`"docs":{"type":"http","url":"https://docs.example/mcp"}}}`,
	}),
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// The staged marketplace carries the platform's marketplace name, so plugin
// ids stay <name>@dlddu-plugins, and no plugin keeps the K3s server: the
// platform registers it, and the plugin's copy wants a token this container
// does not have.
func TestStageClaudeMarketplaceDropsOnlyTheK3SServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "marketplace")
	if err := os.MkdirAll(filepath.Join(dir, "stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := stageClaudeMarketplace(dir, testClaudeSeed, testK3SUpstream); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a previous stage survived: %v", err)
	}
	market := readJSONFile(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	if market["name"] != claudePluginMarketplace {
		t.Fatalf("marketplace name = %v, want %s", market["name"], claudePluginMarketplace)
	}
	wantEntries := []any{
		map[string]any{"name": "development", "source": "./development"},
		map[string]any{"name": "mcp-servers", "source": "./mcp-servers"},
		map[string]any{"name": "mixed", "source": "./mixed"},
	}
	if !reflect.DeepEqual(market["plugins"], wantEntries) {
		t.Fatalf("marketplace plugins = %v, want %v", market["plugins"], wantEntries)
	}
	if _, err := os.Stat(filepath.Join(dir, "development", "skillset", "git-repo-cloner", "SKILL.md")); err != nil {
		t.Fatalf("development skill not staged: %v", err)
	}

	onlyK3s := readJSONFile(t, filepath.Join(dir, "mcp-servers", ".claude-plugin", "plugin.json"))
	if _, ok := onlyK3s["mcpServers"]; ok {
		t.Fatalf("mcp-servers manifest still declares MCP files: %v", onlyK3s)
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp-servers", "mcp", "k3s.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the K3s-only MCP file survived: %v", err)
	}

	mixed := readJSONFile(t, filepath.Join(dir, "mixed", ".claude-plugin", "plugin.json"))
	if !reflect.DeepEqual(mixed["mcpServers"], []any{"./mcp/servers.json"}) {
		t.Fatalf("mixed manifest mcpServers = %v", mixed["mcpServers"])
	}
	servers := readJSONFile(t, filepath.Join(dir, "mixed", "mcp", "servers.json"))["mcpServers"].(map[string]any)
	if _, ok := servers["k3s"]; ok {
		t.Fatal("the K3s server survived in a file with another server")
	}
	if docs, ok := servers["docs"].(map[string]any); !ok || docs["url"] != "https://docs.example/mcp" {
		t.Fatalf("another plugin server was changed: %v", servers)
	}
}

type fakeClaudeInstall struct {
	calls   [][]string
	err     error
	setenvs map[string]string
}

func newTestClaudePlatform(t *testing.T, sidecar codexSidecar, probeErr error, install *fakeClaudeInstall) *claudePlatform {
	t.Helper()
	t.Setenv(k3sMCPURLEnv, testK3SUpstream)
	install.setenvs = map[string]string{}
	return &claudePlatform{
		proxyURL: "http://127.0.0.1:8093",
		workRoot: t.TempDir(),
		probe:    func(string) (codexSidecar, error) { return sidecar, probeErr },
		install: func(marketplaceDir, home, cacheDir string, plugins []string) error {
			install.calls = append(install.calls, append([]string{marketplaceDir, home, cacheDir}, plugins...))
			return install.err
		},
		setenv: func(key, value string) error {
			install.setenvs[key] = value
			return nil
		},
	}
}

func TestClaudePlatformInstallsTheSelectedSeededPlugins(t *testing.T) {
	install := &fakeClaudeInstall{}
	p := newTestClaudePlatform(t, codexSidecar{configured: true, plugins: testClaudeSeed, revision: "abc"}, nil, install)
	selected := []string{"development@dlddu-plugins", "learning@dlddu-plugins"}
	p.prepare(selected)
	// A restore in the same pod installs nothing twice.
	p.prepare(selected)

	if got := p.mcpURL(); got != "http://127.0.0.1:8093/mcp" {
		t.Fatalf("K3s MCP URL = %q, want the sidecar with the upstream's path", got)
	}
	if len(install.calls) != 1 {
		t.Fatalf("install ran %d times, want once per pod", len(install.calls))
	}
	call := install.calls[0]
	if !reflect.DeepEqual(call[3:], []string{"development@dlddu-plugins"}) {
		t.Fatalf("installed %v, want only the selected plugin the seed carried", call[3:])
	}
	if call[0] != filepath.Join(p.workRoot, "marketplace") {
		t.Fatalf("marketplace dir = %q", call[0])
	}
	if got := install.setenvs[claudePluginCacheDirEnv]; got != call[2] {
		t.Fatalf("%s = %q, want the cache the install filled (%q)", claudePluginCacheDirEnv, got, call[2])
	}
	if got := p.enabled(selected); !reflect.DeepEqual(got, []string{"development@dlddu-plugins"}) {
		t.Fatalf("enabled = %v, want what was installed", got)
	}
}

// Like codex: a sidecar without a token is a session without the K3s MCP or
// plugins, not a failure.
func TestClaudePlatformWithoutATokenHasNeitherK3SNorPlugins(t *testing.T) {
	install := &fakeClaudeInstall{}
	p := newTestClaudePlatform(t, codexSidecar{}, nil, install)
	p.prepare([]string{"development@dlddu-plugins"})
	if got := p.mcpURL(); got != "" {
		t.Fatalf("K3s MCP URL = %q, want none from an unconfigured sidecar", got)
	}
	if len(install.calls) != 0 {
		t.Fatalf("install ran with nothing seeded: %v", install.calls)
	}
	if got := p.enabled([]string{"development@dlddu-plugins"}); len(got) != 0 {
		t.Fatalf("enabled = %v, want none", got)
	}
}

func TestClaudePlatformAFailedInstallEnablesNothing(t *testing.T) {
	install := &fakeClaudeInstall{err: errors.New("claude plugin install: exit 1")}
	p := newTestClaudePlatform(t, codexSidecar{configured: true, plugins: testClaudeSeed}, nil, install)
	p.prepare([]string{"development@dlddu-plugins"})
	if got := p.mcpURL(); got == "" {
		t.Fatal("a failed plugin install also dropped the K3s MCP")
	}
	if got := p.enabled([]string{"development@dlddu-plugins"}); len(got) != 0 {
		t.Fatalf("enabled = %v after a failed install, want none", got)
	}
	if _, ok := install.setenvs[claudePluginCacheDirEnv]; ok {
		t.Fatal("the plugin cache was exported after a failed install")
	}
}

// The sidecar's seed may still fail after its health answered; the K3s MCP
// it reported is kept.
func TestClaudePlatformKeepsTheK3SMCPWhenTheSeedFails(t *testing.T) {
	install := &fakeClaudeInstall{}
	p := newTestClaudePlatform(t, codexSidecar{configured: true}, errors.New("plugin marketplace bootstrap failed"), install)
	p.prepare([]string{"development@dlddu-plugins"})
	if got := p.mcpURL(); got != "http://127.0.0.1:8093/mcp" {
		t.Fatalf("K3s MCP URL = %q", got)
	}
	if len(install.calls) != 0 {
		t.Fatalf("install ran without a seed: %v", install.calls)
	}
}

func TestClaudeCLIEnsureHomeEnablesOnlyInstalledPluginsAndPassesTheK3SURL(t *testing.T) {
	install := &fakeClaudeInstall{}
	p := newTestClaudePlatform(t, codexSidecar{configured: true, plugins: testClaudeSeed}, nil, install)
	cli := claudeCLI{platform: p}
	home := t.TempDir()
	if err := cli.ensureHome(home, toolSurface{Plugins: []string{"development@dlddu-plugins", "learning@dlddu-plugins"}}); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	settings, err := loadClaudeManagedSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settings.EnabledPlugins, map[string]bool{"development@dlddu-plugins": true}) {
		t.Fatalf("enabled plugins = %v", settings.EnabledPlugins)
	}
	argv := cli.argv(defaultClaudeRunnerBinary, platformDefaultModel, "--prompt", false)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--k3s-mcp-url http://127.0.0.1:8093/mcp -- --prompt") {
		t.Fatalf("argv = %v, want the K3s MCP URL before the prompt delimiter", argv)
	}
	if got := (claudeCLI{}).argv(defaultClaudeRunnerBinary, platformDefaultModel, "p", false); slicesContain(got, "--k3s-mcp-url") {
		t.Fatalf("argv without a sidecar = %v, want no K3s MCP", got)
	}
}

func TestClaudePlatformFromEnvKeepsTheTokenOutAndTheProxyOnLoopback(t *testing.T) {
	t.Setenv(k3sMCPTokenEnv, "")
	t.Setenv(k3sMCPProxyURLEnv, "http://127.0.0.1:8093")
	if p, err := claudePlatformFromEnv(nil); err != nil || p.proxyURL != "http://127.0.0.1:8093" {
		t.Fatalf("loopback sidecar: %v, %+v", err, p)
	}
	for name, env := range map[string][2]string{
		"token in the CLI container": {"tok", "http://127.0.0.1:8093"},
		"no sidecar":                 {"", ""},
		"pod-network proxy":          {"", "http://10.0.0.5:8093"},
		"wrong port":                 {"", "http://127.0.0.1:8091"},
	} {
		t.Setenv(k3sMCPTokenEnv, env[0])
		t.Setenv(k3sMCPProxyURLEnv, env[1])
		if _, err := claudePlatformFromEnv(nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
