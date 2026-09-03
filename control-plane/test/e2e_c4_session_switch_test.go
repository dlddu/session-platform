//go:build e2e

// 검증 AC: AC-C4
//
// Switching an already-active session is a no-op, and moving back and forth
// across several sessions leaves each one's identity and state intact — the
// switch never breaks isolation.
//
// Restoring a `snapshot` target on switch is the AC-B2 transition and lives in
// that file; this one owns the switching semantics themselves.
package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func switchSession(t *testing.T, id string) session {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/switch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch status=%d body=%s", resp.StatusCode, body)
	}
	var s session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decode switch: %v body=%s", err, body)
	}
	return s
}

// Switching an already-active session is a no-op: same session, still active.
func TestSessionSwitch_ActiveTargetIsANoop(t *testing.T) {
	s := createSession(t, uniqueName(t))

	got := switchSession(t, s.ID)
	if got.ID != s.ID {
		t.Fatalf("switch id=%q want %q", got.ID, s.ID)
	}
	if got.State != "active" {
		t.Fatalf("switch state=%q want active (no-op, AC-C4)", got.State)
	}
	if got.Pod != s.Pod {
		t.Fatalf("switch moved the session to pod %q (was %q); a no-op must not reprovision (AC-C4)", got.Pod, s.Pod)
	}
}

// Moving freely across several sessions activates the right one every time and
// preserves the others — switching never crosses session boundaries.
func TestSessionSwitch_MovesFreelyAcrossSessions(t *testing.T) {
	const n = 3
	sessions := make([]session, 0, n)
	for i := 0; i < n; i++ {
		sessions = append(sessions, createSession(t, fmt.Sprintf("%s-%d", uniqueName(t), i)))
	}

	// Forward, then backward — each switch targets exactly the asked-for session.
	order := []int{0, 1, 2, 1, 0, 2}
	for _, i := range order {
		want := sessions[i]
		got := switchSession(t, want.ID)
		if got.ID != want.ID {
			t.Fatalf("switch to %s returned session %s (AC-C4)", want.ID, got.ID)
		}
		if got.State != "active" {
			t.Fatalf("switched-to session %s state=%q want active (AC-C4)", want.ID, got.State)
		}
		if got.Pod != want.Pod {
			t.Fatalf("session %s moved from pod %q to %q across switches; isolation broken (AC-C4)", want.ID, want.Pod, got.Pod)
		}
	}

	// Every session survived the traversal untouched.
	for _, want := range sessions {
		got := getSession(t, want.ID)
		if got.State != "active" || got.Pod != want.Pod || got.Name != want.Name {
			t.Fatalf("session %s after switching around = %+v, want %+v (AC-C4)", want.ID, got, want)
		}
	}
}
