//go:build e2e

// 검증 AC: AC-A2
//
// One session ⇒ one dedicated Pod, 1:1 (docs/prd/architecture.md, docs/test/architecture.md
// scenarios 1·2). Asserted at both levels: the wire contract (create returns an
// active session carrying its own pod name) and the cluster ground truth (a real
// Pod object labelled `session-id` back to that session, never shared).
package e2e_test

import (
	"fmt"
	"testing"
)

// Creating a session yields an active session bound to a pod of its own, and
// that pod is a real Pod object labelled 1:1 to the session.
func TestDedicatedPod_CreatedSessionOwnsItsPod(t *testing.T) {
	s := createSession(t, uniqueName(t))
	if s.ID == "" {
		t.Fatal("expected a non-empty session id")
	}
	if s.State != "active" {
		t.Fatalf("state=%q want active", s.State)
	}
	if s.Pod == "" {
		t.Fatal("expected a dedicated pod name (AC-A2)")
	}

	cs, _, ok := kubeClient(t)
	if !ok {
		return // wire contract asserted; the cluster half needs the deployed cluster
	}
	pod := getPodEventually(t, cs, sessionNamespace(), s.Pod)
	if got := pod.Labels["session-id"]; got != s.ID {
		t.Fatalf("pod %s session-id label=%q want %q (AC-A2 1:1)", s.Pod, got, s.ID)
	}
}

// N sessions map to N unique pods — no pod is ever shared between sessions.
func TestDedicatedPod_SessionsMapToUniquePods(t *testing.T) {
	cs, _, hasCluster := kubeClient(t)
	ns := sessionNamespace()

	const n = 3
	seen := map[string]string{} // pod name -> session id
	for i := 0; i < n; i++ {
		s := createSession(t, fmt.Sprintf("%s-%d", uniqueName(t), i))
		if s.Pod == "" {
			t.Fatalf("session %s has no pod", s.ID)
		}
		if prev, dup := seen[s.Pod]; dup {
			t.Fatalf("pod %q shared by sessions %s and %s (AC-A2 violated)", s.Pod, prev, s.ID)
		}
		seen[s.Pod] = s.ID

		if hasCluster {
			p := getPodEventually(t, cs, ns, s.Pod)
			if got := p.Labels["session-id"]; got != s.ID {
				t.Fatalf("pod %s session-id label=%q want %q", s.Pod, got, s.ID)
			}
		}
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique pods, got %d", n, len(seen))
	}
}
