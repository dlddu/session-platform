package main

import (
	"errors"
	"io"
)

// agentCLI is everything that differs between the one-shot agent CLIs that run
// on claudeWorkload. The execution model around it — bounded serial queue,
// per-invocation process group, output bounds and redaction, resume flag,
// filesystem archive — is shared and stays in claude.go (AC-E2~E5). A CLI only
// decides how it is invoked, how its stdout becomes user-facing text, what the
// platform installs into its HOME, and what a valid archived state looks like.
type agentCLI interface {
	// workloadName is what the agent reports as its workload on /healthz.
	workloadName() string
	// argv builds one invocation. resume follows AC-E4: true only after the
	// first successful run, so a failed first run never claims a conversation.
	argv(binary, model, prompt string, resume bool) []string
	// newProjector wraps the run's stdout. Closing it flushes any partial line.
	newProjector(dst io.Writer) io.WriteCloser
	// ensureHome installs or normalises the platform-managed parts of the
	// session HOME, on boot and right after a restore.
	ensureHome(homeDir string, tools toolSurface) error
	// validateState checks an archived or live state tree before it is
	// archived or installed (AC-E5's integrity check).
	validateState(root string) error
}

// claudeCLI is what claude-code runs: claude-runner, which drives the pinned
// Claude Code CLI through the Claude Agent SDK (data-plane/claude-runner). It
// is the default when a config names no CLI.
type claudeCLI struct {
	// effort is the session's effort (AC-E8); empty leaves the CLI default.
	effort string
	// platform is the K3s MCP proxy sidecar's offer (AC-E6); nil runs with
	// neither the K3s MCP nor plugins.
	platform *claudePlatform
	// agentURL is this agent on loopback, where the runner relays an
	// AskUserQuestion to a person and puts a tool call to the session owner
	// (AC-E2); empty denies the question and leaves the policy to decide the
	// call on its own.
	agentURL string
}

func (claudeCLI) workloadName() string { return workloadClaudeCode }

func (c claudeCLI) argv(binary, model, prompt string, resume bool) []string {
	return claudeRunnerArgv(binary, model, c.effort, c.platform.mcpURL(), c.agentURL, "", prompt, resume)
}

var errToolApprovalsUnsupported = errors.New("this workload cannot put its tool calls to the session owner")

var _ toolApprovalRelay = claudeCLI{}

// argvWithToolApprovals builds the invocation for a write that asked for the
// owner's decision. The approvals go to this agent on loopback, the same place
// a question goes; the permission mode stays the runner's own auto.
func (c claudeCLI) argvWithToolApprovals(binary, model, prompt string, resume bool) []string {
	return claudeRunnerArgv(binary, model, c.effort, c.platform.mcpURL(), c.agentURL, c.agentURL, prompt, resume)
}

var _ questionAsker = claudeCLI{}

// asksQuestions is true when the runner has somewhere to relay a question.
func (c claudeCLI) asksQuestions() bool { return c.agentURL != "" }

// relaysToolApprovals is true when the runner has somewhere to put a tool call
// to the session owner — the same loopback agent that takes its questions.
func (c claudeCLI) relaysToolApprovals() bool { return c.agentURL != "" }

func (claudeCLI) newProjector(dst io.Writer) io.WriteCloser {
	return newClaudeStreamProjector(dst)
}

// ensureHome installs what the sidecar seeded, once per pod, and enables the
// selected plugins it could install.
func (c claudeCLI) ensureHome(homeDir string, tools toolSurface) error {
	if c.platform != nil {
		c.platform.prepare(tools.Plugins)
		tools.Plugins = c.platform.enabled(tools.Plugins)
	}
	return ensureClaudeManagedSettings(homeDir, tools)
}

func (claudeCLI) validateState(root string) error {
	return validateRestoredClaudeState(root)
}
