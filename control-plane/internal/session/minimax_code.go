package session

// WorkloadTypeMiniMaxCode selects the MiniMax Code CLI (`mcode`) workload
// (docs/prd/minimax-code-workload.md, AC-G1~G4). It reuses the claude-code
// execution model as is — one-shot prompts on a serial queue, offset-cursored
// output, a filesystem archive for snapshots (AC-E2~E5) — and differs only in
// the CLI it runs, how that CLI's stream is projected, and which platform Secret
// its credential proxy holds (AC-G3).
const WorkloadTypeMiniMaxCode WorkloadType = "minimax-code"

// ArchivesThroughAgent reports whether the service snapshots this type through
// the agent's generation-fenced filesystem archive and bounds its prompts before
// activation. claude-code and minimax-code share that path (AC-E5, AC-G4).
func (w WorkloadType) ArchivesThroughAgent() bool {
	return w == WorkloadTypeClaudeCode || w == WorkloadTypeMiniMaxCode
}
