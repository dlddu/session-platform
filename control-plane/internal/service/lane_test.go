package service_test

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// A lane is a conversation inside one pod, not a session of its own (AC-N11),
// so the lifecycle behaviour of a lane call has to be the session's own: a read
// on either lane promotes idle and restores a snapshot exactly as a read does,
// and only the inventory is passive.

func newLaneHarness(t *testing.T) (*service.Service, *agent.StubClient, *k8s.StubOrchestrator) {
	t.Helper()
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := archive.NewStubCheckpointer(true)
	stub := agent.NewStubClient()
	stub.LanePaired(true)
	svc := service.New(orch, configmap.NewStore(fake.NewSimpleClientset(), "sessions"), ckpt, stub,
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexManager, ckpt))
	return svc, stub, orch
}

func newLaneManager(t *testing.T, svc *service.Service, name string) *session.Session {
	t.Helper()
	sess, err := svc.Create(context.Background(), session.CreateRequest{
		Name: name, WorkloadType: session.WorkloadTypeCodexManager,
	})
	if err != nil {
		t.Fatalf("create manager session: %v", err)
	}
	return sess
}

// TestReadLaneRestoresLikeAnyRead: a monitor-lane read of a frozen session
// brings the session back, because the lane is inside the session rather than
// beside it. A lane read that refused instead would make the monitor
// unreachable for exactly the sessions that most need looking after.
func TestReadLaneRestoresLikeAnyRead(t *testing.T) {
	svc, stub, _ := newLaneHarness(t)
	ctx := context.Background()
	sess := newLaneManager(t, svc, "lane-restore")

	if err := stub.WriteLane(ctx, sess.Pod, session.LaneMonitor, "작업 레인의 말"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	res, err := svc.ReadLane(ctx, sess.ID, session.LaneMonitor, 0)
	if err != nil {
		t.Fatalf("read the monitor lane of a snapshot: %v", err)
	}
	if res.Session.State != session.StateActive {
		t.Fatalf("state after a lane read = %q, want it restored like any read", res.Session.State)
	}

	// The parent lane is the session's own read, so it answers identically to
	// Read — that is what keeps a lane-unaware caller correct.
	plain, err := svc.Read(ctx, sess.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	laned, err := svc.ReadLane(ctx, sess.ID, session.LaneParent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Payload != laned.Payload || plain.NextOffset != laned.NextOffset || plain.Path != laned.Path {
		t.Fatalf("Read = %+v but the parent lane = %+v; they must be one conversation", plain, laned)
	}
}

// TestStreamLaneStaysPassive: Stream does not promote idle, restore a snapshot
// or touch lastAccess, and naming a lane must not change that.
func TestStreamLaneStaysPassive(t *testing.T) {
	svc, _, _ := newLaneHarness(t)
	ctx := context.Background()
	sess := newLaneManager(t, svc, "lane-stream")

	stream, err := svc.StreamLane(ctx, sess.ID, session.LaneMonitor, 0)
	if err != nil {
		t.Fatalf("stream the monitor lane: %v", err)
	}
	stream.Close()

	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := svc.StreamLane(ctx, sess.ID, session.LaneMonitor, 0); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("stream a snapshot's lane = %v, want ErrInvalidState rather than a restore", err)
	}
	after, err := svc.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != session.StateSnapshot {
		t.Fatalf("state after streaming = %q, want it still snapshotted", after.State)
	}
}

// TestSessionLanesStampsTheSessionsPod is what makes "the pair is in one pod" a
// fact rather than an assertion: the lanes come from one pod's own answer, and
// the pod on each of them is the session's.
func TestSessionLanesStampsTheSessionsPod(t *testing.T) {
	svc, stub, _ := newLaneHarness(t)
	ctx := context.Background()
	sess := newLaneManager(t, svc, "lane-inventory")

	page, err := svc.SessionLanes(ctx, sess.ID)
	if err != nil {
		t.Fatalf("session lanes: %v", err)
	}
	if page.Pod != sess.Pod || !page.Paired || len(page.Lanes) != 2 {
		t.Fatalf("lanes = %+v, want the pair in pod %q", page, sess.Pod)
	}
	for _, lane := range page.Lanes {
		if lane.Pod != sess.Pod {
			t.Fatalf("lane %+v was not stamped with the session's pod %q", lane, sess.Pod)
		}
	}

	// A pod with no such route is unsupported, which a caller has to be able to
	// tell from a session that has no conversation at all.
	stub.LaneUnreachable(true)
	if _, err := svc.SessionLanes(ctx, sess.ID); !errors.Is(err, session.ErrLanesUnsupported) {
		t.Fatalf("lanes of a pod that predates the pair = %v, want ErrLanesUnsupported", err)
	}
}

// TestWriteTurnLaneIsRefusedForWhatHasNoSecondConversation: only a manager runs
// a second conversation, and a write that both names a lane and asks the owner
// to decide the turn's tool calls is two choices that cannot both hold.
func TestWriteTurnLaneIsRefusedForWhatHasNoSecondConversation(t *testing.T) {
	svc, stub, _ := newLaneHarness(t)
	ctx := context.Background()

	shell, err := svc.Create(ctx, session.CreateRequest{Name: "lane-shell", WorkloadType: session.WorkloadTypeShell})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WriteTurn(ctx, shell.ID, "echo hi",
		session.WriteOptions{Lane: session.LaneMonitor}); !errors.Is(err, session.ErrLanesUnsupported) {
		t.Fatalf("monitor lane on a shell session = %v, want ErrLanesUnsupported", err)
	}
	// Its own chat still writes exactly as it did.
	if _, err := svc.WriteTurn(ctx, shell.ID, "echo hi", session.WriteOptions{}); err != nil {
		t.Fatalf("a write with no lane on a shell session: %v", err)
	}

	manager := newLaneManager(t, svc, "lane-both-choices")
	if _, err := svc.WriteTurn(ctx, manager.ID, "x", session.WriteOptions{
		Lane: session.LaneMonitor, ToolApprovals: true,
	}); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatalf("lane + toolApprovals = %v, want ErrInvalidInput", err)
	}
	if writes := stub.LaneWrites(manager.Pod, session.LaneMonitor); len(writes) != 0 {
		t.Fatalf("a refused write still reached the monitor lane: %v", writes)
	}

	// And a write that does name only the lane lands on it.
	if _, err := svc.WriteTurn(ctx, manager.ID, "작업을 옮겨라",
		session.WriteOptions{Lane: session.LaneMonitor}); err != nil {
		t.Fatalf("write the monitor lane: %v", err)
	}
	writes := stub.LaneWrites(manager.Pod, session.LaneMonitor)
	if len(writes) != 1 || writes[0] != "작업을 옮겨라" {
		t.Fatalf("monitor lane received %v, want the one prompt addressed to it", writes)
	}
}
