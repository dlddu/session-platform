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
	// TakesPlugins mirrors NormalizePlugins: true exactly when a create request
	// may carry plugins for this type (AC-L1).
	TakesPlugins bool
	Input        InputMode
	// ReasoningEfforts lists the efforts a create request may name for this
	// type, in display order (AC-E8). Empty means the type takes none.
	ReasoningEfforts []string
}

// workloadTypeCatalog lists every valid type once, in display order. Adding a
// workload type means adding it here; Valid and NormalizeModel are checked
// against this list by tests so the three cannot drift apart.
var workloadTypeCatalog = []WorkloadTypeInfo{
	{
		Type:         WorkloadTypeShell,
		Label:        "Shell",
		Description:  "Interactive bash. You type the commands.",
		TakesModel:   false,
		TakesPlugins: false,
		Input:        InputModeTerminal,
	},
	{
		Type:         WorkloadTypeClaudeCode,
		Label:        "Claude Code",
		Description:  "Agent CLI. You send prompts, it does the work.",
		TakesModel:   true,
		TakesPlugins: true,
		Input:        InputModePrompt,
		// Claude Code's --effort levels (2.1.220).
		ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"},
	},
	{
		Type:         WorkloadTypeMiniMaxCode,
		Label:        "MiniMax Code",
		Description:  "MiniMax Code CLI behind the approval gate. K3s MCP tools use their own gate.",
		TakesModel:   true,
		TakesPlugins: true,
		Input:        InputModePrompt,
	},
	{
		Type:         WorkloadTypeCodexChat,
		Label:        "Codex Chat",
		Description:  "Codex SDK agent on your ChatGPT plan. Conversation, plugin skills and K3s MCP only.",
		TakesModel:   true,
		TakesPlugins: true,
		Input:        InputModePrompt,
	},
	{
		Type:         WorkloadTypeCodexTask,
		Label:        "Codex Task",
		Description:  "Codex coding agent on a pooled ChatGPT login. Shell, files and network, like Claude Code.",
		TakesModel:   true,
		TakesPlugins: true,
		Input:        InputModePrompt,
		// The Codex SDK's modelReasoningEffort values the platform offers.
		ReasoningEfforts: []string{"minimal", "low", "medium", "high", "xhigh"},
	},
	{
		Type:         WorkloadTypeApprovalGated,
		Label:        "Approval-gated",
		Description:  "Same agent, but every outbound call waits for your approval.",
		TakesModel:   true,
		TakesPlugins: false,
		Input:        InputModePrompt,
	},
}

// WorkloadTypes returns the catalog in display order. The slice is a copy.
func WorkloadTypes() []WorkloadTypeInfo {
	out := append([]WorkloadTypeInfo{}, workloadTypeCatalog...)
	for i := range out {
		out[i].ReasoningEfforts = append([]string{}, out[i].ReasoningEfforts...)
	}
	return out
}
