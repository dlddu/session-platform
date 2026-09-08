//go:build e2e

// 검증 시나리오: shell-workload.md#시나리오 4
//
// docs/prd/shell-workload.md — the markers below are the ones AC-D4's 검증 방법
// names.
package e2e_test

import (
	"strings"
	"testing"
)

func TestProcessTree_EnvAndCwdSurviveTheFreeze(t *testing.T) {
	s := createSession(t, uniqueName(t))

	// export/cd print nothing, so anchor on the PTY's echo of the input line to
	// know the shell has consumed them.
	writeShell(t, s.ID, "export D4MARK=frozen42\n")
	writeShell(t, s.ID, "cd /tmp\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "D4MARK=frozen42")
	})

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the CRIU round trip is not exercisable here; see docs/criu-verification.md")
	}

	// The markers below only appear once bash actually expanded them — the
	// PTY-echoed input carries `$D4MARK` and `$(pwd)` literally, so an echo alone
	// can never match.
	writeShell(t, s.ID, "echo d4env:$D4MARK\n")
	writeShell(t, s.ID, "echo d4cwd:$(pwd)\n")

	got := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d4env:frozen42") && strings.Contains(p, "d4cwd:/tmp")
	})
	if !strings.Contains(got.Payload, "d4env:frozen42") {
		t.Fatalf("restored shell lost $D4MARK; payload=%q (AC-D4)", got.Payload)
	}
	if !strings.Contains(got.Payload, "d4cwd:/tmp") {
		t.Fatalf("restored shell lost its working directory; payload=%q (AC-D4)", got.Payload)
	}
}
