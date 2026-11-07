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
	// Stored records, tenant grants and clients still carry it, so it is read as
	// codex-chat everywhere and never written (AC-J1).
	legacyWorkloadTypeCodex WorkloadType = "codex"
)

// IsCodex reports whether a session of this type runs the Codex SDK runner and
// its loopback K3s MCP sidecar, which is what the plugin refresh needs (AC-J7).
func (w WorkloadType) IsCodex() bool {
	return w == WorkloadTypeCodexChat || w == WorkloadTypeCodexTask
}
