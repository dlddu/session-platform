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

// newServiceWithOrch hands back the stub orchestrator too, so a test can assert
// which workload type actually reached provisioning rather than only what the
// stored record says.
func newServiceWithOrch() (*service.Service, *k8s.StubOrchestrator, *configmap.Store) {
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := criu.NewStubCheckpointer(true)
	svc := service.New(
		orch, store, ckpt, agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	return svc, orch, store
}

// Immutable workload settings are validated before provisioning, copied into the
// pod request, and persisted on the session record.
func TestCreateAppliesWorkloadType(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		requested session.WorkloadType
		model     string
		want      session.WorkloadType
		wantModel string
	}{
		{
			name: "unspecified defaults to shell",
			want: session.WorkloadTypeShell,
		},
		{
			name:      "explicit shell",
			requested: session.WorkloadTypeShell,
			want:      session.WorkloadTypeShell,
		},
		{
			name:      "claude-code defaults to platform model",
			requested: session.WorkloadTypeClaudeCode,
			want:      session.WorkloadTypeClaudeCode,
			wantModel: session.PlatformDefaultModel,
		},
		{
			name:      "explicit claude-code model",
			requested: session.WorkloadTypeClaudeCode,
			model:     "claude-sonnet-4-5",
			want:      session.WorkloadTypeClaudeCode,
			wantModel: "claude-sonnet-4-5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, orch, store := newServiceWithOrch()
			sess, err := svc.Create(ctx, session.CreateRequest{
				Name: tc.name, WorkloadType: tc.requested, Model: tc.model,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if sess.WorkloadType != tc.want {
				t.Errorf("returned workloadType = %q, want %q", sess.WorkloadType, tc.want)
			}
			if sess.Model != tc.wantModel {
				t.Errorf("returned model = %q, want %q", sess.Model, tc.wantModel)
			}
			if got := orch.WorkloadFor(sess.ID); got != tc.want {
				t.Errorf("orchestrator provisioned for %q, want %q", got, tc.want)
			}
			if got := orch.ModelFor(sess.ID); got != tc.wantModel {
				t.Errorf("orchestrator model = %q, want %q", got, tc.wantModel)
			}
			// The record — not just the in-memory value — has to carry it, since
			// restore and the reaper read it back from the store.
			stored, err := store.Get(ctx, sess.ID)
			if err != nil {
				t.Fatalf("get stored: %v", err)
			}
			if stored.WorkloadType != tc.want {
				t.Errorf("stored workloadType = %q, want %q", stored.WorkloadType, tc.want)
			}
			if stored.Model != tc.wantModel {
				t.Errorf("stored model = %q, want %q", stored.Model, tc.wantModel)
			}
		})
	}
}

// An unknown type is rejected outright — and, importantly, before any
// pod is provisioned, so a bad request cannot leak cluster resources.
func TestCreateRejectsUnknownWorkloadType(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newServiceWithOrch()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "bad-type", WorkloadType: "definitely-not-a-type"})
	if !errors.Is(err, session.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if sess != nil {
		t.Errorf("session = %+v, want nil", sess)
	}
	if n := orch.RunningCount(); n != 0 {
		t.Errorf("orchestrator holds %d pods after a rejected create, want 0", n)
	}
}

// Invalid model settings fail before provisioning. Shell has no model,
// and Claude Code model identifiers cannot contain whitespace or CLI flags.
func TestCreateRejectsInvalidModel(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		workload session.WorkloadType
		model    string
	}{
		{
			name:     "shell model",
			workload: session.WorkloadTypeShell,
			model:    "claude-sonnet-4-5",
		},
		{
			name:     "claude-code whitespace",
			workload: session.WorkloadTypeClaudeCode,
			model:    "bad model",
		},
		{
			name:     "claude-code option",
			workload: session.WorkloadTypeClaudeCode,
			model:    "--danger",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, orch, _ := newServiceWithOrch()
			got, err := svc.Create(ctx, session.CreateRequest{
				Name: tc.name, WorkloadType: tc.workload, Model: tc.model,
			})
			if !errors.Is(err, session.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if got != nil {
				t.Errorf("session = %+v, want nil", got)
			}
			if count := orch.RunningCount(); count != 0 {
				t.Errorf("running pods = %d, want 0", count)
			}
		})
	}
}

// Immutable workload settings survive the freeze/restore round
// trip and reach the brand-new pod.
func TestWorkloadTypeSurvivesSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newServiceWithOrch()

	const model = "claude-sonnet-4-5"
	sess, err := svc.Create(ctx, session.CreateRequest{
		Name: "round-trip", WorkloadType: session.WorkloadTypeClaudeCode, Model: model,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored, err := svc.Restore(ctx, sess.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.WorkloadType != session.WorkloadTypeClaudeCode {
		t.Errorf("restored workloadType = %q, want %q", restored.WorkloadType, session.WorkloadTypeClaudeCode)
	}
	if restored.Model != model {
		t.Errorf("restored model = %q, want %q", restored.Model, model)
	}
	if got := orch.WorkloadFor(sess.ID); got != session.WorkloadTypeClaudeCode {
		t.Errorf("restore provisioned for %q, want %q", got, session.WorkloadTypeClaudeCode)
	}
	if got := orch.ModelFor(sess.ID); got != model {
		t.Errorf("restore provisioned with model %q, want %q", got, model)
	}
}

func TestOversizedClaudePromptIsRejectedBeforeRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newServiceWithOrch()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "limited", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods after snapshot = %d, want 0", got)
	}

	result, err := svc.Write(ctx, sess.ID, strings.Repeat("x", session.MaxClaudePromptBytes+1))
	if !errors.Is(err, session.ErrWorkloadPromptTooLarge) {
		t.Fatalf("write error = %v, want %v", err, session.ErrWorkloadPromptTooLarge)
	}
	if result != nil {
		t.Fatalf("write result = %+v, want nil", result)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("oversized write restored %d pods, want 0", got)
	}

	stored, err := svc.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.State != session.StateSnapshot {
		t.Fatalf("state = %q, want snapshot", stored.State)
	}
}

// Sessions written before the type axis existed have no workloadType at all.
// Reading one back must yield the shell default — the only type they could have
// been — rather than an empty type that would fail provisioning on restore.
func TestLegacySessionRecordRestoresAsShell(t *testing.T) {
	ctx := context.Background()
	svc, orch, store := newServiceWithOrch()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "legacy"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Rewrite the stored record the way a pre-AC-E1 control plane wrote it.
	stored, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	stored.WorkloadType = ""
	if err := store.Put(ctx, stored); err != nil {
		t.Fatalf("put legacy record: %v", err)
	}
	got, err := svc.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get legacy session: %v", err)
	}
	if got.WorkloadType != session.WorkloadTypeShell || got.Model != "" {
		t.Fatalf("legacy get = (%q, %q), want (shell, empty model)", got.WorkloadType, got.Model)
	}
	listed, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list legacy sessions: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed sessions = %d, want 1", len(listed))
	}
	if listed[0].WorkloadType != session.WorkloadTypeShell || listed[0].Model != "" {
		t.Fatalf(
			"legacy list = (%q, %q), want (shell, empty model)",
			listed[0].WorkloadType, listed[0].Model,
		)
	}

	if _, err := svc.Restore(ctx, sess.ID); err != nil {
		t.Fatalf("restore legacy session: %v", err)
	}
	if got := orch.WorkloadFor(sess.ID); got != session.WorkloadTypeShell {
		t.Errorf("legacy restore provisioned for %q, want %q", got, session.WorkloadTypeShell)
	}
	if got := orch.ModelFor(sess.ID); got != "" {
		t.Errorf("legacy shell restore provisioned with model %q, want empty", got)
	}
}
