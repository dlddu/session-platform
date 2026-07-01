package service_test

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func newService() *service.Service {
	return service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(false),
	)
}

// newServiceWithStore is like newService but also hands back the store, so a
// test can drive a session into a non-active state directly (there is no
// idle->snapshot reaper yet — that trigger is a separate deferred decision).
// The store is the real ConfigMap adapter over a fake clientset, so
// CompareAndSwapState behaves exactly as in production.
func newServiceWithStore() (*service.Service, *configmap.Store) {
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	svc := service.New(k8s.NewStubOrchestrator("sessions"), store, criu.NewStubCheckpointer(false))
	return svc, store
}

// TestSnapshotRestoreCycle covers active -> snapshot -> restore (AC-B1, AC-B2,
// AC-A3): the pod is reclaimed on snapshot and a new one is provisioned on
// restore.
func TestSnapshotRestoreCycle(t *testing.T) {
	ctx := context.Background()
	svc := newService()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "model-train-7b"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	origPod := sess.Pod

	frozen, err := svc.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if frozen.State != session.StateSnapshot {
		t.Errorf("state = %q, want snapshot", frozen.State)
	}
	if frozen.Pod != "" {
		t.Error("snapshot should reclaim the pod (AC-A3)")
	}
	if frozen.Checkpoint == nil {
		t.Error("snapshot should record checkpoint metadata")
	}

	restored, err := svc.Restore(ctx, sess.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.State != session.StateActive {
		t.Errorf("restored state = %q, want active", restored.State)
	}
	if restored.Pod == "" {
		t.Error("restore should provision a new pod (AC-B2)")
	}
	if restored.Pod == origPod {
		t.Error("restore should provision a *new* pod, not reuse the old name")
	}
}

// TestReadDispatchesOnState covers the uniform resume-on-access read policy
// (AC-C2): active serves in place, idle is promoted to active, snapshot is
// restored to active — and in every non-active case the session ends active.
func TestReadDispatchesOnState(t *testing.T) {
	ctx := context.Background()
	svc, store := newServiceWithStore()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "notebook-alpha"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// active: served directly.
	res, err := svc.Read(ctx, sess.ID)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if res.Path != "active" {
		t.Errorf("active read path = %q, want active", res.Path)
	}

	// idle: a read resumes it to active (idle still holds its pod).
	if err := store.CompareAndSwapState(ctx, sess.ID, session.StateActive, session.StateIdle); err != nil {
		t.Fatalf("force idle: %v", err)
	}
	res, err = svc.Read(ctx, sess.ID)
	if err != nil {
		t.Fatalf("read idle: %v", err)
	}
	if res.Path != "idle->active->read" {
		t.Errorf("idle read path = %q, want idle->active->read", res.Path)
	}
	if res.Session.State != session.StateActive {
		t.Errorf("after idle read, state = %q, want active", res.Session.State)
	}

	// snapshot: a read restores it to active.
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	res, err = svc.Read(ctx, sess.ID)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if res.Path != "snapshot->restore->read" {
		t.Errorf("snapshot read path = %q, want snapshot->restore->read", res.Path)
	}
	if res.Session.State != session.StateActive {
		t.Errorf("after snapshot read, state = %q, want active", res.Session.State)
	}
}

// TestWriteDispatchesOnState mirrors the read test for the write policy (AC-C3):
// snapshot/idle writes are not rejected — the session is restored/promoted to
// active first and then written.
func TestWriteDispatchesOnState(t *testing.T) {
	ctx := context.Background()
	svc, store := newServiceWithStore()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "scrape-worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := svc.Write(ctx, sess.ID, "payload-a")
	if err != nil {
		t.Fatalf("write active: %v", err)
	}
	if res.Path != "active" {
		t.Errorf("active write path = %q, want active", res.Path)
	}

	if err := store.CompareAndSwapState(ctx, sess.ID, session.StateActive, session.StateIdle); err != nil {
		t.Fatalf("force idle: %v", err)
	}
	res, err = svc.Write(ctx, sess.ID, "payload-b")
	if err != nil {
		t.Fatalf("write idle: %v", err)
	}
	if res.Path != "idle->active->write" {
		t.Errorf("idle write path = %q, want idle->active->write", res.Path)
	}
	if res.Session.State != session.StateActive {
		t.Errorf("after idle write, state = %q, want active", res.Session.State)
	}

	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	res, err = svc.Write(ctx, sess.ID, "payload-c")
	if err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	if res.Path != "snapshot->restore->write" {
		t.Errorf("snapshot write path = %q, want snapshot->restore->write", res.Path)
	}
	if res.Session.State != session.StateActive {
		t.Errorf("after snapshot write, state = %q, want active", res.Session.State)
	}
}

// reachTrackingOrchestrator wraps the stub to observe/steer the AC-D1 shell
// reachability verification: it counts Reach calls, can fail them, and records
// which pods were stopped.
type reachTrackingOrchestrator struct {
	*k8s.StubOrchestrator
	reachCalls int
	reachErr   error
	stopped    []k8s.PodRef
}

func (o *reachTrackingOrchestrator) Reach(context.Context, k8s.PodRef) error {
	o.reachCalls++
	return o.reachErr
}

func (o *reachTrackingOrchestrator) Stop(ctx context.Context, ref k8s.PodRef) error {
	o.stopped = append(o.stopped, ref)
	return o.StubOrchestrator.Stop(ctx, ref)
}

func newTrackedService() (*service.Service, *reachTrackingOrchestrator, *configmap.Store) {
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	return service.New(orch, store, criu.NewStubCheckpointer(false)), orch, store
}

// TestCreateVerifiesShellReachability: a session only becomes active after the
// control plane has opened the pod's shell attach stream (AC-D1) — Create must
// call Reach exactly once on the happy path.
func TestCreateVerifiesShellReachability(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newTrackedService()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "shell-check"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if orch.reachCalls != 1 {
		t.Errorf("reach calls = %d, want 1 (AC-D1 verification on create)", orch.reachCalls)
	}
	if sess.State != session.StateActive {
		t.Errorf("state = %q, want active", sess.State)
	}
}

// TestCreateReachFailureRollsBackPod: if the shell is unreachable the session
// must not become active — Create returns the error, reclaims the pod (AC-A3
// hygiene) and registers nothing.
func TestCreateReachFailureRollsBackPod(t *testing.T) {
	ctx := context.Background()
	svc, orch, store := newTrackedService()
	orch.reachErr = errors.New("attach stream refused")

	if _, err := svc.Create(ctx, session.CreateRequest{Name: "unreachable"}); err == nil {
		t.Fatal("create succeeded despite unreachable shell (AC-D1)")
	}
	if len(orch.stopped) != 1 {
		t.Fatalf("stopped %d pods, want 1 (unreachable pod must be reclaimed)", len(orch.stopped))
	}
	sessions, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("store holds %d sessions after failed create, want 0", len(sessions))
	}
}

// TestRestoreReachFailureRollsBackPod: the restore path holds the same bar —
// a restored pod whose shell is unreachable is stopped and the session stays
// in snapshot rather than going active (AC-D1, AC-B2).
func TestRestoreReachFailureRollsBackPod(t *testing.T) {
	ctx := context.Background()
	svc, orch, store := newTrackedService()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "restore-check"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	orch.reachErr = errors.New("attach stream refused")
	stoppedBefore := len(orch.stopped) // snapshot already stopped the original pod
	if _, err := svc.Restore(ctx, sess.ID); err == nil {
		t.Fatal("restore succeeded despite unreachable shell (AC-D1)")
	}
	if got := len(orch.stopped) - stoppedBefore; got != 1 {
		t.Fatalf("restore stopped %d pods, want 1 (unreachable restored pod must be reclaimed)", got)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get after failed restore: %v", err)
	}
	if got.State != session.StateSnapshot {
		t.Errorf("state after failed restore = %q, want snapshot (must not go active)", got.State)
	}
}

// TestTerminate removes the session and reclaims its pod (AC-A3).
func TestTerminate(t *testing.T) {
	ctx := context.Background()
	svc := newService()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "scrape-worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Terminate(ctx, sess.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if _, err := svc.Get(ctx, sess.ID); err != session.ErrNotFound {
		t.Errorf("get after terminate err = %v, want ErrNotFound", err)
	}
}
