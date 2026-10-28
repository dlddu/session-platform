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

// ArchivesThroughAgent reports whether the service snapshots this type through
// the agent's generation-fenced filesystem archive and bounds its prompts before
// activation. claude-code and minimax-code share that path (AC-E5, AC-G4).
// approval-gated is left out on purpose: its archive wiring has not landed in
// the service, and widening this check would change what it does today.
func (w WorkloadType) ArchivesThroughAgent() bool {
	return w == WorkloadTypeClaudeCode || w == WorkloadTypeMiniMaxCode
}
