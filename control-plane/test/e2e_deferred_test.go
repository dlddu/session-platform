//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// This file seeds the *deferred* e2e scenarios — the ones blocked on real
// adapters or lifecycle triggers that the α stub SUT cannot exercise. Each test
// still blocked skips with its precondition and the AC it will verify; the ones
// whose precondition has landed (the real client-go PodOrchestrator) are filled
// and assert directly against the cluster API.
//
// The mapping from these placeholders to the documented scenarios/ACs lives in
// docs/test/e2e.md.

// sessionNamespace is where the deployed control plane provisions its data plane
// pods — the same namespace it runs in (default in the kind deploy/). Overridable
// for clusters that place the control plane elsewhere.
func sessionNamespace() string {
	if v := os.Getenv("E2E_SESSION_NAMESPACE"); v != "" {
		return v
	}
	return "default"
}

// kubeClient builds a client for the cluster the SUT runs in, from the ambient
// kubeconfig (kind writes one) or the in-cluster config. It reports ok=false
// when neither is available, so a run pointed at a non-cluster SUT skips rather
// than fails.
func kubeClient(t *testing.T) (kubernetes.Interface, bool) {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		if cfg, err = rest.InClusterConfig(); err != nil {
			return nil, false
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, false
	}
	return cs, true
}

// getPodEventually fetches a pod, tolerating brief API eventual-consistency.
// Create returns only after the pod is Ready, so it should already exist.
func getPodEventually(t *testing.T, cs kubernetes.Interface, ns, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			return pod
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s not found within timeout: %v", ns, name, lastErr)
			return nil
		}
		time.Sleep(time.Second)
	}
}

// AC-A1/A2 (real pod): the deployed control-plane creates a dedicated Pod object
// per session in the session namespace, 1:1 (architecture scenarios 1·2). Filled
// now that the real client-go PodOrchestrator backs the SUT — the API's returned
// pod name corresponds to an actual Pod object labelled to its session.
func TestDeferred_RealPodProvisioned(t *testing.T) {
	cs, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — real-pod assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	// Scenario 1: one session => one Pod object, labelled 1:1 to the session.
	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("API returned an empty pod name for a created session (AC-A1/A2)")
	}
	pod := getPodEventually(t, cs, ns, s.Pod)
	if got := pod.Labels["session-id"]; got != s.ID {
		t.Fatalf("pod %s session-id label=%q want %q (AC-A2 1:1)", s.Pod, got, s.ID)
	}

	// Scenario 2: N sessions => N unique Pods, each labelled to its own session.
	const n = 3
	seen := map[string]string{} // pod name -> session id
	for i := 0; i < n; i++ {
		si := createSession(t, fmt.Sprintf("%s-%d", uniqueName(t), i))
		if si.Pod == "" {
			t.Fatalf("session %s has no pod", si.ID)
		}
		if prev, dup := seen[si.Pod]; dup {
			t.Fatalf("pod %q shared by sessions %s and %s (AC-A2 violated)", si.Pod, prev, si.ID)
		}
		seen[si.Pod] = si.ID
		p := getPodEventually(t, cs, ns, si.Pod)
		if got := p.Labels["session-id"]; got != si.ID {
			t.Fatalf("pod %s session-id label=%q want %q", si.Pod, got, si.ID)
		}
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique pods, got %d", n, len(seen))
	}
}

// AC-A3 (real pod): terminating/snapshotting a session deletes its Pod and
// reclaims cluster resources.
// Blocked on: a real client-go PodOrchestrator (and the snapshot/terminate path).
func TestDeferred_RealPodReclaimed(t *testing.T) {
	t.Skip("deferred: needs the real client-go PodOrchestrator to assert Pod deletion + resource reclaim (AC-A3); fill when the k8s adapter lands")
}

// AC-B1: after 60m idle a session is checkpointed (CRIU) and transitions to
// snapshot with its pod reclaimed.
// Blocked on: an idle->snapshot trigger (reaper or test-only endpoint). The α SUT
// never leaves the active state, so there is no way to drive a snapshot here.
func TestDeferred_IdleToSnapshot(t *testing.T) {
	t.Skip("deferred: needs an idle->snapshot trigger (reaper or test endpoint) to reach the snapshot state (AC-B1); fill when the lifecycle trigger lands")
}

// AC-B2: accessing a snapshot session restores it into a new pod and goes active.
// Blocked on: a snapshot-state session (see IdleToSnapshot) + real CRIU.
func TestDeferred_SnapshotRestore(t *testing.T) {
	t.Skip("deferred: needs a snapshot-state session (AC-B1 trigger) to exercise restore-on-access (AC-B2); fill when snapshot + restore land")
}

// AC-B3: a restored session's in-memory state matches the pre-snapshot state.
// Blocked on: a verified CRIU runtime (ContainerCheckpoint feature gate) — the
// stub checkpointer carries no real process state. See docs/criu-verification.md.
func TestDeferred_CRIUIntegrity(t *testing.T) {
	t.Skip("deferred: needs a verified CRIU runtime to assert checkpoint/restore integrity (AC-B3); see docs/criu-verification.md")
}

// AC-C2 (idle/snapshot branches): read dispatches on a non-active state.
// Blocked on: idle/snapshot states (lifecycle trigger). The active-read branch
// is covered by TestReadSession_ActivePath in e2e_test.go.
func TestDeferred_ReadIdleAndSnapshotBranches(t *testing.T) {
	t.Skip("deferred: needs idle/snapshot states to assert the non-active read branches (AC-C2); the active branch is covered today")
}

// AC-C3 (idle/snapshot branches): write dispatches on a non-active state.
// Blocked on: idle/snapshot states (lifecycle trigger). The active-write branch
// is covered by TestWriteSession_ActivePath in e2e_test.go.
func TestDeferred_WriteIdleAndSnapshotBranches(t *testing.T) {
	t.Skip("deferred: needs idle/snapshot states to assert the non-active write branches (AC-C3); the active branch is covered today")
}

// AC-C1: concurrent restore/snapshot/switch on the same session converge to a
// single valid state with no torn transitions or duplicate pods.
// Blocked on: a real Redis-backed StateStore + multi-replica control-plane. The
// α SUT is a single replica with an in-memory store, so cross-replica atomicity
// cannot be exercised.
func TestDeferred_CrossReplicaAtomicity(t *testing.T) {
	t.Skip("deferred: needs a Redis-backed StateStore + multi-replica control-plane to assert atomic transitions (AC-C1); fill when the redis adapter lands")
}
