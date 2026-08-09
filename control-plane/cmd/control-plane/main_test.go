package main

import (
	"reflect"
	"testing"
)

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
			name:  "ordered catalog",
			value: `["claude-model-a","provider/model:b","claude.model-c"]`,
			want:  []string{"claude-model-a", "provider/model:b", "claude.model-c"},
		},
		{name: "malformed JSON", value: `[`, wantErr: true},
		{name: "not an array", value: `{"model":"claude-model-a"}`, wantErr: true},
		{name: "null", value: `null`, wantErr: true},
		{name: "empty entry", value: `[""]`, wantErr: true},
		{name: "surrounding whitespace", value: `[" claude-model-a"]`, wantErr: true},
		{name: "invalid model", value: `["bad model"]`, wantErr: true},
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
	t.Setenv("CLAUDE_CODE_MODELS", `["claude-model-a","claude-model-b"]`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"claude-model-a", "claude-model-b"}
	if !reflect.DeepEqual(cfg.claudeCodeModels, want) {
		t.Fatalf("claudeCodeModels = %#v, want %#v", cfg.claudeCodeModels, want)
	}
}
