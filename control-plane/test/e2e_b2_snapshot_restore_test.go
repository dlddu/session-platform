//go:build e2e

// 검증 AC: AC-B2
//
// docs/prd/lifecycle.md, docs/test/lifecycle.md scenario 2; the CRIU round trip
// the restore rides on is docs/criu-verification.md.
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
		t.Skip("SUT predates the product snapshot endpoint — the restore path is not exercisable here; see docs/criu-verification.md")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot", frozen.State)
	}

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

	got := getSession(t, s.ID)
	if got.State != "active" || got.Pod != restored.Pod {
		t.Fatalf("session after restore = %+v, want state=active pod=%q (AC-B2)", got, restored.Pod)
	}
}
