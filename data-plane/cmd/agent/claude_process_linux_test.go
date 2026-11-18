package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
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

// The worker closes the steering pipe only after Run returns, so Run must
// return on the runner's exit while the pipe is still open.
func TestExecCommandRunnerReturnsWhileSteeringPipeIsOpen(t *testing.T) {
	var desk steerDesk
	stdin, closeSteer, err := desk.openPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer closeSteer()
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (execCommandRunner{}).Run(ctx, []string{"/bin/sh", "-c", "head -n 1"}, runnerOptions{
			Dir:    t.TempDir(),
			Stdin:  stdin,
			Stdout: &output,
			Stderr: &output,
		})
	}()
	steerCtx, stopSteer := context.WithCancel(context.Background())
	defer stopSteer()
	go func() { _, _ = desk.steer(steerCtx, "mid-flight") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner failed: %v (output %q)", err, output.String())
		}
	case <-time.After(claudeProcessGroupDrainTimeout + 5*time.Second):
		t.Fatal("Run did not return after the runner exited while the steering pipe was open")
	}
	if !strings.Contains(output.String(), `"text":"mid-flight"`) {
		t.Fatalf("runner stdin did not carry the steer line: %q", output.String())
	}
}
