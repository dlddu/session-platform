package session

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// AC-L1: an omitted plugin list keeps the type default, a given one replaces it
// and must be a valid, non-empty, duplicate-free set on a type that loads
// marketplace plugins.
func TestNormalizePlugins(t *testing.T) {
	tooMany := make([]string, MaxSessionPlugins+1)
	for i := range tooMany {
		tooMany[i] = "p" + strings.Repeat("x", i)
	}
	cases := []struct {
		name     string
		workload WorkloadType
		in       []string
		want     []string
		wantErr  bool
	}{
		{name: "omitted on claude-code is the default", workload: WorkloadTypeClaudeCode, in: nil, want: nil},
		{name: "omitted on shell is fine", workload: WorkloadTypeShell, in: nil, want: nil},
		{name: "claude-code list kept in order", workload: WorkloadTypeClaudeCode, in: []string{"development", "session-platform"}, want: []string{"development", "session-platform"}},
		{name: "codex list", workload: WorkloadTypeCodex, in: []string{"codex-session", "learning"}, want: []string{"codex-session", "learning"}},
		{name: "minimax list", workload: WorkloadTypeMiniMaxCode, in: []string{"mcp-servers"}, want: []string{"mcp-servers"}},
		{name: "dots underscores hyphens", workload: WorkloadTypeClaudeCode, in: []string{"a.b_c-d"}, want: []string{"a.b_c-d"}},
		{name: "shell refuses a list", workload: WorkloadTypeShell, in: []string{"development"}, wantErr: true},
		{name: "approval-gated refuses a list", workload: WorkloadTypeApprovalGated, in: []string{"development"}, wantErr: true},
		{name: "empty list", workload: WorkloadTypeClaudeCode, in: []string{}, wantErr: true},
		{name: "duplicate", workload: WorkloadTypeClaudeCode, in: []string{"dev", "dev"}, wantErr: true},
		{name: "blank name", workload: WorkloadTypeClaudeCode, in: []string{""}, wantErr: true},
		{name: "padded name", workload: WorkloadTypeClaudeCode, in: []string{" dev"}, wantErr: true},
		{name: "marketplace suffix", workload: WorkloadTypeClaudeCode, in: []string{"dev@dlddu-plugins"}, wantErr: true},
		{name: "path", workload: WorkloadTypeCodex, in: []string{"../etc"}, wantErr: true},
		{name: "comma would split the env list", workload: WorkloadTypeCodex, in: []string{"a,b"}, wantErr: true},
		{name: "leading dot", workload: WorkloadTypeCodex, in: []string{".hidden"}, wantErr: true},
		{name: "too long", workload: WorkloadTypeCodex, in: []string{"a" + strings.Repeat("b", 64)}, wantErr: true},
		{name: "too many", workload: WorkloadTypeCodex, in: tooMany, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePlugins(tc.workload, tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("err = %v, want ErrInvalidInput", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestNormalizePluginsReturnsCopy(t *testing.T) {
	in := []string{"development"}
	got, err := NormalizePlugins(WorkloadTypeClaudeCode, in)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = "mutated"
	if in[0] != "development" {
		t.Fatal("NormalizePlugins aliased the caller's slice")
	}
}
