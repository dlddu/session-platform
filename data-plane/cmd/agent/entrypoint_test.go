package main

import (
	"os"
	"os/exec"
	"path/filepath"
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

// AC-E6: the entrypoint bootstraps nothing for claude-code, whatever the
// session chose — no K3s MCP token request, no marketplace, no install. The
// agent installs the sidecar's plugin seed (claude_plugins.go).
func TestEntrypointStartsTheClaudeCodeAgentWithoutABootstrap(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	writeEntrypointTestExecutable(t, dir, "curl", `printf 'curl\n' >> "$CALL_LOG"`)
	writeEntrypointTestExecutable(t, dir, "git", `printf 'git\n' >> "$CALL_LOG"`)
	writeEntrypointTestExecutable(t, dir, "claude", `printf 'claude:%s\n' "$*" >> "$CALL_LOG"`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `printf 'agent:%s\n' "$*" >> "$CALL_LOG"`)
	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"), "--flag")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=claude-code",
		"DATA_PLANE_AGENT_BIN="+agent,
		"CLAUDE_CODE_PLUGINS=development,mcp-servers",
		"CALL_LOG="+logPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, output)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "agent:--flag\n" {
		t.Fatalf("calls = %q, want the agent alone, with its arguments", got)
	}
}
