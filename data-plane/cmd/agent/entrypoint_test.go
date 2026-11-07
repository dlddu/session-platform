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

// runClaudeEntrypointWithPlugins runs the claude-code bootstrap against stub
// curl/claude/agent binaries with CLAUDE_CODE_PLUGINS set to plugins, and
// returns the recorded calls and the entrypoint's error.
func runClaudeEntrypointWithPlugins(t *testing.T, plugins string) (string, []byte, error) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	writeEntrypointTestExecutable(t, dir, "curl", `printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"isError":false,"content":[{"type":"resource","resource":{"uri":"file:///github-token.env","mimeType":"text/plain","text":"GITHUB_TOKEN=test-installation-token\n"}}]}}'`)
	writeEntrypointTestExecutable(t, dir, "claude", `printf 'claude:%s\n' "$*" >> "$CALL_LOG"`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `
[ "$CLAUDE_CODE_PLUGINS" = "$EXPECTED_PLUGINS" ]
printf 'agent\n' >> "$CALL_LOG"
`)
	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=claude-code",
		"DATA_PLANE_AGENT_BIN="+agent,
		"K3S_MCP_TOKEN=test-k3s-mcp-token",
		"CLAUDE_CODE_PLUGIN_CACHE_DIR="+filepath.Join(dir, "cache"),
		"CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME="+filepath.Join(dir, "home"),
		"CLAUDE_CODE_PLUGINS="+plugins,
		"EXPECTED_PLUGINS="+plugins,
		"CALL_LOG="+logPath,
	)
	output, err := cmd.CombinedOutput()
	data, _ := os.ReadFile(logPath)
	return string(data), output, err
}

// AC-L1: a session's own selection replaces the platform plugin, each name is
// installed from the platform marketplace in order, and the agent sees the
// same list so it enables exactly what was installed.
func TestEntrypointInstallsTheSessionsPluginSelection(t *testing.T) {
	calls, output, err := runClaudeEntrypointWithPlugins(t, "development,dear-baby")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	want := strings.Join([]string{
		"claude:plugin marketplace add https://github.com/dlddu/plugin-marketplace.git",
		"claude:plugin install development@dlddu-plugins",
		"claude:plugin install dear-baby@dlddu-plugins",
		"agent",
		"",
	}, "\n")
	if calls != want {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

// A malformed selection stops the pod before anything is installed or the
// agent starts, rather than handing claude a name it would read as a flag or
// a different marketplace.
func TestEntrypointRefusesAnInvalidPluginSelection(t *testing.T) {
	for _, plugins := range []string{"", "development,,dear-baby", "-x", "dev@other", "a b", "*", "dev*"} {
		calls, output, err := runClaudeEntrypointWithPlugins(t, plugins)
		if err == nil {
			t.Errorf("CLAUDE_CODE_PLUGINS=%q: entrypoint succeeded, want failure\n%s", plugins, output)
		}
		if strings.Contains(calls, "plugin install") || strings.Contains(calls, "agent") {
			t.Errorf("CLAUDE_CODE_PLUGINS=%q: calls = %q, want nothing installed or started", plugins, calls)
		}
	}
}
