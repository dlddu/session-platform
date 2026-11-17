package session

const (
	// WorkloadTypeCodexChat selects the Codex SDK chat workload
	// (docs/prd/codex-workload.md, AC-J1~J7): a long-lived single session that
	// signs in to ChatGPT itself and whose tool surface is narrowed to
	// conversation, platform plugin skills and the K3s MCP.
	WorkloadTypeCodexChat WorkloadType = "codex-chat"
	// WorkloadTypeCodexTask selects the Codex SDK task workload
	// (docs/prd/codex-task-workload.md, AC-M1~M4): claude-code's tool surface,
	// with the ChatGPT login held by an auth pool slot rather than the session.
	WorkloadTypeCodexTask WorkloadType = "codex-task"

	// legacyWorkloadTypeCodex is codex-chat's name before the 2026-11-07 split.
	// Only session records written before it still carry the name; they are
	// read as codex-chat and the next write stores the new name (AC-J1).
	legacyWorkloadTypeCodex WorkloadType = "codex"
)

// IsCodex reports whether a session of this type runs the Codex SDK runner and
// its loopback K3s MCP sidecar, which is what the plugin refresh needs (AC-J7).
func (w WorkloadType) IsCodex() bool {
	return w == WorkloadTypeCodexChat || w == WorkloadTypeCodexTask
}

// Steered is what one mid-turn message did (AC-J9). Applied means codex put
// it into the turn that was running; anything else is a rejection with the
// reason the runner reported, so a caller can tell "the turn took it" from
// "it never reached the turn" and queue it behind the turn instead.
type Steered struct {
	Status string `json:"status"`
	TurnID string `json:"turnId,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SteerStatusApplied is the one status that means the running turn has it.
const SteerStatusApplied = "applied"

// Applied reports whether the running turn took the message.
func (s Steered) Applied() bool { return s.Status == SteerStatusApplied }

// SteerableWorkload reports whether this type's turns take mid-turn messages
// (AC-J9) — the Codex workloads, whose runner speaks the app server's
// turn/steer. claude-code's mid-turn surface is its own (AC-E2 questions and
// tool approvals), not free text.
func SteerableWorkload(t WorkloadType) bool {
	switch t {
	case WorkloadTypeCodexChat, WorkloadTypeCodexTask, legacyWorkloadTypeCodex:
		return true
	default:
		return false
	}
}
