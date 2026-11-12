package session

import (
	"errors"
	"testing"
)

// AC-E2: only claude-code takes a permission mode, and only auto or acceptEdits.
func TestNormalizePermissionMode(t *testing.T) {
	for _, mode := range ClaudeCodePermissionModes {
		if got, err := NormalizePermissionMode(WorkloadTypeClaudeCode, mode); err != nil || got != mode {
			t.Fatalf("claude-code %q = %q, %v; want it accepted", mode, got, err)
		}
	}
	for _, w := range []WorkloadType{WorkloadTypeShell, WorkloadTypeClaudeCode, WorkloadTypeCodexTask, WorkloadTypeMiniMaxCode} {
		if got, err := NormalizePermissionMode(w, ""); err != nil || got != "" {
			t.Fatalf("%s empty = %q, %v; want the default", w, got, err)
		}
	}
	for _, tc := range []struct {
		workload WorkloadType
		mode     string
	}{
		{WorkloadTypeClaudeCode, "default"},
		{WorkloadTypeClaudeCode, "bypassPermissions"},
		{WorkloadTypeClaudeCode, "plan"},
		{WorkloadTypeClaudeCode, "dontAsk"},
		{WorkloadTypeClaudeCode, "Auto"},
		{WorkloadTypeShell, "auto"},
		{WorkloadTypeCodexTask, "acceptEdits"},
		{WorkloadTypeMiniMaxCode, "auto"},
	} {
		if _, err := NormalizePermissionMode(tc.workload, tc.mode); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s %q err = %v, want ErrInvalidInput", tc.workload, tc.mode, err)
		}
	}
}
