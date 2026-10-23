package main

import (
	"os"
	"path/filepath"
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

func TestMiniMaxEnsureHomeInstallsTheSeed(t *testing.T) {
	stateDir, seed := t.TempDir(), t.TempDir()
	writeSeedPlugin(t, seed, "mcp-servers", `{}`)
	cli, err := newMiniMaxCLI(filepath.Join(stateDir, "state"), "", claudeProxyBaseURL, claudeProxyPlaceholderToken)
	if err != nil {
		t.Fatal(err)
	}
	cli.pluginSeedDir = seed
	home := filepath.Join(stateDir, "state", "home")

	if err := cli.ensureHome(home, toolSurface{}); err != nil {
		t.Fatalf("ensureHome: %v", err)
	}
	if _, err := os.Stat(filepath.Join(miniMaxPluginsDir(home), "mcp-servers", ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("ensureHome did not install the staged plugin: %v", err)
	}
	if _, err := os.Stat(cli.configPath); err != nil {
		t.Errorf("ensureHome did not write the managed config: %v", err)
	}
}
