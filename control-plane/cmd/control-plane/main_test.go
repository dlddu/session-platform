package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
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

// The approval-gated archive gate is separate so it can be dropped on its own,
// and it inherits claude-code's value so that separating it costs no manifest
// change. Both halves are asserted here: without them a later default flip
// would silently turn the type off in every deployment that never names it.
func TestLoadConfigApprovalGatedArchiveGate(t *testing.T) {
	for _, tt := range []struct {
		name           string
		claude         string
		approvalGated  string
		wantClaude     bool
		wantApprovalAG bool
	}{
		{name: "unset inherits the enabled claude-code gate", claude: "true", approvalGated: "", wantClaude: true, wantApprovalAG: true},
		{name: "unset inherits the disabled claude-code gate", claude: "", approvalGated: "", wantClaude: false, wantApprovalAG: false},
		{name: "explicit false is the kill switch", claude: "true", approvalGated: "false", wantClaude: true, wantApprovalAG: false},
		{name: "explicit true stands on its own", claude: "", approvalGated: "true", wantClaude: false, wantApprovalAG: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_ARCHIVE_ENABLED", tt.claude)
			t.Setenv("SESSION_APPROVAL_GATED_ARCHIVE_ENABLED", tt.approvalGated)

			cfg, err := loadConfig()
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.claudeArchiveEnabled != tt.wantClaude {
				t.Errorf("claudeArchiveEnabled = %v, want %v", cfg.claudeArchiveEnabled, tt.wantClaude)
			}
			if cfg.approvalGatedArchiveEnabled != tt.wantApprovalAG {
				t.Errorf("approvalGatedArchiveEnabled = %v, want %v", cfg.approvalGatedArchiveEnabled, tt.wantApprovalAG)
			}
		})
	}
}

func TestOfferedWorkloadTypesFollowConfiguredImages(t *testing.T) {
	offered := func(cfg config) map[session.WorkloadType]bool {
		got := map[session.WorkloadType]bool{}
		for _, w := range cfg.offeredWorkloadTypes() {
			got[w] = true
		}
		return got
	}
	if got := offered(config{}); len(got) != 1 || !got[session.WorkloadTypeShell] {
		t.Fatalf("no images: offered = %v, want shell only", got)
	}
	got := offered(config{
		dataPlaneClaudeCodeImage:    "img",
		dataPlaneApprovalGatedImage: "img",
		miniMax:                     miniMaxConfig{image: "img"},
	})
	for _, w := range []session.WorkloadType{
		session.WorkloadTypeShell, session.WorkloadTypeClaudeCode,
		session.WorkloadTypeApprovalGated, session.WorkloadTypeMiniMaxCode,
	} {
		if !got[w] {
			t.Errorf("all images set: %q not offered", w)
		}
	}
	if got := offered(config{miniMax: miniMaxConfig{image: "img"}}); len(got) != 2 || !got[session.WorkloadTypeMiniMaxCode] {
		t.Fatalf("minimax image only: offered = %v, want shell + minimax-code", got)
	}
}
