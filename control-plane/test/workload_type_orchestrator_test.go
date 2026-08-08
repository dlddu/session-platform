//go:build integration

// AC-E1 on the *real* client-go orchestrator: the workload type a session was
// created with has to reach the cluster object, not just the control plane's
// own record. These run against the same fake clientset as
// client_orchestrator_test.go, so they assert the pod spec the orchestrator
// actually submits.
package integration_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const claudeCodeImage = "ghcr.io/dlddu/session-platform-claude-code:dev"

// envOf returns the value of the named env var on the pod's session container.
func envOf(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// AC-E1: "control plane은 타입에 따라 서로 다른 data plane 워크로드로 pod를
// 프로비저닝한다" — the two types must not produce the same pod. Image, the
// workload env var and the workload label all have to follow the type.
func TestClientOrchestrator_PodSpecBranchesOnWorkloadType(t *testing.T) {
	const shellImage = "ghcr.io/dlddu/session-platform-data-plane:dev"

	cases := []struct {
		workload  session.WorkloadType
		wantImage string
	}{
		{session.WorkloadTypeShell, shellImage},
		{session.WorkloadTypeClaudeCode, claudeCodeImage},
	}
	for _, tc := range cases {
		orch, cs := newReadyOrchestrator(t,
			k8s.WithImage(shellImage),
			k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage))
		if _, err := orch.Start(context.Background(), "wt01", tc.workload); err != nil {
			t.Fatalf("start (%s): %v", tc.workload, err)
		}
		pod := listPods(t, cs)[0]
		c := pod.Spec.Containers[0]
		if c.Image != tc.wantImage {
			t.Errorf("%s: image = %q, want %q", tc.workload, c.Image, tc.wantImage)
		}
		if got, ok := envOf(c, "DATA_PLANE_WORKLOAD"); !ok || got != string(tc.workload) {
			t.Errorf("%s: DATA_PLANE_WORKLOAD = %q (present=%v), want %q", tc.workload, got, ok, tc.workload)
		}
		if got := pod.Labels[k8s.LabelWorkloadType]; got != string(tc.workload) {
			t.Errorf("%s: %s label = %q, want %q", tc.workload, k8s.LabelWorkloadType, got, tc.workload)
		}
	}
}

// An unspecified type on the orchestrator boundary is the shell default, the
// same rule the service layer applies — so a caller that predates the type axis
// keeps getting exactly the pod it used to get.
func TestClientOrchestrator_UnspecifiedWorkloadTypeIsShell(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"))
	if _, err := orch.Start(context.Background(), "wt02", ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	pod := listPods(t, cs)[0]
	if got := pod.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeShell) {
		t.Errorf("%s label = %q, want %q", k8s.LabelWorkloadType, got, session.WorkloadTypeShell)
	}
	if got, _ := envOf(pod.Spec.Containers[0], "DATA_PLANE_WORKLOAD"); got != string(session.WorkloadTypeShell) {
		t.Errorf("DATA_PLANE_WORKLOAD = %q, want %q", got, session.WorkloadTypeShell)
	}
}

// A type with no image configured must fail loudly and provision nothing. The
// alternative — silently falling back to the shell image — would hand the
// caller a session that claims a workload it is not running, which is the exact
// "gate-behind-a-stub" shape the convergence model rejects.
func TestClientOrchestrator_UnconfiguredWorkloadTypeIsRefused(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"))
	_, err := orch.Start(context.Background(), "wt03", session.WorkloadTypeClaudeCode)
	if err == nil {
		t.Fatal("start with an unconfigured workload type succeeded, want an error")
	}
	if !strings.Contains(err.Error(), string(session.WorkloadTypeClaudeCode)) {
		t.Errorf("error %q does not name the workload type", err)
	}
	if pods := listPods(t, cs); len(pods) != 0 {
		t.Errorf("created %d pods for a refused workload type, want 0", len(pods))
	}
}

// AC-E1 + AC-B2: a restore never changes the type, so the restore-target pod
// carries the same image, env and label as the pod that was frozen.
func TestClientOrchestrator_RestoreKeepsWorkloadType(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage))

	ref, err := orch.RestoreInto(context.Background(), "wt04", "s3://bucket/wt04.tar", session.WorkloadTypeClaudeCode)
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	var pod *corev1.Pod
	for i, p := range listPods(t, cs) {
		if p.Name == ref.Name {
			pod = &listPods(t, cs)[i]
			break
		}
	}
	if pod == nil {
		t.Fatalf("restore pod %q not found", ref.Name)
	}
	if got := pod.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeClaudeCode) {
		t.Errorf("%s label = %q, want %q", k8s.LabelWorkloadType, got, session.WorkloadTypeClaudeCode)
	}
	if got := pod.Spec.Containers[0].Image; got != claudeCodeImage {
		t.Errorf("restore image = %q, want %q", got, claudeCodeImage)
	}
	if _, ok := pod.Annotations[k8s.AnnotationRestoreCheckpoint]; !ok {
		t.Errorf("restore pod is missing %s", k8s.AnnotationRestoreCheckpoint)
	}
}
