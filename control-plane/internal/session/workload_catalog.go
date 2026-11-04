package session

// InputMode is how a client drives a session of a given workload type.
type InputMode string

const (
	// InputModeTerminal — keystrokes go to a PTY and the output is a terminal
	// screen (shell).
	InputModeTerminal InputMode = "terminal"
	// InputModePrompt — whole prompts are queued to a one-shot agent and the
	// output is the agent's projected text stream.
	InputModePrompt InputMode = "prompt"
)

// WorkloadTypeInfo is the public, non-sensitive description of a workload type
// that clients render a type picker from and validate create requests against.
// It carries no deployment state; which types a deployment offers and their
// model catalogs are layered on top by the API.
type WorkloadTypeInfo struct {
	Type        WorkloadType
	Label       string
	Description string
	// TakesModel mirrors NormalizeModel: true exactly when a create request may
	// carry a model for this type.
	TakesModel bool
	Input      InputMode
	// ApprovalGate mirrors HasApprovalGate: true exactly when sessions of this
	// type run behind the approval gate (docs/prd/approval-gate.md AC-F2~F6).
	// Clients render the gated workspace from it instead of from a type name,
	// so a new gated type needs no client change.
	ApprovalGate bool
}

// workloadTypeCatalog lists every valid type once, in display order. Adding a
// workload type means adding it here; Valid, NormalizeModel and
// HasApprovalGate are checked against this list by tests so they cannot drift
// apart.
var workloadTypeCatalog = []WorkloadTypeInfo{
	{
		Type:        WorkloadTypeShell,
		Label:       "Shell",
		Description: "Interactive bash. You type the commands.",
		TakesModel:  false,
		Input:       InputModeTerminal,
	},
	{
		Type:        WorkloadTypeClaudeCode,
		Label:       "Claude Code",
		Description: "Agent CLI. You send prompts, it does the work.",
		TakesModel:  true,
		Input:       InputModePrompt,
	},
	{
		Type:         WorkloadTypeMiniMaxCode,
		Label:        "MiniMax Code",
		Description:  "MiniMax Code CLI behind the approval gate. K3s MCP tools use their own gate.",
		TakesModel:   true,
		Input:        InputModePrompt,
		ApprovalGate: true,
	},
}

// WorkloadTypes returns the catalog in display order. The slice is a copy.
func WorkloadTypes() []WorkloadTypeInfo {
	return append([]WorkloadTypeInfo{}, workloadTypeCatalog...)
}
