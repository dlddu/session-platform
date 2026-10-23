package main

import "io"

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
	ensureHome(homeDir string) error
	// validateState checks an archived or live state tree before it is
	// archived or installed (AC-E5's integrity check).
	validateState(root string) error
}

// claudeCLI is the Claude Code CLI of the claude-code workload. It is the
// default when a config names no CLI.
type claudeCLI struct{}

func (claudeCLI) workloadName() string { return workloadClaudeCode }

func (claudeCLI) argv(binary, model, prompt string, resume bool) []string {
	return claudeArgv(binary, model, prompt, resume)
}

func (claudeCLI) newProjector(dst io.Writer) io.WriteCloser {
	return newClaudeStreamProjector(dst)
}

func (claudeCLI) ensureHome(homeDir string) error {
	return ensureClaudeManagedSettings(homeDir)
}

func (claudeCLI) validateState(root string) error {
	return validateRestoredClaudeState(root)
}
