//go:build e2e

// 검증 AC: AC-D4
//
// The shell process tree is what CRIU checkpoints, so the in-memory state it
// carries survives the freeze (docs/prd/shell-workload.md). This is the concrete
// marker form of AC-B3's "no data loss": AC-D4's 검증 방법 names it exactly —
// `export MARKER=42` + `cd /tmp` before the freeze, `echo $MARKER; pwd` after
// the restore.
//
// AC-B3's file owns the scrollback/cursor half; AC-B2's owns the transition.
package e2e_test

import (
	"strings"
	"testing"
)

func TestProcessTree_EnvAndCwdSurviveTheFreeze(t *testing.T) {
	s := createSession(t, uniqueName(t))

	// Shell state that must survive. export/cd print nothing, so anchor on the
	// PTY's echo of the input line to know the shell has consumed them.
	writeShell(t, s.ID, "export D4MARK=frozen42\n")
	writeShell(t, s.ID, "cd /tmp\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "D4MARK=frozen42")
	})

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT has no snapshot trigger (E2E_TEST_ENDPOINTS off) — the CRIU round trip is not exercisable here; see docs/criu-verification.md")
	}

	// Restored shell resumes on top of the frozen context: the variable and the
	// working directory are exactly as they were. The markers below only appear
	// once bash actually expanded them — the PTY-echoed input carries `$D4MARK`
	// and `$(pwd)` literally, so an echo alone can never match.
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
