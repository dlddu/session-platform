package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

const claudeDetachedHelperEnv = "SESSION_PLATFORM_CLAUDE_DETACHED_HELPER"

func TestClaudeDetachedProcessHelper(t *testing.T) {
	switch os.Getenv(claudeDetachedHelperEnv) {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestClaudeDetachedProcessHelper$")
		cmd.Env = append(os.Environ(), claudeDetachedHelperEnv+"=child")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "child":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func TestExecCommandRunnerKillsAndReapsDetachedDescendants(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := (execCommandRunner{}).Run(ctx, []string{
		os.Args[0], "-test.run=^TestClaudeDetachedProcessHelper$",
	}, runnerOptions{
		Dir:    t.TempDir(),
		Env:    append(os.Environ(), claudeDetachedHelperEnv+"=parent"),
		Stdout: &output,
		Stderr: &output,
	})
	if err != nil {
		t.Fatalf("runner failed to clean detached descendant: %v (output %q)", err, output.String())
	}
	children, err := directClaudeChildren(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("detached descendants remain after runner: %v", children)
	}
}
