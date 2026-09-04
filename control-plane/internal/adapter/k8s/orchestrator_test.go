package k8s_test

import (
	"context"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func TestStubOrchestratorRecordsModel(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		model string
	}{
		{name: "platform default", id: "default-model", model: session.PlatformDefaultModel},
		{name: "specified model", id: "specified-model", model: "claude-sonnet-4-5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := k8s.NewStubOrchestrator("sessions")
			_, err := orch.Start(context.Background(), tc.id, k8s.WorkloadSpec{
				Type: session.WorkloadTypeClaudeCode, Model: tc.model,
			})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if got := orch.ModelFor(tc.id); got != tc.model {
				t.Errorf("ModelFor(%q) = %q, want %q", tc.id, got, tc.model)
			}
		})
	}
}

func TestStubOrchestratorRestoreRecordsModel(t *testing.T) {
	orch := k8s.NewStubOrchestrator("sessions")
	const (
		id    = "restore-model"
		model = "claude-sonnet-4-5"
	)
	_, err := orch.RestoreInto(context.Background(), id, "checkpoint-ref", k8s.WorkloadSpec{
		Type: session.WorkloadTypeClaudeCode, Model: model,
	})
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	if got := orch.ModelFor(id); got != model {
		t.Errorf("ModelFor(%q) after restore = %q, want %q", id, got, model)
	}
}

func TestStubOrchestratorStopAcceptsNamespaceOptionalRef(t *testing.T) {
	orch := k8s.NewStubOrchestrator("sessions")
	pods, err := orch.Start(context.Background(), "stop-me", k8s.WorkloadSpec{Type: session.WorkloadTypeShell})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := orch.Stop(context.Background(), k8s.PodRef{Name: pods.Workload.Name}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods = %d, want 0", got)
	}
}

// TestStubOrchestratorStartsAuxiliaryPods: a session's pod set is the workload
// pod plus the auxiliary pods its type requires (AC-A2, AC-F4).
func TestStubOrchestratorStartsAuxiliaryPods(t *testing.T) {
	orch := k8s.NewStubOrchestrator("sessions")
	orch.SetAuxiliaryPods(1)
	pods, err := orch.Start(context.Background(), "aux", k8s.WorkloadSpec{Type: session.WorkloadTypeShell})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(pods.Auxiliary) != 1 {
		t.Fatalf("auxiliary pods = %d, want 1", len(pods.Auxiliary))
	}
	if pods.Auxiliary[0].Name == pods.Workload.Name {
		t.Fatalf("auxiliary pod reuses the workload pod name %q", pods.Workload.Name)
	}
	all := pods.All()
	if len(all) != 2 || all[0] != pods.Workload {
		t.Fatalf("All() = %v, want the workload pod first followed by its auxiliary pod", all)
	}
	if got := orch.RunningCount(); got != 2 {
		t.Fatalf("running pods = %d, want 2 (workload + auxiliary)", got)
	}
	if got := orch.SessionsCount(); got != 1 {
		t.Fatalf("sessions holding pods = %d, want 1 (AC-A2 is still 1:1 on the workload pod)", got)
	}
}

// TestStubOrchestratorPartialStopKeepsSessionVisible: AC-A3 is satisfied when
// every pod of the session is gone, not just the first one.
func TestStubOrchestratorPartialStopKeepsSessionVisible(t *testing.T) {
	orch := k8s.NewStubOrchestrator("sessions")
	orch.SetAuxiliaryPods(1)
	pods, err := orch.Start(context.Background(), "partial", k8s.WorkloadSpec{Type: session.WorkloadTypeShell})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := orch.Stop(context.Background(), k8s.PodRef{Name: pods.Workload.Name}); err != nil {
		t.Fatalf("stop workload pod: %v", err)
	}
	if got := orch.RunningCount(); got != 1 {
		t.Fatalf("running pods after reclaiming only the workload pod = %d, want 1 (the auxiliary pod leaked)", got)
	}
	if err := orch.Stop(context.Background(), pods.All()...); err != nil {
		t.Fatalf("stop whole set: %v", err)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods after reclaiming the set = %d, want 0 (AC-A3)", got)
	}
}
