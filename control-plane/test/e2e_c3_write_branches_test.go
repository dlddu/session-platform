//go:build e2e

// 검증 AC: AC-C3
//
// Write follows the same uniform rule as read — bring the session `active`
// first, then apply. A snapshot write is NOT rejected: it restores and then
// applies. The response's `path` names the branch:
//   - active   -> "active"                     (asserted below)
//   - snapshot -> "snapshot->restore->write"   (asserted below)
//   - idle     -> "idle->active->write"        (not asserted: `idle` is not
//     reachable yet. Registered as a gap in docs/test/e2e.md.)
//
// What write DOES to the workload (stdin injection, non-blocking return) is
// AC-D2's file.
package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func writeAt(t *testing.T, id, payload string) writeResp {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/write", map[string]string{"payload": payload})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status=%d body=%s", resp.StatusCode, body)
	}
	var w writeResp
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode write: %v body=%s", err, body)
	}
	return w
}

// active branch: applied in place.
func TestWriteBranches_ActiveAppliedInPlace(t *testing.T) {
	s := createSession(t, uniqueName(t))

	w := writeAt(t, s.ID, "true\n")
	if w.Path != "active" {
		t.Fatalf("write path=%q want active (AC-C3)", w.Path)
	}
	if w.Session.ID != s.ID {
		t.Fatalf("write session id=%q want %q", w.Session.ID, s.ID)
	}
	if w.Session.State != "active" {
		t.Fatalf("state after write = %q, want active (AC-C3)", w.Session.State)
	}
}

// snapshot branch: restore first, then apply — never a rejection.
func TestWriteBranches_SnapshotRestoresThenWrites(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "echo c3-pre-$((40+1))\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "c3-pre-41")
	})

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the snapshot write branch is not exercisable here")
	}

	w := writeAt(t, s.ID, "true\n")
	if w.Path != "snapshot->restore->write" {
		t.Fatalf("write path on a snapshot session = %q, want snapshot->restore->write (AC-C3)", w.Path)
	}
	if w.Session.State != "active" {
		t.Fatalf("state after writing a snapshot session = %q, want active (AC-C3)", w.Session.State)
	}
	if w.Session.Pod == "" {
		t.Fatal("restored session has no pod after the write branch (AC-C3)")
	}
}
