package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-A5 (docs/prd/architecture.md): the platform writes nothing into the
// model's input. Behaviour tests elsewhere pin each path that used to — the
// codex config's developer_instructions, the scheduler's run header, the
// skill-change notice, the runners' platform messages and follow-up turns.
// This tripwire covers the doors those tests do not: a new instructions key,
// an appended system prompt, a CLAUDE.md or AGENTS.md written by the image.
// Finding one of these means the change needs the AC amended first.
func TestNoPromptHarnessInPlatformSources(t *testing.T) {
	forbidden := []string{
		`"instructions"`,         // MCP initialize instructions, codex config key
		"developer_instructions", // codex config
		"base_instructions",      // codex config
		"developerInstructions",  // codex app-server thread params
		"baseInstructions",       // codex app-server thread params
		"appendSystemPrompt",     // Agent SDK option
		"append-system-prompt",   // Claude Code flag
		"--system-prompt",        // Claude Code flag
		"CLAUDE.md",              // memory files the agent would load
		"AGENTS.md",
	}
	roots := []struct{ dir, ext, testSuffix string }{
		{".", ".go", "_test.go"},
		{"../../claude-runner", ".mjs", ".test.mjs"},
		{"../../codex-runner", ".mjs", ".test.mjs"},
		{"../..", ".sh", ""},
		{"../..", "Dockerfile", ""},
	}
	checked := 0
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, root.ext) || (root.testSuffix != "" && strings.HasSuffix(name, root.testSuffix)) {
				continue
			}
			path := filepath.Join(root.dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			for _, word := range forbidden {
				if strings.Contains(string(data), word) {
					t.Errorf("%s contains %q: the platform must not shape the model's input with prompts (AC-A5)", path, word)
				}
			}
		}
	}
	if checked < 10 {
		t.Fatalf("scanned only %d files: the guard is not looking where the code is", checked)
	}

	// The claude-code system prompt is the CLI's own preset, nothing added.
	lib, err := os.ReadFile("../../claude-runner/lib.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lib), `systemPrompt: { type: "preset", preset: "claude_code" },`) {
		t.Error(`claude-runner must pass the plain claude_code preset as its system prompt (AC-A5)`)
	}
}
