//go:build integration

package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
)

// AC-C5: Gone tells a lost pod (absent, or evicted and never running again)
// from one that still exists.
func TestClientOrchestratorGone(t *testing.T) {
	pod := func(name string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	client := fake.NewSimpleClientset(
		pod("running", corev1.PodRunning),
		pod("pending", corev1.PodPending),
		pod("evicted", corev1.PodFailed),
		pod("finished", corev1.PodSucceeded),
	)
	orch := k8s.NewClientOrchestrator(client, testNS)

	for name, want := range map[string]bool{
		"running":  false,
		"pending":  false,
		"evicted":  true,
		"finished": true,
		"missing":  true,
	} {
		got, err := orch.Gone(context.Background(), k8s.PodRef{Name: name})
		if err != nil {
			t.Fatalf("Gone(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("Gone(%s) = %v, want %v", name, got, want)
		}
	}
}
