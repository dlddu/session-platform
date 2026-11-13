package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The two sanctioned proxy placements, and the guarantee that opening one does
// not open the other.
func TestCredentialProxyBindRulesAreScopedToItsPlacement(t *testing.T) {
	for _, tc := range []struct {
		placement credentialProxyPlacement
		addr      string
		wantOK    bool
	}{
		{proxyPlacementSidecar, "127.0.0.1:8091", true},
		{proxyPlacementSidecar, "[::1]:8091", true},
		{proxyPlacementSidecar, "0.0.0.0:8091", false},
		{proxyPlacementSidecar, ":8091", false},
		{proxyPlacementSidecar, "10.42.0.7:8091", false},
		{proxyPlacementHelper, "0.0.0.0:8091", true},
		{proxyPlacementHelper, ":8091", true},
		{proxyPlacementHelper, "10.42.0.7:8091", true},
		{proxyPlacementHelper, "127.0.0.1:8091", false},
		{proxyPlacementHelper, "0.0.0.0:0", false},
	} {
		t.Run(string(tc.placement)+"/"+tc.addr, func(t *testing.T) {
			err := validateCredentialProxyBindAddr(tc.addr, tc.placement)
			if tc.wantOK && err != nil {
				t.Fatalf("bind %q rejected in %s placement: %v", tc.addr, tc.placement, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("bind %q accepted in %s placement", tc.addr, tc.placement)
			}
		})
	}
}

func TestCredentialProxyPlacementFailsClosed(t *testing.T) {
	t.Setenv(proxyPlacementEnv, "")
	if got, err := credentialProxyPlacementFromEnv(); err != nil || got != proxyPlacementSidecar {
		t.Fatalf("unset placement = %q (%v), want the restrictive sidecar placement", got, err)
	}
	t.Setenv(proxyPlacementEnv, "anywhere")
	if _, err := credentialProxyPlacementFromEnv(); err == nil {
		t.Fatal("unrecognised placement accepted; it must fail rather than default")
	}
}

// claude-code's tool surface is the session's marketplace plugins, none unless
// it chose some (AC-L1). No Claude-CLI type sits
// behind the gate now (minimax-code is the gated type and has its own surface,
// minimax_tools.go), so agentToolSurface knows no other Claude workload.
func TestClaudeCodeToolSurfaceIsUnchanged(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", claudeProxyBaseURL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	tools, err := agentToolSurface(workloadClaudeCode)
	if err != nil {
		t.Fatalf("claude-code environment rejected: %v", err)
	}
	if len(tools.Plugins) != 0 || tools.SessionMCP != "" {
		t.Fatalf("claude-code tool surface = %+v, want no plugins for a session that chose none", tools)
	}
}

func readManagedSettings(t *testing.T, homeDir string) claudeManagedSettings {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(homeDir, claudeSettingsDir, claudeSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	var settings claudeManagedSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

// The Claude CLI has one tool surface, the marketplace plugins (AC-E6). A
// settings file that permits or registers an MCP server is not one the platform
// ever wrote, so an archive carrying it is refused rather than normalised.
func TestManagedSettingsRefuseAnMCPSurface(t *testing.T) {
	for name, edit := range map[string]func(*claudeManagedSettings){
		"permits the session MCP": func(s *claudeManagedSettings) {
			s.Permissions.Allow = append(s.Permissions.Allow, "mcp__"+sessionMCPServerName)
		},
		"registers an MCP server": func(s *claudeManagedSettings) {
			s.MCPServers = map[string]json.RawMessage{sessionMCPServerName: json.RawMessage(`{"type":"http","url":"http://10.42.0.9:8092/mcp"}`)}
		},
		"lists a plugin as disabled": func(s *claudeManagedSettings) {
			s.EnabledPlugins = map[string]bool{testClaudePlugin: false}
		},
	} {
		t.Run(name, func(t *testing.T) {
			homeDir := t.TempDir()
			if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugins: []string{testClaudePlugin}}); err != nil {
				t.Fatal(err)
			}
			if err := validateClaudeManagedSettings(homeDir); err != nil {
				t.Fatalf("platform-written settings rejected: %v", err)
			}
			settings := readManagedSettings(t, homeDir)
			edit(&settings)
			settingsDir := filepath.Join(homeDir, claudeSettingsDir)
			if err := storeClaudeManagedSettings(settingsDir, filepath.Join(settingsDir, claudeSettingsFile), settings); err != nil {
				t.Fatal(err)
			}
			if err := validateClaudeManagedSettings(homeDir); err == nil {
				t.Fatal("settings accepted")
			}
		})
	}
}
