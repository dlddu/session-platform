//go:build e2e

// 검증 AC: AC-B2
//
// Accessing a `snapshot` session restores it into a NEW pod and transitions it
// back to `active` (docs/prd/lifecycle.md, docs/test/lifecycle.md scenario 2).
// This file asserts the transition itself — that access restores rather than
// rejects, and that the restore lands on freshly provisioned compute. What
// survives the round trip is AC-B3's file (history/cursor integrity) and
// AC-D4's (the shell process tree).
//
// The whole stack runs over HTTP: the control plane asks the session pod's agent
// to CRIU-dump its shell tree, streams the archive to the checkpoint store,
// reclaims the pod, then on the next access provisions a restore-target pod and
// streams the archive back (docs/criu-verification.md). Against a SUT without
// CRIU or the snapshot trigger this skips, so the suite still runs anywhere.
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSnapshotRestore_AccessRestoresIntoANewPod(t *testing.T) {
	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("created session has no pod")
	}

	frozen, ok := snapshotSession(t, s.ID)
	if !ok {
		t.Skip("SUT has no snapshot trigger (E2E_TEST_ENDPOINTS off) — the restore path is not exercisable here; see docs/criu-verification.md")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot", frozen.State)
	}

	// Access = switch. AC-B2 names read/write/switch alike; switch is the one
	// this file owns (read/write dispatch is AC-C2/AC-C3).
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/switch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch on a snapshot session: status=%d body=%s", resp.StatusCode, body)
	}
	var restored session
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatalf("decode switch: %v body=%s", err, body)
	}

	if restored.State != "active" {
		t.Fatalf("state after accessing a snapshot session = %q, want active (AC-B2)", restored.State)
	}
	if restored.Pod == "" {
		t.Fatal("restored session has no pod — restore must provision one (AC-B2)")
	}
	if restored.Pod == s.Pod {
		t.Fatalf("restored onto the pre-freeze pod %q; the freeze reclaimed it, so restore must use a new pod (AC-B2)", s.Pod)
	}

	// Read back independently: the restore stuck, it was not just the response.
	got := getSession(t, s.ID)
	if got.State != "active" || got.Pod != restored.Pod {
		t.Fatalf("session after restore = %+v, want state=active pod=%q (AC-B2)", got, restored.Pod)
	}
}
