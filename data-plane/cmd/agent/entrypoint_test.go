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
[ "$CLAUDE_CODE_PLUGIN_SEED_DIR" = "$EXPECTED_CACHE_DIR" ]
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
