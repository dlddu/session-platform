//go:build e2e

// 검증 AC: AC-B3
//
// Snapshot/restore loses no session data (docs/prd/lifecycle.md, docs/test/lifecycle.md
// scenario 3). The observable contract, per the "offset과 복원" design note in
// docs/prd/shell-workload.md: a read cursor issued BEFORE the freeze stays a
// valid offset into the restored buffer (it yields only post-restore output, not
// a full replay), and offset=0 still returns the whole pre-freeze + post-restore
// history in order.
//
// The in-memory shell state that survives (env, cwd, process tree) is AC-D4's
// file; that access restores at all is AC-B2's.
package e2e_test

import (
	"strings"
	"testing"
)

func TestRestoreIntegrity_HistoryAndCursorSurviveTheFreeze(t *testing.T) {
	s := createSession(t, uniqueName(t))

	// Pre-freeze output + the cursor that points just past it.
	writeShell(t, s.ID, "echo b3-pre-$((40+1))\n")
	before := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "b3-pre-41")
	})
	if before.NextOffset <= 0 {
		t.Fatalf("pre-freeze nextOffset=%d want > 0", before.NextOffset)
	}

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the CRIU round trip is not exercisable here; see docs/criu-verification.md")
	}

	// Writing restores the session and appends after the frozen history.
	writeShell(t, s.ID, "echo b3-post-$((40+2))\n")

	// The pre-freeze cursor is still valid: it yields the delta only, no replay.
	delta := eventuallyShellRead(t, s.ID, before.NextOffset, func(p string) bool {
		return strings.Contains(p, "b3-post-42")
	})
	if strings.Contains(delta.Payload, "b3-pre-41") {
		t.Fatalf("delta from the pre-freeze cursor replayed pre-freeze output %q; the cursor must stay valid across the freeze (AC-B3)", delta.Payload)
	}

	// offset=0 still replays everything across the freeze, in execution order.
	full := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "b3-pre-41") && strings.Contains(p, "b3-post-42")
	})
	pre, post := strings.Index(full.Payload, "b3-pre-41"), strings.Index(full.Payload, "b3-post-42")
	if pre == -1 || post == -1 || pre > post {
		t.Fatalf("history order broken across the freeze: pre=%d post=%d in %q (AC-B3)", pre, post, full.Payload)
	}
	if full.NextOffset < delta.NextOffset {
		t.Fatalf("full-read cursor %d regressed below the delta cursor %d after restore", full.NextOffset, delta.NextOffset)
	}
}
