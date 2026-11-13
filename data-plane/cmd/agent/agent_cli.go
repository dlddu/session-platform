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
}

func (claudeCLI) workloadName() string { return workloadClaudeCode }

func (c claudeCLI) argv(binary, model, prompt string, resume bool) []string {
	return claudeRunnerArgv(binary, model, c.effort, "", c.platform.mcpURL(), prompt, resume)
}

// permissionModer is an agentCLI whose turns run in a permission mode the write
// that queued them may choose (AC-E2). Only claude-code is one; a write naming a
// mode to any other workload is refused.
type permissionModer interface {
	validPermissionMode(mode string) bool
	argvWithPermissionMode(binary, model, prompt string, resume bool, mode string) []string
}

var (
	errPermissionModeUnsupported = errors.New("this workload takes no permission mode")
	errPermissionModeUnknown     = errors.New("unknown permission mode; use auto or acceptEdits")
)

var _ permissionModer = claudeCLI{}

// claudePermissionModes are the modes a write may choose. The runner starts a
// turn in auto when the write chose none, and offers no mode that skips
// permission checks.
var claudePermissionModes = map[string]bool{"auto": true, "acceptEdits": true}

func (claudeCLI) validPermissionMode(mode string) bool {
	return claudePermissionModes[mode]
}

func (c claudeCLI) argvWithPermissionMode(binary, model, prompt string, resume bool, mode string) []string {
	return claudeRunnerArgv(binary, model, c.effort, mode, c.platform.mcpURL(), prompt, resume)
}

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
