//go:build integration

// This file exercises the *real* client-go PodOrchestrator against a fake
// clientset (no cluster needed), asserting the create/label/1:1/delete contract
// that the in-memory stub can only approximate. The HTTP happy-path scenarios in
// integration_test.go still run against the stub adapters.
package integration_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
)

const testNS = "sessions"

// newReadyOrchestrator returns a ClientOrchestrator backed by a fake clientset
// that immediately marks created pods Running+Ready. The fake has no kubelet to
// transition pods, so without this the orchestrator's readiness wait would block
// until timeout. The short poll/timeout keeps the test fast.
func newReadyOrchestrator(t *testing.T) (*k8s.ClientOrchestrator, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		})
		// Fall through (handled=false) to the tracker, which persists the pod we
		// just mutated, so a later Get observes it Ready.
		return false, pod, nil
	})
	orch := k8s.NewClientOrchestrator(cs, testNS, k8s.WithReadiness(time.Millisecond, 5*time.Second))
	return orch, cs
}

func listPods(t *testing.T, cs *fake.Clientset) []corev1.Pod {
	t.Helper()
	pods, err := cs.CoreV1().Pods(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return pods.Items
}

// Scenario 1 (AC-A1/A2): Start creates exactly one pod in the orchestrator's
// namespace, labelled 1:1 to the session.
func TestClientOrchestrator_StartCreatesOnePodWithLabel(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	ref, err := orch.Start(context.Background(), "a1b2")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ref.Namespace != testNS {
		t.Fatalf("ref namespace=%q want %q", ref.Namespace, testNS)
	}
	pods := listPods(t, cs)
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods))
	}
	p := pods[0]
	if p.Name != ref.Name {
		t.Fatalf("pod name=%q want %q (Start ref)", p.Name, ref.Name)
	}
	if p.Namespace != testNS {
		t.Fatalf("pod namespace=%q want %q", p.Namespace, testNS)
	}
	if got := p.Labels[k8s.LabelSessionID]; got != "a1b2" {
		t.Fatalf("%s label=%q want %q (AC-A2 1:1)", k8s.LabelSessionID, got, "a1b2")
	}
	if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image == "" {
		t.Fatalf("expected one container with an image, got %+v", p.Spec.Containers)
	}
}

// Scenario 2 (AC-A2): N sessions => N unique pods, each labelled to its session.
func TestClientOrchestrator_NSessionsNUniquePods(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	ids := []string{"aa01", "bb02", "cc03", "dd04"}
	names := map[string]bool{}
	for _, id := range ids {
		ref, err := orch.Start(context.Background(), id)
		if err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		names[ref.Name] = true
	}
	if len(names) != len(ids) {
		t.Fatalf("expected %d unique pod names, got %d: %v", len(ids), len(names), names)
	}
	pods := listPods(t, cs)
	if len(pods) != len(ids) {
		t.Fatalf("expected %d pods, got %d", len(ids), len(pods))
	}
	for _, p := range pods {
		if p.Labels[k8s.LabelSessionID] == "" {
			t.Fatalf("pod %s missing %s label", p.Name, k8s.LabelSessionID)
		}
	}
}

// Scenario 3 (AC-A3): Stop deletes the pod and is idempotent.
func TestClientOrchestrator_StopDeletesPodIdempotently(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	ref, err := orch.Start(context.Background(), "ee05")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if n := len(listPods(t, cs)); n != 1 {
		t.Fatalf("expected 1 pod after start, got %d", n)
	}
	if err := orch.Stop(context.Background(), ref); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := len(listPods(t, cs)); n != 0 {
		t.Fatalf("expected 0 pods after stop, got %d", n)
	}
	// Deleting an already-gone pod is not an error (AC-A3 reclaim is idempotent).
	if err := orch.Stop(context.Background(), ref); err != nil {
		t.Fatalf("stop (idempotent) returned error: %v", err)
	}
}
