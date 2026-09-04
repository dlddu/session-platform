package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func setApprovalGatedEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_BASE_URL", "http://10.42.0.9:8091")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("K3S_MCP_TOKEN", "")
	t.Setenv(sessionMCPURLEnv, "http://10.42.0.9:8092")
}

func TestApprovalGatedWorkloadAcceptsItsHelperEndpoints(t *testing.T) {
	setApprovalGatedEnv(t)
	tools, err := agentToolSurface(workloadApprovalGated)
	if err != nil {
		t.Fatalf("approval-gated environment rejected: %v", err)
	}
	if tools.SessionMCP != "http://10.42.0.9:8092" {
		t.Fatalf("session MCP = %q, want the injected helper address", tools.SessionMCP)
	}
	// AC-F6's 2026-09-03 decision: no marketplace plugin in this type.
	if tools.Plugin {
		t.Fatal("approval-gated enabled the marketplace plugin")
	}
}

func TestApprovalGatedWorkloadRejectsUnsafeCredentialWiring(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"loopback provider endpoint", map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8091"}},
		{"provider endpoint on the wrong port", map[string]string{"ANTHROPIC_BASE_URL": "http://10.42.0.9:8092"}},
		{"missing provider endpoint", map[string]string{"ANTHROPIC_BASE_URL": ""}},
		{"real provider token", map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-real"}},
		{"direct api key", map[string]string{"ANTHROPIC_API_KEY": "sk-real"}},
		{"oauth token", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth"}},
		{"k3s mcp token", map[string]string{"K3S_MCP_TOKEN": "k3s"}},
		{"missing session mcp", map[string]string{sessionMCPURLEnv: ""}},
		{"loopback session mcp", map[string]string{sessionMCPURLEnv: "http://127.0.0.1:8092"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setApprovalGatedEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := agentToolSurface(workloadApprovalGated); err == nil {
				t.Fatalf("approval-gated accepted %s", tc.name)
			}
		})
	}
}

// claude-code keeps its own wiring: same function, different branch.
func TestClaudeCodeToolSurfaceIsUnchanged(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", claudeProxyBaseURL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	tools, err := agentToolSurface(workloadClaudeCode)
	if err != nil {
		t.Fatalf("claude-code environment rejected: %v", err)
	}
	if !tools.Plugin || tools.SessionMCP != "" {
		t.Fatalf("claude-code tool surface = %+v, want the marketplace plugin only", tools)
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

// AC-F6: the session MCP is registered as the agent's only outward tool
// surface, and the marketplace plugin is not enabled.
func TestApprovalGatedManagedSettingsRegisterOnlyTheSessionMCP(t *testing.T) {
	homeDir := t.TempDir()
	const mcpURL = "http://10.42.0.9:8092"
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: mcpURL}); err != nil {
		t.Fatalf("write managed settings: %v", err)
	}
	settings := readManagedSettings(t, homeDir)
	if len(settings.EnabledPlugins) != 0 {
		t.Fatalf("enabledPlugins = %v, want none", settings.EnabledPlugins)
	}
	server, ok := settings.MCPServers[sessionMCPServerName]
	if !ok || server.URL != mcpURL || server.Type != "http" {
		t.Fatalf("mcpServers = %v, want the session MCP over http", settings.MCPServers)
	}
	if len(settings.MCPServers) != 1 {
		t.Fatalf("mcpServers = %v, want exactly one", settings.MCPServers)
	}
	for _, tool := range claudeManagedTools {
		if !containsString(settings.Permissions.Allow, tool) {
			t.Fatalf("permissions lost AC-E2's %s", tool)
		}
	}
	if !containsString(settings.Permissions.Allow, sessionMCPPermission) {
		t.Fatalf("permissions = %v, want the session MCP permitted", settings.Permissions.Allow)
	}
	if err := validateClaudeManagedSettings(homeDir); err != nil {
		t.Fatalf("approval-gated managed settings are invalid: %v", err)
	}
}

// ensureClaudeManagedSettings' normalise-rather-than-trust rule, across two
// rounds of helper pod addresses (AC-F4).
func TestRestoredManagedSettingsArePointedAtTheCurrentSessionMCP(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.0.9:8092"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.1.4:8092"}); err != nil {
		t.Fatalf("re-point managed settings: %v", err)
	}
	settings := readManagedSettings(t, homeDir)
	if got := settings.MCPServers[sessionMCPServerName].URL; got != "http://10.42.1.4:8092" {
		t.Fatalf("session MCP = %q, want this round's helper pod", got)
	}
	if n := len(settings.MCPServers); n != 1 {
		t.Fatalf("mcpServers = %d, want the stale entry replaced rather than accumulated", n)
	}
}

// The two surfaces are alternatives, not a menu: a settings file must never
// carry both, in either direction.
func TestManagedSettingsRejectMixedToolSurfaces(t *testing.T) {
	homeDir := t.TempDir()
	settingsDir := filepath.Join(homeDir, claudeSettingsDir)
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mixed := managedSettingsFor(toolSurface{SessionMCP: "http://10.42.0.9:8092"})
	mixed.EnabledPlugins = map[string]bool{claudeSessionPlatformPlugin: true}
	if err := storeClaudeManagedSettings(settingsDir, filepath.Join(settingsDir, claudeSettingsFile), mixed); err != nil {
		t.Fatal(err)
	}
	err := validateClaudeManagedSettings(homeDir)
	if err == nil || !strings.Contains(err.Error(), "tool surface") {
		t.Fatalf("validation error = %v, want a mixed-surface rejection", err)
	}
}
