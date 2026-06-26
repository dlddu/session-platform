package service_test

import (
	"context"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/redis"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func newService() *service.Service {
	return service.New(
		k8s.NewStubOrchestrator("sessions"),
		redis.NewStubStore(""),
		criu.NewStubCheckpointer(false),
	)
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

// TestReadDispatchesOnState covers the state-branched read paths (AC-C2),
// including snapshot->restore->read.
func TestReadDispatchesOnState(t *testing.T) {
	ctx := context.Background()
	svc := newService()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "notebook-alpha"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := svc.Read(ctx, sess.ID)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if res.Path != "active" {
		t.Errorf("active read path = %q, want active", res.Path)
	}

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
