//go:build e2e

// 검증 AC: AC-D2
//
// docs/prd/shell-workload.md, docs/test/shell-workload.md scenario 2.
//
// The $((…)) marker only exists in the output once bash expanded and ran the
// line — the PTY-echoed input alone cannot contain the computed value, so it
// distinguishes "injected into stdin" from "echoed back".
package e2e_test

import (
	"strings"
	"testing"
	"time"
)

func TestShellWrite_InjectsIntoStdinWithoutWaiting(t *testing.T) {
	s := createSession(t, uniqueName(t))

	start := time.Now()
	writeShell(t, s.ID, "sleep 3; echo d2-marker-$((40+2))\n")
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("write blocked for %v on a 3s command — write must not wait for completion (AC-D2)", took)
	}

	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d2-marker-42")
	})
}

func TestShellWrite_SuccessiveWritesReachTheSameShell(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "D2VAR=$((40+3))\n")
	writeShell(t, s.ID, "echo d2-var:$D2VAR\n")

	// The second command can only print 43 if the first landed in the SAME
	// shell's stdin before it (AC-D2).
	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d2-var:43")
	})
}
