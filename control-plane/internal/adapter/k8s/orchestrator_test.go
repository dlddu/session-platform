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
	ref, err := orch.Start(context.Background(), "stop-me", k8s.WorkloadSpec{Type: session.WorkloadTypeShell})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := orch.Stop(context.Background(), k8s.PodRef{Name: ref.Name}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods = %d, want 0", got)
	}
}
