//go:build e2e

// 검증 AC: AC-C2
//
// Read dispatches on the session's state and always ends with the session
// `active`. The response's `path` field names the branch taken:
//   - active            -> "active"                    (asserted below)
//   - snapshot          -> "snapshot->restore->read"   (asserted below)
//   - idle              -> "idle->active->read"        (not asserted: no way to
//     reach `idle` yet. Registered as a gap in docs/test/e2e.md.)
//
// What read RETURNS (shell scrollback, offset cursor semantics) is AC-D3's file.
package e2e_test

import (
	"strings"
	"testing"
	"time"
)

// active branch: served in place, and the cursor comes back with the payload.
func TestReadBranches_ActiveServedInPlace(t *testing.T) {
	s := createSession(t, uniqueName(t))

	// The freshly started bash eventually prints its prompt; output timing is
	// non-deterministic, so wait for the shell to speak rather than asserting on
	// the first read.
	deadline := time.Now().Add(30 * time.Second)
	var r readResp
	for {
		r = readShellAt(t, s.ID, 0)
		if r.Path != "active" {
			t.Fatalf("read path=%q want active (AC-C2)", r.Path)
		}
		if r.Session.ID != s.ID {
			t.Fatalf("read session id=%q want %q", r.Session.ID, s.ID)
		}
		if r.Session.State != "active" {
			t.Fatalf("state after read = %q, want active (AC-C2)", r.Session.State)
		}
		if r.Payload != "" {
			break // the shell has spoken (its prompt at minimum)
		}
		if time.Now().After(deadline) {
			t.Fatal("shell never produced output (expected at least the bash prompt)")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if r.NextOffset <= 0 {
		t.Fatalf("nextOffset=%d want > 0 alongside a non-empty payload", r.NextOffset)
	}
}

// snapshot branch: read restores first, then reads — it never rejects.
func TestReadBranches_SnapshotRestoresThenReads(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "echo c2-pre-$((40+1))\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "c2-pre-41")
	})

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the snapshot read branch is not exercisable here")
	}

	r := readShellAt(t, s.ID, 0)
	if r.Path != "snapshot->restore->read" {
		t.Fatalf("read path on a snapshot session = %q, want snapshot->restore->read (AC-C2)", r.Path)
	}
	if r.Session.State != "active" {
		t.Fatalf("state after reading a snapshot session = %q, want active (AC-C2)", r.Session.State)
	}
	if r.Session.Pod == "" {
		t.Fatal("restored session has no pod after the read branch (AC-C2)")
	}
}
