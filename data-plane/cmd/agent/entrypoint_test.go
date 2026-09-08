package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeEntrypointTestExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestEntrypointSkipsPluginBootstrapForShell(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	agent := writeEntrypointTestExecutable(t, dir, "agent", `printf 'agent\n' >> "$CALL_LOG"`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"DATA_PLANE_WORKLOAD=shell",
		"DATA_PLANE_AGENT_BIN="+agent,
		"CALL_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "agent\n" {
		t.Fatalf("calls = %q, want agent only", got)
	}
}

func TestEntrypointBootstrapsPrivateMarketplaceWithScopedAuthHeaderWithoutLeakingGitHubTokenToAgent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	cacheDir := filepath.Join(dir, "plugin-cache")
	bootstrapHome := filepath.Join(dir, "bootstrap-home")
	writeEntrypointTestExecutable(t, dir, "curl", `printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"isError":false,"content":[{"type":"resource","resource":{"uri":"file:///github-token.env","mimeType":"text/plain","text":"# Expires at: 2099-01-01T00:00:00Z\nGITHUB_TOKEN=test-installation-token\n"}}]}}'`)
	writeEntrypointTestExecutable(t, dir, "claude", `
[ "$HOME" = "$EXPECTED_BOOTSTRAP_HOME" ]
[ "$CLAUDE_CODE_PLUGIN_CACHE_DIR" = "$EXPECTED_CACHE_DIR" ]
[ "$GIT_CONFIG_COUNT" = "1" ]
[ "$GIT_CONFIG_KEY_0" = "http.https://github.com/dlddu/plugin-marketplace.git.extraheader" ]
[ "$GIT_CONFIG_VALUE_0" = "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dGVzdC1pbnN0YWxsYXRpb24tdG9rZW4=" ]
[ "$GIT_TERMINAL_PROMPT" = "0" ]
[ "${SESSION_PLATFORM_GITHUB_TOKEN+x}" != "x" ]
printf 'claude:%s\n' "$*" >> "$CALL_LOG"
`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `
[ "$CLAUDE_CODE_PLUGIN_CACHE_DIR" = "$EXPECTED_CACHE_DIR" ]
[ "${CLAUDE_CODE_PLUGIN_SEED_DIR+x}" != "x" ]
[ "${SESSION_PLATFORM_GITHUB_TOKEN+x}" != "x" ]
[ "${GIT_CONFIG_COUNT+x}" != "x" ]
[ "${GIT_CONFIG_KEY_0+x}" != "x" ]
[ "${GIT_CONFIG_VALUE_0+x}" != "x" ]
printf 'agent\n' >> "$CALL_LOG"
`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=claude-code",
		"DATA_PLANE_AGENT_BIN="+agent,
		"K3S_MCP_TOKEN=test-k3s-mcp-token",
		"K3S_MCP_URL=https://k3s-mcp.example.test/mcp",
		"CLAUDE_CODE_PLUGIN_CACHE_DIR="+cacheDir,
		"CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME="+bootstrapHome,
		"EXPECTED_CACHE_DIR="+cacheDir,
		"EXPECTED_BOOTSTRAP_HOME="+bootstrapHome,
		"CALL_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"claude:plugin marketplace add https://github.com/dlddu/plugin-marketplace.git",
		"claude:plugin install session-platform@dlddu-plugins",
		"agent",
		"",
	}, "\n")
	if got := string(data); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// The marketplace is a plain git remote, so the platform can serve it from
// anywhere that carries the same repository shape (the kind e2e SUT deploys an
// in-cluster one). Overriding the URL must move BOTH the clone target and the
// scope of the auth header — a header scoped to github.com would silently stop
// applying, and a header scoped too widely would leak the token to other hosts.
func TestEntrypointPointsMarketplaceAndAuthHeaderAtTheConfiguredRemote(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	cacheDir := filepath.Join(dir, "plugin-cache")
	bootstrapHome := filepath.Join(dir, "bootstrap-home")
	const remote = "http://plugin-marketplace-git:8080/plugin-marketplace.git"
	const mcpURL = "http://k3s-mcp-fake:8080/mcp"

	writeEntrypointTestExecutable(t, dir, "curl", `
for mcp_target in "$@"; do :; done
[ "$mcp_target" = "$EXPECTED_K3S_MCP_URL" ]
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"isError":false,"content":[{"type":"resource","resource":{"uri":"file:///github-token.env","mimeType":"text/plain","text":"# Expires at: 2099-01-01T00:00:00Z\nGITHUB_TOKEN=test-installation-token\n"}}]}}'`)
	writeEntrypointTestExecutable(t, dir, "claude", `
[ "$GIT_CONFIG_COUNT" = "1" ]
[ "$GIT_CONFIG_KEY_0" = "http.$EXPECTED_REMOTE.extraheader" ]
[ "$GIT_CONFIG_VALUE_0" = "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dGVzdC1pbnN0YWxsYXRpb24tdG9rZW4=" ]
printf 'claude:%s\n' "$*" >> "$CALL_LOG"
`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `printf 'agent\n' >> "$CALL_LOG"`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=claude-code",
		"DATA_PLANE_AGENT_BIN="+agent,
		"K3S_MCP_TOKEN=test-k3s-mcp-token",
		"K3S_MCP_URL="+mcpURL,
		"EXPECTED_K3S_MCP_URL="+mcpURL,
		"CLAUDE_CODE_PLUGIN_MARKETPLACE_URL="+remote,
		"CLAUDE_CODE_PLUGIN_CACHE_DIR="+cacheDir,
		"CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME="+bootstrapHome,
		"EXPECTED_REMOTE="+remote,
		"CALL_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"claude:plugin marketplace add " + remote,
		"claude:plugin install session-platform@dlddu-plugins",
		"agent",
		"",
	}, "\n")
	if got := string(data); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// writeSeedFixtureMarketplace builds a git remote shaped like the real
// marketplace — plugins that declare an MCP server, and the file they point at.
func writeSeedFixtureMarketplace(t *testing.T, dir string) string {
	t.Helper()
	repo := filepath.Join(dir, "marketplace-source")
	if err := os.MkdirAll(filepath.Join(repo, ".claude-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "mcp"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"dlddu-plugins","plugins":[{"name":"session-platform","source":"./",` +
		`"skills":["./skillset/git-repo-cloner"],"mcpServers":["./mcp/k3s.json"]}]}`
	if err := os.WriteFile(filepath.Join(repo, ".claude-plugin", "marketplace.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "mcp", "k3s.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@invalid", "add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	return repo
}

// The helper pod publishes the plugin for a workload pod that cannot reach the
// marketplace itself, and strips every MCP server on the way: one would be a
// tool surface that never meets the approval gate (AC-F3, AC-F6).
func TestEntrypointSeedsSkillsOnlyMarketplaceFromTheHelperPod(t *testing.T) {
	dir := t.TempDir()
	sharedDir := filepath.Join(dir, "shared")
	if err := os.MkdirAll(sharedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := writeSeedFixtureMarketplace(t, dir)
	writeEntrypointTestExecutable(t, dir, "curl", `printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"isError":false,"content":[{"type":"resource","resource":{"text":"GITHUB_TOKEN=test-installation-token\n"}}]}}'`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `printf 'agent\n' >> "$CALL_LOG"`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=mcp",
		"DATA_PLANE_AGENT_BIN="+agent,
		"K3S_MCP_TOKEN=test-k3s-mcp-token",
		"CLAUDE_CODE_PLUGIN_MARKETPLACE_URL="+source,
		"SESSION_SHARED_DIR="+sharedDir,
		"CALL_LOG="+filepath.Join(dir, "calls.log"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}

	seed := filepath.Join(sharedDir, "plugin-marketplace")
	manifest, err := os.ReadFile(filepath.Join(seed, ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatalf("seeded marketplace unreadable: %v", err)
	}
	if strings.Contains(string(manifest), "mcpServers") {
		t.Fatalf("seeded marketplace still declares an MCP server: %s", manifest)
	}
	if _, err := os.Stat(filepath.Join(seed, "mcp")); !os.IsNotExist(err) {
		t.Fatalf("seeded marketplace kept its mcp directory (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(sharedDir, ".plugin-marketplace.staging")); !os.IsNotExist(err) {
		t.Fatalf("staging directory left behind (%v)", err)
	}
}

// The workload pod installs from the volume, so it needs no marketplace
// credential and no route off the pod (AC-F2).
func TestEntrypointInstallsTheSeededMarketplaceWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	sharedDir := filepath.Join(dir, "shared")
	seed := filepath.Join(sharedDir, "plugin-marketplace")
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEntrypointTestExecutable(t, dir, "claude", `
[ "${GIT_CONFIG_KEY_0+x}" != "x" ]
[ "${GIT_CONFIG_VALUE_0+x}" != "x" ]
printf 'claude:%s\n' "$*" >> "$CALL_LOG"
`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `
[ "$CLAUDE_CODE_PLUGIN_ENABLED" = "1" ]
printf 'agent\n' >> "$CALL_LOG"
`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=approval-gated",
		"DATA_PLANE_AGENT_BIN="+agent,
		"SESSION_SHARED_DIR="+sharedDir,
		"CLAUDE_CODE_PLUGIN_CACHE_DIR="+filepath.Join(dir, "plugin-cache"),
		"CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME="+filepath.Join(dir, "bootstrap-home"),
		"CALL_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"claude:plugin marketplace add " + seed,
		"claude:plugin install session-platform@dlddu-plugins",
		"agent",
		"",
	}, "\n")
	if got := string(data); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

// AC-F5's volume is opt-in, so its absence has to leave the session running on
// the session MCP alone rather than failing the pod.
func TestEntrypointRunsApprovalGatedWithoutASeed(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	agent := writeEntrypointTestExecutable(t, dir, "agent", `
[ "${CLAUDE_CODE_PLUGIN_ENABLED+x}" != "x" ]
printf 'agent\n' >> "$CALL_LOG"
`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"DATA_PLANE_WORKLOAD=approval-gated",
		"DATA_PLANE_AGENT_BIN="+agent,
		"SESSION_SHARED_DIR="+filepath.Join(dir, "absent"),
		"CALL_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "agent\n" {
		t.Fatalf("calls = %q, want agent only", got)
	}
}
