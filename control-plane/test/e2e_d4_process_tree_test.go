//go:build e2e

// 검증 시나리오: shell-workload.md#시나리오 4
//
// docs/prd/shell-workload.md — the markers below are the ones AC-D4's 검증 방법
// names: a shell freezes into an archive of its home directory and scrollback,
// and thaws as a fresh shell started in that home directory.
package e2e_test

import (
	"strings"
	"testing"
)

// shellHome is where the control plane points the shell agent's state root
// (SHELL_STATE_DIR=/session/state); the agent's home is its home/ child.
const shellHome = "/session/state/home"

func TestShellArchive_HomeFilesSurviveAndTheShellStartsFresh(t *testing.T) {
	s := createSession(t, uniqueName(t))

	// Every marker is built by the shell at run time, so the PTY's echo of the
	// typed input never contains it: only executed output can match.
	writeShell(t, s.ID, "printf 'd4file:%s\\n' frozen42 > ~/d4marker.txt\n")
	writeShell(t, s.ID, "export D4MARK=frozen42\n")
	writeShell(t, s.ID, "cd /tmp\n")
	writeShell(t, s.ID, "echo d4ready:$((40+2))\n")
	before := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d4ready:42")
	})
	cursorBefore := before.NextOffset

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the archive round trip is not exercisable here")
	}

	writeShell(t, s.ID, "cat ~/d4marker.txt\n")
	writeShell(t, s.ID, "echo \"d4env:[$D4MARK]\"\n")
	writeShell(t, s.ID, "echo \"d4cwd:$(pwd)\"\n")

	delta := eventuallyShellRead(t, s.ID, cursorBefore, func(p string) bool {
		return strings.Contains(p, "d4file:frozen42") &&
			(strings.Contains(p, "d4env:[]") || strings.Contains(p, "d4env:[frozen42]")) &&
			strings.Contains(p, "d4cwd:/")
	})
	if !strings.Contains(delta.Payload, "d4file:frozen42") {
		t.Fatalf("restored shell lost ~/d4marker.txt; payload=%q (AC-D4)", delta.Payload)
	}
	if !strings.Contains(delta.Payload, "d4env:[]") {
		t.Fatalf("restored shell still carries $D4MARK — it must be a fresh shell; payload=%q (AC-D4)", delta.Payload)
	}
	if !strings.Contains(delta.Payload, "d4cwd:"+shellHome) || strings.Contains(delta.Payload, "d4cwd:/tmp") {
		t.Fatalf("restored shell did not start fresh in %s; payload=%q (AC-D4)", shellHome, delta.Payload)
	}
	// Cursor continuity: the pre-freeze cursor still indexes the preloaded
	// scrollback, so its delta holds only post-restore output.
	if strings.Contains(delta.Payload, "d4ready:42") {
		t.Fatalf("delta from the pre-freeze cursor replayed pre-freeze output; payload=%q", delta.Payload)
	}

	full := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d4ready:42") && strings.Contains(p, "d4file:frozen42")
	})
	if pre, post := strings.Index(full.Payload, "d4ready:42"), strings.Index(full.Payload, "d4file:frozen42"); pre == -1 || post == -1 || pre > post {
		t.Fatalf("full history order broken across the freeze: pre=%d post=%d in %q", pre, post, full.Payload)
	}
}
