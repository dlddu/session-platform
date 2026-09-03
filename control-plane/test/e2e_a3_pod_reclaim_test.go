//go:build e2e

// 검증 AC: AC-A3
//
// Freezing a session reclaims the cluster resources it held (docs/prd/architecture.md,
// docs/test/architecture.md scenario 3): the API clears the pod field AND the
// backing Pod object is actually deleted — reclaim, not merely dropped from the
// API's view.
//
// The freeze is driven by the product snapshot endpoint; its automatic
// counterpart (service.IdleReaper's idle window, AC-B1) is a registered
// exception in docs/test/e2e.md.
package e2e_test

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodReclaim_FreezeDeletesTheBackingPod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — Pod-reclaim assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	// Precondition: an active session backed by a real Pod object.
	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("created session has no pod")
	}
	getPodEventually(t, cs, ns, s.Pod) // the Pod exists before the freeze

	frozen, ok := snapshotSession(t, s.ID)
	if !ok {
		t.Skip("SUT predates the product snapshot endpoint — Pod reclaim not exercisable here")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot", frozen.State)
	}
	if frozen.Pod != "" {
		t.Fatalf("pod after snapshot = %q, want it reclaimed (AC-A3)", frozen.Pod)
	}

	// Ground truth from the cluster: the backing Pod object is actually gone (or
	// at least accepted for deletion). Poll past the pod's termination grace
	// (default 30s) before giving up.
	deadline := time.Now().Add(90 * time.Second)
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), s.Pod, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break // fully deleted — resources reclaimed
		}
		if err != nil {
			t.Fatalf("get pod %s/%s: %v", ns, s.Pod, err)
		}
		if pod.DeletionTimestamp != nil {
			break // accepted for deletion (terminating) — reclaim in progress
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s still present with no deletion after snapshot — not reclaimed (AC-A3)", ns, s.Pod)
		}
		time.Sleep(time.Second)
	}

	// And the session reads back unambiguously frozen — an independent
	// confirmation the reclaim stuck.
	got := getSession(t, s.ID)
	if got.State != "snapshot" || got.Pod != "" {
		t.Fatalf("session after snapshot = %+v, want state=snapshot pod=\"\" (AC-A3)", got)
	}
}
