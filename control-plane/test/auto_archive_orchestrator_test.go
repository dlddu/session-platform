//go:build integration

package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type autoArchiveBases struct{ asked []string }

func (b *autoArchiveBases) base(sessionID, pod string) (string, error) {
	b.asked = append(b.asked, pod)
	return "https://bucket.s3/sessions/" + sessionID + "/checkpoint.tar", nil
}

func assertAutoArchive(t *testing.T, pod corev1.Pod, wantBase, wantPath string) {
	t.Helper()
	c := pod.Spec.Containers[0]
	if got, _ := envValue(c, k8s.AutoArchiveURLBaseEnvVar); got != wantBase {
		t.Fatalf("pod %s %s = %q, want %q", pod.Name, k8s.AutoArchiveURLBaseEnvVar, got, wantBase)
	}
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Exec == nil {
		t.Fatalf("pod %s container %s has no exec preStop hook", pod.Name, c.Name)
	}
	cmd := c.Lifecycle.PreStop.Exec.Command
	if len(cmd) != 3 || cmd[2] != wantPath {
		t.Fatalf("pod %s preStop = %v, want the agent calling %s", pod.Name, cmd, wantPath)
	}
	if g := pod.Spec.TerminationGracePeriodSeconds; g == nil || *g != k8s.AutoArchiveGracePeriodSeconds {
		t.Fatalf("pod %s grace = %v, want %d", pod.Name, g, k8s.AutoArchiveGracePeriodSeconds)
	}
}

// AC-B5: a pod of a covered type is told its one upload destination and calls
// its agent before it stops.
func TestAutoArchive_WorkloadPodArchivesBeforeStopping(t *testing.T) {
	bases := &autoArchiveBases{}
	orch, cs := newReadyOrchestrator(t, k8s.WithAutoArchive(bases.base, session.WorkloadTypeShell))
	const id = "0123456789abcdef0123456789abcdef"
	_, err := orch.Start(context.Background(), id, k8s.WorkloadSpec{Type: session.WorkloadTypeShell})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pods := listPods(t, cs)
	if len(pods) != 1 {
		t.Fatalf("pods = %d, want 1", len(pods))
	}
	assertAutoArchive(t, pods[0], "https://bucket.s3/sessions/"+id+"/checkpoint.tar", k8s.AutoArchiveRunPath)
}

// A type the option does not cover keeps the plain pod.
func TestAutoArchive_UncoveredTypeIsUnchanged(t *testing.T) {
	bases := &autoArchiveBases{}
	orch, cs := newReadyOrchestrator(t, k8s.WithAutoArchive(bases.base, session.WorkloadTypeClaudeCode))
	if _, err := orch.Start(context.Background(), "a1b2", k8s.WorkloadSpec{Type: session.WorkloadTypeShell}); err != nil {
		t.Fatalf("start: %v", err)
	}
	p := listPods(t, cs)[0]
	if p.Spec.Containers[0].Lifecycle != nil || p.Spec.TerminationGracePeriodSeconds != nil || len(bases.asked) != 0 {
		t.Fatalf("uncovered pod got automatic-archive settings: %+v", p.Spec)
	}
}

// AC-B5: a gated session's helper relays its workload pod's upload, so it is
// told the workload pod's destination and holds its own termination for it.
func TestAutoArchive_GatedHelperHoldsForItsWorkloadPod(t *testing.T) {
	bases := &autoArchiveBases{}
	const id = "fedcba9876543210fedcba9876543210"
	_, set := startMiniMax(t, id, k8s.WithAutoArchive(bases.base, session.WorkloadTypeMiniMaxCode))
	want := "https://bucket.s3/sessions/" + id + "/checkpoint.tar"
	assertAutoArchive(t, set.workload, want, k8s.AutoArchiveRunPath)
	assertAutoArchive(t, set.helper, want, k8s.AutoArchiveHoldPath)
}
