package session

import "slices"

// WriteOptions are what one write chooses for its own turn, beside the payload.
type WriteOptions struct {
	// PermissionMode is the claude-code turn's permission mode (AC-E2), or ""
	// for the runner's default, auto.
	PermissionMode string
}

// ClaudeCodePermissionModes are the permission modes a claude-code write may
// choose (AC-E2). "auto" — the default — lets the CLI's model classifier decide
// what the managed settings leave open; "acceptEdits" accepts file edits. No
// mode that skips permission checks is offered.
var ClaudeCodePermissionModes = []string{"auto", "acceptEdits"}

// NormalizePermissionMode validates a write's permission mode (AC-E2). Empty
// means the default and is valid for every type; a value is valid only for a
// claude-code session and only when it is in ClaudeCodePermissionModes. Callers
// that must reject an explicitly empty value (the write API) do so first.
func NormalizePermissionMode(workload WorkloadType, mode string) (string, error) {
	if mode == "" {
		return "", nil
	}
	if workload != WorkloadTypeClaudeCode || !slices.Contains(ClaudeCodePermissionModes, mode) {
		return "", ErrInvalidInput
	}
	return mode, nil
}
