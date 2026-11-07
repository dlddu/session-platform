package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// AC-L1: unset keeps the platform plugin; a selection is qualified with the
// platform marketplace, kept in order, and refused when malformed.
func TestClaudePluginsFromEnv(t *testing.T) {
	t.Run("unset is the platform plugin", func(t *testing.T) {
		// t.Setenv restores the variable once the test ends.
		t.Setenv(claudePluginsEnv, "")
		os.Unsetenv(claudePluginsEnv)
		got, err := claudePluginsFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{claudeSessionPlatformPlugin}; !reflect.DeepEqual(got, want) {
			t.Fatalf("plugins = %q, want %q", got, want)
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
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugins: []string{claudeSessionPlatformPlugin}}); err != nil {
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
