package main

import (
	"bytes"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// AC-M1: codex-task's config opens the sandbox and keeps the shell, and turns
// off everything else codex-chat turns off.
func TestRenderCodexTaskConfig(t *testing.T) {
	doc := tomlCheck(t, string(renderCodexConfig(codexConfig{
		task:        true,
		k3sMCPURL:   "http://127.0.0.1:8093/mcp",
		agentBinary: "/agent",
		skillsDir:   "/session/state/home/.agents/skills",
	})))
	for key, want := range map[string]any{
		"approval_policy":            "never",
		"sandbox_mode":               "danger-full-access",
		"web_search":                 "disabled",
		"cli_auth_credentials_store": "file",
		"forced_login_method":        "chatgpt",
		"skills.bundled.enabled":     false,
		"mcp_servers.k3s.url":        "http://127.0.0.1:8093/mcp",
		"mcp_servers.skills.command": "/agent",
	} {
		if got := doc[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	for _, feature := range []string{"shell_tool", "unified_exec"} {
		if _, set := doc["features."+feature]; set {
			t.Errorf("features.%s must stay on in codex-task", feature)
		}
	}
	for _, feature := range codexTaskDisabledFeatures {
		if doc["features."+feature] != false {
			t.Errorf("features.%s = %#v, want false", feature, doc["features."+feature])
		}
	}
	instructions, _ := doc["developer_instructions"].(string)
	if !strings.Contains(instructions, codexTaskInstructionsMarker) || strings.Contains(instructions, codexInstructionsMarker) {
		t.Errorf("developer_instructions = %q, want only the task marker", instructions)
	}
}

func TestRunAgentSubcommandRendersTaskConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	var stderr bytes.Buffer
	code, handled := runAgentSubcommand([]string{codexRenderConfigCommand, home, "task"}, nil, io.Discard, &stderr)
	if !handled || code != 0 {
		t.Fatalf("render = %d/%v: %s", code, handled, stderr.String())
	}
	doc := tomlCheck(t, readFile(t, filepath.Join(home, ".codex", "config.toml")))
	if doc["sandbox_mode"] != "danger-full-access" {
		t.Errorf("rendered task config = %v", doc)
	}
	if code, _ := runAgentSubcommand([]string{codexRenderConfigCommand, home, "shell"}, nil, io.Discard, io.Discard); code != 2 {
		t.Error("an unknown mode is a usage error")
	}
}

func TestCodexChatArgvHasNoMode(t *testing.T) {
	got := (codexCLI{}).argv("r", "gpt-6", "p", true)
	if !reflect.DeepEqual(got, []string{"r", "--model", "gpt-6", "--resume", "--", "p"}) {
		t.Fatalf("chat argv = %q", got)
	}
	if (codexCLI{}).workloadName() != workloadCodexChat || (codexCLI{task: true}).workloadName() != workloadCodexTask {
		t.Fatal("workload names")
	}
}
