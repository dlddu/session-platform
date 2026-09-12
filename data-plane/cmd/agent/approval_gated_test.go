package main

import (
	"encoding/json"
	"net/http"
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
		{"session mcp carrying a path", map[string]string{sessionMCPURLEnv: "http://10.42.0.9:8092/mcp"}},
		{"session mcp with a trailing slash", map[string]string{sessionMCPURLEnv: "http://10.42.0.9:8092/"}},
		{"provider endpoint carrying a path", map[string]string{"ANTHROPIC_BASE_URL": "http://10.42.0.9:8091/v1"}},
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

func readCLIConfig(t *testing.T, homeDir string) map[string]json.RawMessage {
	t.Helper()
	config, err := readClaudeCLIConfig(filepath.Join(homeDir, claudeCLIConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func registeredSessionMCP(t *testing.T, homeDir string) claudeMCPServer {
	t.Helper()
	servers, err := loadClaudeMCPRegistration(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	return servers[sessionMCPServerName]
}

// AC-F6: the session MCP is registered as the agent's only outward tool
// surface, and the marketplace plugin is not enabled. *Where* it is registered
// is the load-bearing part (claude_mcp_registration.go) — nothing else here
// fails when it is wrong, which is how it shipped wrong.
func TestApprovalGatedManagedSettingsRegisterOnlyTheSessionMCP(t *testing.T) {
	homeDir := t.TempDir()
	const mcpURL = "http://10.42.0.9:8092"
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: mcpURL}); err != nil {
		t.Fatalf("write managed settings: %v", err)
	}
	if server := registeredSessionMCP(t, homeDir); server.URL != mcpURL+sessionMCPPath || server.Type != "http" {
		t.Fatalf("registered session MCP = %+v, want the helper address' MCP endpoint over http", server)
	}
	settings := readManagedSettings(t, homeDir)
	if len(settings.MCPServers) != 0 {
		t.Fatalf("settings.json carries mcpServers = %v; the CLI never reads it there", settings.MCPServers)
	}
	if len(settings.EnabledPlugins) != 0 {
		t.Fatalf("enabledPlugins = %v, want none", settings.EnabledPlugins)
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

// Drives the real mux rather than comparing the two strings: the registering
// side and the serving side are in different files, and a disagreement between
// them costs nothing here but every tool call on the SUT (run 34685103754).
func TestRegisteredSessionMCPAnswersTheCLIsHandshake(t *testing.T) {
	srv := newSessionMCPServer(t)
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: srv.URL}); err != nil {
		t.Fatalf("write managed settings: %v", err)
	}

	registered := registeredSessionMCP(t, homeDir)
	resp, err := http.Post(registered.URL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize at the registered URL %q = %d, want 200", registered.URL, resp.StatusCode)
	}
	var body struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Result.ServerInfo.Name != sessionMCPServerName {
		t.Fatalf("the registered URL answered as %q, want the session MCP %s",
			body.Result.ServerInfo.Name, sessionMCPServerName)
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
	if got := registeredSessionMCP(t, homeDir).URL; got != "http://10.42.1.4:8092"+sessionMCPPath {
		t.Fatalf("session MCP = %q, want this round's helper pod", got)
	}
	servers, err := loadClaudeMCPRegistration(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("registered servers = %v, want the stale entry replaced rather than accumulated", servers)
	}
}

// The merge contract (claude_mcp_registration.go): anything the CLI put in its
// config has to survive the round trip.
func TestMCPRegistrationPreservesTheCLIsOwnConfig(t *testing.T) {
	homeDir := t.TempDir()
	existing := []byte(`{
	  "machineID": "m-1",
	  "userID": "u-1",
	  "projects": {"/session/state/workspace": {"lastSessionId": "s-1"}},
	  "mcpServers": {"something-the-session-added": {"type": "http", "url": "http://10.42.0.9:9000"}}
	}`)
	if err := os.WriteFile(filepath.Join(homeDir, claudeCLIConfigFile), existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.0.9:8092"}); err != nil {
		t.Fatal(err)
	}

	config := readCLIConfig(t, homeDir)
	for key, want := range map[string]string{"machineID": `"m-1"`, "userID": `"u-1"`} {
		if string(config[key]) != want {
			t.Fatalf("%s = %s, want the CLI's own value %s preserved", key, config[key], want)
		}
	}
	if !strings.Contains(string(config["projects"]), "s-1") {
		t.Fatalf("projects = %s, want the CLI's conversation state preserved", config["projects"])
	}
	servers, err := loadClaudeMCPRegistration(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := servers["something-the-session-added"].URL; got != "http://10.42.0.9:9000" {
		t.Fatalf("session's own MCP server = %q, want it left alone", got)
	}
	if got := servers[sessionMCPServerName].URL; got != "http://10.42.0.9:8092"+sessionMCPPath {
		t.Fatalf("session MCP = %q, want it merged in alongside", got)
	}
}

// A snapshot taken while the registration was written into settings.json still
// has to restore: rejecting it would cost the session its state.
func TestArchivedSettingsRegistrationIsAcceptedThenMoved(t *testing.T) {
	homeDir := t.TempDir()
	settingsDir := filepath.Join(homeDir, claudeSettingsDir)
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := managedSettingsFor(toolSurface{SessionMCP: "http://10.42.0.9:8092"})
	legacy.MCPServers = map[string]claudeMCPServer{
		sessionMCPServerName: {Type: "http", URL: "http://10.42.0.9:8092"},
	}
	if err := storeClaudeManagedSettings(settingsDir, filepath.Join(settingsDir, claudeSettingsFile), legacy); err != nil {
		t.Fatal(err)
	}
	if err := validateClaudeManagedSettings(homeDir); err != nil {
		t.Fatalf("archive with the old registration site rejected: %v", err)
	}

	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.1.4:8092"}); err != nil {
		t.Fatal(err)
	}
	if got := registeredSessionMCP(t, homeDir).URL; got != "http://10.42.1.4:8092"+sessionMCPPath {
		t.Fatalf("session MCP = %q, want it moved to the CLI config at this round's address", got)
	}
	if servers := readManagedSettings(t, homeDir).MCPServers; len(servers) != 0 {
		t.Fatalf("settings.json still carries mcpServers = %v, want the unread copy dropped", servers)
	}
}

// claude-code has no session MCP (AC-F6's 2026-09-03 decision). A stale
// registration must be cleared rather than left pointing at a dead helper pod.
func TestPluginToolSurfaceClearsAnyMCPRegistration(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.0.9:8092"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugin: true}); err != nil {
		t.Fatal(err)
	}
	servers, err := loadClaudeMCPRegistration(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := servers[sessionMCPServerName]; ok {
		t.Fatalf("registered servers = %v, want the session MCP cleared", servers)
	}
}

// The two surfaces are alternatives, not a menu: a session HOME must never
// carry both, in either direction.
func TestManagedSettingsRejectMixedToolSurfaces(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.0.9:8092"}); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(homeDir, claudeSettingsDir)
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

// AC-F5's reading half: the path the MCP hands back is only useful if the
// agent's file tools may open it, and they are scoped to its own state tree
// otherwise.
func TestApprovalGatedManagedSettingsOpenTheSharedVolumeToTheAgent(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir,
		toolSurface{SessionMCP: "http://10.42.0.9:8092", SharedDir: "/shared"}); err != nil {
		t.Fatalf("write managed settings: %v", err)
	}
	settings := readManagedSettings(t, homeDir)
	if got := settings.Permissions.AdditionalDirectories; len(got) != 1 || got[0] != "/shared" {
		t.Fatalf("additionalDirectories = %v, want exactly the shared volume", got)
	}
	if err := validateClaudeManagedSettings(homeDir); err != nil {
		t.Fatalf("managed settings are invalid: %v", err)
	}
}

// The opt-in's other side: no volume, no widened file scope. A directory named
// here that the pod does not hold would be a permission granted for nothing.
func TestManagedSettingsWidenNothingWithoutASharedVolume(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.0.9:8092"}); err != nil {
		t.Fatal(err)
	}
	if got := readManagedSettings(t, homeDir).Permissions.AdditionalDirectories; len(got) != 0 {
		t.Fatalf("additionalDirectories = %v, want none", got)
	}
	if err := ensureClaudeManagedSettings(homeDir, toolSurface{Plugin: true}); err != nil {
		t.Fatal(err)
	}
	if got := readManagedSettings(t, homeDir).Permissions.AdditionalDirectories; len(got) != 0 {
		t.Fatalf("claude-code additionalDirectories = %v, want none — that type has no shared volume", got)
	}
}

// An archive carries the previous round's settings, and the previous round's
// volume is gone with its claim. Normalising the list rather than merging it is
// what keeps a stale directory out of this round's permissions (AC-F4/AC-F5).
func TestRestoredManagedSettingsDropAStaleSharedDirectory(t *testing.T) {
	homeDir := t.TempDir()
	if err := ensureClaudeManagedSettings(homeDir,
		toolSurface{SessionMCP: "http://10.42.0.9:8092", SharedDir: "/shared-old"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeManagedSettings(homeDir,
		toolSurface{SessionMCP: "http://10.42.1.4:8092", SharedDir: "/shared"}); err != nil {
		t.Fatalf("re-point managed settings: %v", err)
	}
	got := readManagedSettings(t, homeDir).Permissions.AdditionalDirectories
	if len(got) != 1 || got[0] != "/shared" {
		t.Fatalf("additionalDirectories = %v, want only this round's volume", got)
	}
}

// The permission and the registration are two halves of one surface; neither
// half alone is a valid archive.
func TestManagedSettingsRejectHalfDeclaredMCPSurface(t *testing.T) {
	t.Run("permitted but not registered", func(t *testing.T) {
		homeDir := t.TempDir()
		settingsDir := filepath.Join(homeDir, claudeSettingsDir)
		if err := os.MkdirAll(settingsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		settings := managedSettingsFor(toolSurface{SessionMCP: "http://10.42.0.9:8092"})
		if err := storeClaudeManagedSettings(settingsDir, filepath.Join(settingsDir, claudeSettingsFile), settings); err != nil {
			t.Fatal(err)
		}
		if err := validateClaudeManagedSettings(homeDir); err == nil {
			t.Fatal("a permitted-but-unregistered session MCP was accepted")
		}
	})
	t.Run("registered but not permitted", func(t *testing.T) {
		homeDir := t.TempDir()
		if err := ensureClaudeManagedSettings(homeDir, toolSurface{SessionMCP: "http://10.42.0.9:8092"}); err != nil {
			t.Fatal(err)
		}
		settingsDir := filepath.Join(homeDir, claudeSettingsDir)
		stripped := managedSettingsFor(toolSurface{})
		if err := storeClaudeManagedSettings(settingsDir, filepath.Join(settingsDir, claudeSettingsFile), stripped); err != nil {
			t.Fatal(err)
		}
		if err := validateClaudeManagedSettings(homeDir); err == nil {
			t.Fatal("a registered-but-unpermitted session MCP was accepted")
		}
	})
}
