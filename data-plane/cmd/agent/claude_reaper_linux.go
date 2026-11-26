package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// The agent binary doubles as this invocation-scoped reaper, selected by its
// first argument (runAgentSubcommand) like the other helper modes.
//
// Why a helper process runs the CLI at all: a manager pod has two lanes, the
// parent's and the monitor's, and both run their invocations concurrently on
// the one execCommandRunner of the same agent process. Cleaning up from the
// agent cannot tell one invocation's detached tools from the other's — every
// one of them is a child of that single agent — so the lane that finished
// first killed the running lane's CLI and took its exit status. Giving each
// invocation a process that is its own subreaper makes that distinction a
// property of the process tree: a detached tool reparents onto the reaper of
// the invocation that started it, and onto nothing else.
const (
	claudeReaperCommand = "claude-reaper"
	// claudeReaperArgvSeparator keeps a CLI argument that looks like a
	// subcommand from being read as one.
	claudeReaperArgvSeparator = "--"

	// Exit codes the reaper reports for its own failures. They are outside the
	// range the Claude CLI uses and each one is reported on stderr as well, so
	// a turn that fails here says why rather than looking like a CLI exit.
	claudeReaperUsageExit   = 2
	claudeReaperSetupExit   = 125
	claudeReaperCleanupExit = 124
	claudeReaperExecExit    = 127
)

// claudeReaperArgv is the command the agent runs in place of the CLI.
func claudeReaperArgv(self string, argv []string) []string {
	reaper := make([]string, 0, len(argv)+3)
	reaper = append(reaper, self, claudeReaperCommand, claudeReaperArgvSeparator)
	return append(reaper, argv...)
}

// runClaudeReaper runs one invocation's CLI as its own child and hands that
// child's exit status back to the agent unchanged, then kills and waits on
// every descendant this invocation left behind.
//
// Standard input, output and error are the agent's own file descriptors,
// passed straight through: the CLI writes to the same pipes it did before, so
// the turn's bytes and their interleaving are the CLI's, not this helper's.
func runClaudeReaper(args []string, stderr io.Writer) int {
	argv, err := claudeReaperTarget(args)
	if err != nil {
		fmt.Fprintln(stderr, claudeReaperCommand+":", err)
		return claudeReaperUsageExit
	}
	// This invocation's detached tools reparent here rather than onto the
	// agent, which is the whole point of running under this helper.
	if err := prepareClaudeProcessIsolation(); err != nil {
		fmt.Fprintln(stderr, claudeReaperCommand+":", err)
		return claudeReaperSetupExit
	}
	// The agent's cancel and timeout arrive as SIGTERM. It has to reach the
	// cleanup below instead of ending this process: a reaper that dies on the
	// signal leaves its invocation's tools to reparent onto the agent, which
	// is the state this helper exists to prevent.
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Its own process group, so the cancel path can kill the CLI and every
	// tool it started in one signal without killing this helper with them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, claudeReaperCommand+":", err)
		return claudeReaperExecExit
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-waited:
	case <-stopping:
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) {
			fmt.Fprintln(stderr, claudeReaperCommand+": kill the claude process group:", err)
		}
		runErr = <-waited
	}
	// The CLI is reaped; whatever is still a child here is a detached tool of
	// this invocation. Killing the group again first catches the ones that are
	// still in it, which the loop would otherwise have to walk to one by one.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		fmt.Fprintln(stderr, claudeReaperCommand+": clean the claude process group:", err)
	}
	cleanupErr := killAndReapOwnDescendants()
	if cleanupErr != nil {
		fmt.Fprintln(stderr, claudeReaperCommand+":", cleanupErr)
	}
	return claudeReaperExit(runErr, cleanupErr, stopping)
}

// claudeReaperExit reports the CLI's own status. A CLI killed by a signal is
// reported by re-raising that signal here, so the agent's Wait sees the
// signalled status it would have seen without this helper in between —
// including the SIGKILL of the cancel and timeout paths, which is what makes
// those turns read as canceled rather than as a plain non-zero exit.
func claudeReaperExit(runErr, cleanupErr error, stopping chan os.Signal) int {
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal.Stop(stopping)
			signal.Reset(status.Signal())
			_ = syscall.Kill(os.Getpid(), status.Signal())
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	if runErr != nil {
		return claudeReaperSetupExit
	}
	// A CLI that succeeded while its tools could not be cleaned up still has
	// to fail the turn: an invocation that leaked processes is not an ok turn,
	// and that was this cleanup's reported outcome before it moved here.
	if cleanupErr != nil {
		return claudeReaperCleanupExit
	}
	return 0
}

func claudeReaperTarget(args []string) ([]string, error) {
	if len(args) > 0 && args[0] == claudeReaperArgvSeparator {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, errors.New("usage: agent " + claudeReaperCommand + " -- <command> [args...]")
	}
	return args, nil
}
