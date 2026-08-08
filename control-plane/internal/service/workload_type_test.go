package service_test

import (
	"context"
	"errors"
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
// which workload type actually reached provisioning (AC-E1) rather than only
// what the stored record says.
func newServiceWithOrch() (*service.Service, *k8s.StubOrchestrator, *configmap.Store) {
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	svc := service.New(orch, store, criu.NewStubCheckpointer(false), agent.NewStubClient())
	return svc, orch, store
}

// AC-E1: the requested type is validated before anything is provisioned, then
// carried into the pod request and persisted on the session record.
func TestCreateAppliesWorkloadType(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		requested session.WorkloadType
		want      session.WorkloadType
	}{
		{"unspecified defaults to shell", "", session.WorkloadTypeShell},
		{"explicit shell", session.WorkloadTypeShell, session.WorkloadTypeShell},
		{"explicit claude-code", session.WorkloadTypeClaudeCode, session.WorkloadTypeClaudeCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, orch, store := newServiceWithOrch()
			sess, err := svc.Create(ctx, session.CreateRequest{Name: tc.name, WorkloadType: tc.requested})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if sess.WorkloadType != tc.want {
				t.Errorf("returned workloadType = %q, want %q", sess.WorkloadType, tc.want)
			}
			if got := orch.WorkloadFor(sess.ID); got != tc.want {
				t.Errorf("orchestrator provisioned for %q, want %q", got, tc.want)
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
		})
	}
}

// AC-E1: an unknown type is rejected outright — and, importantly, before any
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

// AC-E1: the type survives the freeze/restore round trip. Restore provisions a
// brand-new pod, so this is where a dropped type would silently turn a
// claude-code session into a shell one.
func TestWorkloadTypeSurvivesSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newServiceWithOrch()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "round-trip", WorkloadType: session.WorkloadTypeClaudeCode})
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
	if got := orch.WorkloadFor(sess.ID); got != session.WorkloadTypeClaudeCode {
		t.Errorf("restore provisioned for %q, want %q", got, session.WorkloadTypeClaudeCode)
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

	if _, err := svc.Restore(ctx, sess.ID); err != nil {
		t.Fatalf("restore legacy session: %v", err)
	}
	if got := orch.WorkloadFor(sess.ID); got != session.WorkloadTypeShell {
		t.Errorf("legacy restore provisioned for %q, want %q", got, session.WorkloadTypeShell)
	}
}
