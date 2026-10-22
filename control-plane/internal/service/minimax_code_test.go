package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// newMiniMaxService wires the archive checkpointer for minimax-code only when
// archive is true, mirroring MINIMAX_CODE_ARCHIVE_ENABLED (AC-G4).
func newMiniMaxService(archive bool) (*service.Service, *k8s.StubOrchestrator) {
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := criu.NewStubCheckpointer(true)
	var opts []service.Option
	if archive {
		opts = append(opts, service.WithWorkloadCheckpointer(session.WorkloadTypeMiniMaxCode, ckpt))
	}
	return service.New(orch, store, ckpt, agent.NewStubClient(), opts...), orch
}

// AC-G1: the type and its model reach provisioning with the AC-E6 contract.
func TestMiniMaxCodeCreateAppliesTypeAndModel(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		model, want string
	}{
		{model: "", want: session.PlatformDefaultModel},
		{model: "MiniMax-M3", want: "MiniMax-M3"},
	} {
		svc, orch := newMiniMaxService(false)
		sess, err := svc.Create(ctx, session.CreateRequest{
			Name: "mm", WorkloadType: session.WorkloadTypeMiniMaxCode, Model: tc.model,
		})
		if err != nil {
			t.Fatalf("create (model %q): %v", tc.model, err)
		}
		if sess.WorkloadType != session.WorkloadTypeMiniMaxCode || sess.Model != tc.want {
			t.Errorf("session = %s/%s, want %s/%s", sess.WorkloadType, sess.Model, session.WorkloadTypeMiniMaxCode, tc.want)
		}
		if got := orch.WorkloadFor(sess.ID); got != session.WorkloadTypeMiniMaxCode {
			t.Errorf("provisioned for %q, want %q", got, session.WorkloadTypeMiniMaxCode)
		}
		if got := orch.ModelFor(sess.ID); got != tc.want {
			t.Errorf("provisioned model = %q, want %q", got, tc.want)
		}
	}
}

// AC-G4: with its gate on, the type freezes and thaws through the same
// generation-fenced archive path as claude-code and keeps its settings.
func TestMiniMaxCodeSurvivesSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch := newMiniMaxService(true)
	sess, err := svc.Create(ctx, session.CreateRequest{
		Name: "mm-round-trip", WorkloadType: session.WorkloadTypeMiniMaxCode, Model: "MiniMax-M3",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	frozen, err := svc.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if frozen.State != session.StateSnapshot || orch.RunningCount() != 0 {
		t.Fatalf("after snapshot: state %q with %d pods, want snapshot with none", frozen.State, orch.RunningCount())
	}
	restored, err := svc.Restore(ctx, sess.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.WorkloadType != session.WorkloadTypeMiniMaxCode || restored.Model != "MiniMax-M3" {
		t.Errorf("restored = %s/%s, want minimax-code/MiniMax-M3", restored.WorkloadType, restored.Model)
	}
	if got := orch.WorkloadFor(sess.ID); got != session.WorkloadTypeMiniMaxCode {
		t.Errorf("restore provisioned for %q, want %q", got, session.WorkloadTypeMiniMaxCode)
	}
}

// AC-G4: with its gate off, the type refuses to freeze rather than falling
// through to the shell's checkpointer, and keeps its pod.
func TestMiniMaxCodeSnapshotIsRefusedWithoutItsGate(t *testing.T) {
	ctx := context.Background()
	svc, orch := newMiniMaxService(false)
	sess, err := svc.Create(ctx, session.CreateRequest{Name: "mm-no-gate", WorkloadType: session.WorkloadTypeMiniMaxCode})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); !errors.Is(err, session.ErrCheckpointDisabled) {
		t.Fatalf("snapshot err = %v, want ErrCheckpointDisabled", err)
	}
	if orch.RunningCount() == 0 {
		t.Error("refused snapshot reclaimed the session pod")
	}
}

// AC-G2 (AC-E2's 1 MiB bound): an oversized prompt is refused before it can
// thaw a snapshotted session.
func TestMiniMaxCodeOversizedPromptIsRejectedBeforeRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch := newMiniMaxService(true)
	sess, err := svc.Create(ctx, session.CreateRequest{Name: "mm-limited", WorkloadType: session.WorkloadTypeMiniMaxCode})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := svc.Write(ctx, sess.ID, strings.Repeat("x", session.MaxClaudePromptBytes+1)); !errors.Is(err, session.ErrWorkloadPromptTooLarge) {
		t.Fatalf("write error = %v, want %v", err, session.ErrWorkloadPromptTooLarge)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("oversized write restored %d pods, want 0", got)
	}
}
