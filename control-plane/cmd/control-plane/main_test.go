package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseClaudeCodeDefaultModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "unset", want: "platform-default"},
		{name: "ordinary model", value: "anthropic/claude-sonnet-4", want: "anthropic/claude-sonnet-4"},
		{name: "OpenRouter latest alias", value: "~deepseek/deepseek-v4-flash-latest", want: "~deepseek/deepseek-v4-flash-latest"},
		{name: "reserved alias", value: "platform-default", wantErr: true},
		{name: "surrounding whitespace", value: " claude-model-a", wantErr: true},
		{name: "trailing whitespace", value: "claude-model-a ", wantErr: true},
		{name: "invalid model", value: "bad model", wantErr: true},
		{name: "too long", value: strings.Repeat("a", 129), wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseClaudeCodeDefaultModel(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseClaudeCodeDefaultModel(%q) succeeded, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClaudeCodeDefaultModel(%q): %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("parseClaudeCodeDefaultModel(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseClaudeCodeModels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr bool
	}{
		{name: "unset", want: []string{}},
		{name: "empty array", value: `[]`, want: []string{}},
		{
			name:  "ordered catalog including OpenRouter latest aliases",
			value: `["~deepseek/deepseek-v4-flash-latest","~anthropic/claude-opus-latest","xiaomi/mimo-v2.5","inclusionai/ling-3.0-flash"]`,
			want: []string{
				"~deepseek/deepseek-v4-flash-latest",
				"~anthropic/claude-opus-latest",
				"xiaomi/mimo-v2.5",
				"inclusionai/ling-3.0-flash",
			},
		},
		{name: "malformed JSON", value: `[`, wantErr: true},
		{name: "not an array", value: `{"model":"claude-model-a"}`, wantErr: true},
		{name: "null", value: `null`, wantErr: true},
		{name: "empty entry", value: `[""]`, wantErr: true},
		{name: "surrounding whitespace", value: `[" claude-model-a"]`, wantErr: true},
		{name: "invalid model", value: `["bad model"]`, wantErr: true},
		{name: "bare latest alias prefix", value: `["~"]`, wantErr: true},
		{name: "repeated latest alias prefix", value: `["~~anthropic/claude-opus-latest"]`, wantErr: true},
		{name: "reserved alias", value: `["platform-default"]`, wantErr: true},
		{name: "duplicate", value: `["claude-model-a","claude-model-a"]`, wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseClaudeCodeModels(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseClaudeCodeModels(%q) succeeded, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClaudeCodeModels(%q): %v", tt.value, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseClaudeCodeModels(%q) = %#v, want %#v", tt.value, got, tt.want)
			}
		})
	}
}

func TestLoadConfigReadsClaudeCodeModels(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DEFAULT_MODEL", "~anthropic/claude-opus-latest")
	t.Setenv("CLAUDE_CODE_MODELS", `["claude-model-a","claude-model-b"]`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"claude-model-a", "claude-model-b"}
	if !reflect.DeepEqual(cfg.claudeCodeModels, want) {
		t.Fatalf("claudeCodeModels = %#v, want %#v", cfg.claudeCodeModels, want)
	}
	if got, want := cfg.claudeCodeDefaultModel, "~anthropic/claude-opus-latest"; got != want {
		t.Fatalf("claudeCodeDefaultModel = %q, want %q", got, want)
	}
}

func TestLoadConfigDefaultsClaudeCodeDefaultModel(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DEFAULT_MODEL", "")
	t.Setenv("CLAUDE_CODE_MODELS", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got, want := cfg.claudeCodeDefaultModel, "platform-default"; got != want {
		t.Fatalf("claudeCodeDefaultModel = %q, want %q", got, want)
	}
}

func TestLoadConfigRejectsInvalidClaudeCodeDefaultModel(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DEFAULT_MODEL", "platform-default")

	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_DEFAULT_MODEL") {
		t.Fatalf("loadConfig error = %v, want CLAUDE_CODE_DEFAULT_MODEL error", err)
	}
}
