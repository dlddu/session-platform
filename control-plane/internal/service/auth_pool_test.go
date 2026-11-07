package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/store"
)

type poolHarness struct {
	svc   *service.Service
	orch  *k8s.StubOrchestrator
	pool  *k8s.MemoryAuthPool
	store store.StateStore
}

func newPoolHarness(t *testing.T, slots int, orch k8s.PodOrchestrator, stub *k8s.StubOrchestrator) poolHarness {
	t.Helper()
	ckpt := archive.NewStubCheckpointer(true)
	pool := k8s.NewMemoryAuthPool(slots)
	st := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	svc := service.New(orch, st, ckpt, agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexTask, ckpt),
		service.WithAuthPool(session.WorkloadTypeCodexTask, pool),
	)
	return poolHarness{svc: svc, orch: stub, pool: pool, store: st}
}

func newStubPoolHarness(t *testing.T, slots int) poolHarness {
	stub := k8s.NewStubOrchestrator("sessions")
	return newPoolHarness(t, slots, stub, stub)
}

func createTask(t *testing.T, h poolHarness, name string) *session.Session {
	t.Helper()
	sess, err := h.svc.Create(context.Background(), session.CreateRequest{
		Name: name, WorkloadType: session.WorkloadTypeCodexTask, ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return sess
}

// AC-L2/L3: a codex-task session holds a slot exactly while it has a pod.
func TestAuthPoolLeaseFollowsPod(t *testing.T) {
	ctx := context.Background()
	h := newStubPoolHarness(t, 1)

	a := createTask(t, h, "a")
	spec := h.orch.SpecFor(a.ID)
	if spec.AuthSlot == nil || *spec.AuthSlot != 0 || spec.ReasoningEffort != "high" {
		t.Fatalf("pod spec = %+v, want slot 0 and effort high", spec)
	}
	if a.ReasoningEffort != "high" {
		t.Fatalf("session effort = %q", a.ReasoningEffort)
	}
	if _, err := h.svc.Create(ctx, session.CreateRequest{Name: "b", WorkloadType: session.WorkloadTypeCodexTask}); !errors.Is(err, session.ErrAuthPoolExhausted) {
		t.Fatalf("second create err = %v, want exhausted", err)
	}
	if n := h.orch.RunningCount(); n != 1 {
		t.Fatalf("running pods = %d after a refused create, want 1", n)
	}

	if _, err := h.svc.Snapshot(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if holder := h.pool.Holder(0); holder != "" {
		t.Fatalf("frozen session still holds the slot (holder %q)", holder)
	}
	b := createTask(t, h, "b")

	// A full pool leaves the frozen session frozen (AC-L3).
	if _, err := h.svc.Restore(ctx, a.ID); !errors.Is(err, session.ErrAuthPoolExhausted) {
		t.Fatalf("restore with a full pool err = %v, want exhausted", err)
	}
	if got, _ := h.svc.Get(ctx, a.ID); got.State != session.StateSnapshot {
		t.Fatalf("state after a refused restore = %s", got.State)
	}

	if err := h.svc.Terminate(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := h.svc.Restore(ctx, a.ID)
	if err != nil {
		t.Fatalf("restore after delete freed the slot: %v", err)
	}
	if restored.ReasoningEffort != "high" || h.orch.SpecFor(a.ID).ReasoningEffort != "high" {
		t.Fatalf("restore lost the effort: session %q, pod %q", restored.ReasoningEffort, h.orch.SpecFor(a.ID).ReasoningEffort)
	}
	if slot, ok := h.pool.HeldBy(a.ID); !ok || slot != 0 {
		t.Fatalf("restored session holds %d, %v", slot, ok)
	}
}

type failingStart struct {
	*k8s.StubOrchestrator
}

func (failingStart) Start(context.Context, string, k8s.WorkloadSpec) (k8s.SessionPods, error) {
	return k8s.SessionPods{}, errors.New("apiserver down")
}

// A create that never reaches a pod gives its slot back (AC-L2).
func TestAuthPoolReleasedWhenStartFails(t *testing.T) {
	stub := k8s.NewStubOrchestrator("sessions")
	h := newPoolHarness(t, 1, failingStart{stub}, stub)
	if _, err := h.svc.Create(context.Background(), session.CreateRequest{Name: "x", WorkloadType: session.WorkloadTypeCodexTask}); err == nil {
		t.Fatal("create succeeded without a pod")
	}
	if holder := h.pool.Holder(0); holder != "" {
		t.Fatalf("failed create kept the slot (holder %q)", holder)
	}
}

// AC-L2's holder half: a lease whose session record is gone is reclaimed.
func TestAuthPoolReclaimsGoneHolder(t *testing.T) {
	h := newStubPoolHarness(t, 1)
	if _, err := h.pool.Acquire(context.Background(), "deleted-long-ago", nil); err != nil {
		t.Fatal(err)
	}
	sess := createTask(t, h, "after-crash")
	if slot, ok := h.pool.HeldBy(sess.ID); !ok || slot != 0 {
		t.Fatalf("slot not reclaimed from a gone holder: %d, %v", slot, ok)
	}
}

// A type without a pool takes no slot; claude-code here runs in gateway mode.
func TestAuthPoolOnlyForPooledTypes(t *testing.T) {
	h := newStubPoolHarness(t, 1)
	cc, err := h.svc.Create(context.Background(), session.CreateRequest{Name: "cc", WorkloadType: session.WorkloadTypeClaudeCode, ReasoningEffort: "max"})
	if err != nil {
		t.Fatal(err)
	}
	if spec := h.orch.SpecFor(cc.ID); spec.AuthSlot != nil || spec.ReasoningEffort != "max" {
		t.Fatalf("claude-code spec = %+v", spec)
	}
	if holder := h.pool.Holder(0); holder != "" {
		t.Fatalf("a pool-less type took slot 0 (holder %q)", holder)
	}
}

// AC-E8/AC-M4: efforts outside the type's list never reach a pod.
func TestCreateRejectsForeignEffort(t *testing.T) {
	h := newStubPoolHarness(t, 1)
	for _, req := range []session.CreateRequest{
		{Name: "x", WorkloadType: session.WorkloadTypeCodexTask, ReasoningEffort: "max"},
		{Name: "x", WorkloadType: session.WorkloadTypeClaudeCode, ReasoningEffort: "minimal"},
		{Name: "x", WorkloadType: session.WorkloadTypeShell, ReasoningEffort: "low"},
		{Name: "x", WorkloadType: session.WorkloadTypeCodexChat, ReasoningEffort: "low"},
		{Name: "x", WorkloadType: session.WorkloadTypeApprovalGated, ReasoningEffort: "low"},
	} {
		if _, err := h.svc.Create(context.Background(), req); !errors.Is(err, session.ErrInvalidInput) {
			t.Errorf("%s effort %q: err = %v, want invalid input", req.WorkloadType, req.ReasoningEffort, err)
		}
	}
	if n := h.orch.RunningCount(); n != 0 {
		t.Fatalf("running pods = %d, want none", n)
	}
	if holder := h.pool.Holder(0); holder != "" {
		t.Fatalf("a refused create took the slot (holder %q)", holder)
	}
}

// AC-J1: records stored before the codex split read as codex-chat.
func TestStoredLegacyCodexReadsAsCodexChat(t *testing.T) {
	h := newStubPoolHarness(t, 1)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := h.store.Put(ctx, &session.Session{
		ID: "legacy", Name: "old-chat", WorkloadType: "codex", Model: "gpt-x",
		State: session.StateSnapshot, CreatedAt: now, LastAccess: now,
		Checkpoint: &session.Checkpoint{Ref: "ref", CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := h.svc.Get(ctx, "legacy")
	if err != nil || got.WorkloadType != session.WorkloadTypeCodexChat {
		t.Fatalf("legacy record = %+v, %v", got, err)
	}
	all, err := h.svc.List(ctx)
	if err != nil || len(all) != 1 || all[0].WorkloadType != session.WorkloadTypeCodexChat {
		t.Fatalf("List = %v, %v", all, err)
	}
}

// AC-J7 applies to both codex types.
func TestRefreshPluginsCodexTask(t *testing.T) {
	h := newStubPoolHarness(t, 1)
	sess := createTask(t, h, "task")
	if _, err := h.svc.RefreshPlugins(context.Background(), sess.ID); err != nil {
		t.Fatalf("codex-task refresh: %v", err)
	}
}
