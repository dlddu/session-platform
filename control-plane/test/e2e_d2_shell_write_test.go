//go:build e2e

// 검증 AC: AC-D2
//
// write = shell stdin (docs/prd/shell-workload.md, docs/test/shell-workload.md
// scenario 2): the payload is injected into the session shell's stdin and bash
// actually runs it, and the call returns without waiting for the command to
// finish.
//
// The $((…)) marker only exists in the output once bash expanded and ran the
// line — the PTY-echoed input alone cannot contain the computed value, so it
// distinguishes "injected into stdin" from "echoed back". Recovering that output
// (offset cursor semantics) is AC-D3's file; the state dispatch around write is
// AC-C3's.
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

	// The command really ran in the session's shell.
	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d2-marker-42")
	})
}

// Successive writes are injected in order — stdin is a stream, not a one-shot.
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
