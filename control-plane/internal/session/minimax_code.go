package session

// WorkloadTypeMiniMaxCode selects the MiniMax Code CLI (`mcode`) workload
// (docs/prd/minimax-code-workload.md, AC-G1~G6). It runs on the claude-code
// execution model (AC-E2~E5) behind approval-gated's network boundary and
// helper pod (AC-G6).
const WorkloadTypeMiniMaxCode WorkloadType = "minimax-code"

// HasApprovalGate reports whether a session of this type runs behind a helper
// pod that fronts the approval gate, with its workload egress confined to that
// pod (AC-F2~F4, AC-G6). Every such type also holds its idle count while a
// human decision is pending (AC-F3).
func (w WorkloadType) HasApprovalGate() bool {
	return w == WorkloadTypeApprovalGated || w == WorkloadTypeMiniMaxCode
}

// QueuesPrompts reports whether a session of this type takes prompts that its
// agent queues for a CLI run, rather than raw shell stdin. The service bounds
// those prompts before activation so an oversized one never restores a
// snapshot only to be rejected by the agent (AC-E2, AC-F1, AC-G1).
func (w WorkloadType) QueuesPrompts() bool {
	return w == WorkloadTypeClaudeCode || w == WorkloadTypeApprovalGated || w == WorkloadTypeMiniMaxCode || w.IsCodex()
}
