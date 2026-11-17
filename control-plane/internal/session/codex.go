package session

const (
	// WorkloadTypeCodexManager selects the Codex SDK manager workload
	// (docs/prd/codex-workload.md, AC-J1~J8): a long-lived single session that
	// signs in to ChatGPT itself and whose tool surface is narrowed to
	// conversation, platform plugin skills and the K3s MCP.
	WorkloadTypeCodexManager WorkloadType = "codex-manager"
	// WorkloadTypeCodexTask selects the Codex SDK task workload
	// (docs/prd/codex-task-workload.md, AC-M1~M4): claude-code's tool surface,
	// with the ChatGPT login held by an auth pool slot rather than the session.
	WorkloadTypeCodexTask WorkloadType = "codex-task"

	// legacyWorkloadTypeCodexChat is codex-manager's name between the
	// 2026-11-07 codex split and the 2026-11-17 rename. Unlike the older
	// "codex", it stays accepted on every input path — create requests, tenant
	// grants and stored records — because the callers and the deployed tenant
	// set still name it (AC-J1).
	legacyWorkloadTypeCodexChat WorkloadType = "codex-chat"
	// legacyWorkloadTypeCodex is the name before the 2026-11-07 split. Only
	// session records written before it still carry the name; they are read as
	// codex-manager and the next write stores the new name (AC-J1).
	legacyWorkloadTypeCodex WorkloadType = "codex"
)

// IsCodex reports whether a session of this type runs the Codex SDK runner and
// its loopback K3s MCP sidecar, which is what the plugin refresh needs (AC-J7).
func (w WorkloadType) IsCodex() bool {
	return w == WorkloadTypeCodexManager || w == WorkloadTypeCodexTask
}
