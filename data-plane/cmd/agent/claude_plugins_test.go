package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
