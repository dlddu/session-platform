package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// generationRecorder is the archive checkpointer installed for minimax-code. It
// counts generation-fenced checkpoints, so a test passes only if the service
// reached this type's own strategy through the AC-E5 transaction path.
type generationRecorder struct {
	*archive.StubCheckpointer
	generations int
}

func (g *generationRecorder) CheckpointWithGeneration(ctx context.Context, ref k8s.PodRef, generation string) (*session.Checkpoint, error) {
	g.generations++
	return g.StubCheckpointer.CheckpointWithGeneration(ctx, ref, generation)
}

func newMiniMaxService(archiveOn bool) (*service.Service, *k8s.StubOrchestrator, *generationRecorder) {
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := archive.NewStubCheckpointer(true)
	recorder := &generationRecorder{StubCheckpointer: archive.NewStubCheckpointer(true)}
	var opts []service.Option
	if archiveOn {
		opts = append(opts, service.WithWorkloadCheckpointer(session.WorkloadTypeMiniMaxCode, recorder))
	}
	return service.New(orch, store, ckpt, agent.NewStubClient(), opts...), orch, recorder
}

func TestMiniMaxCodeCreateAppliesTypeAndModel(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		model, want string
	}{
		{model: "", want: session.PlatformDefaultModel},
		{model: "MiniMax-M3", want: "MiniMax-M3"},
	} {
		svc, orch, _ := newMiniMaxService(false)
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

func TestMiniMaxCodeSurvivesSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch, recorder := newMiniMaxService(true)
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
	if recorder.generations != 1 {
		t.Fatalf("generation-fenced checkpoints = %d, want 1", recorder.generations)
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

func TestMiniMaxCodeSnapshotIsRefusedWithoutItsGate(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newMiniMaxService(false)
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

func TestMiniMaxCodeOversizedPromptIsRejectedBeforeRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newMiniMaxService(true)
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
