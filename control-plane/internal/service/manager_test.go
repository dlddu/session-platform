package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	storeport "github.com/dlddu/session-platform/control-plane/internal/store"
)

func newService() *service.Service {
	return service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
	)
}

// newServiceWithStore is like newService but also hands back the store, so a
// test can seed a non-active state without waiting for the background reaper;
// reaper timing and cutoff behavior are covered in reaper_test.go.
// The store is the real ConfigMap adapter over a fake clientset, so
// CompareAndSwapState behaves exactly as in production. The agent stub is
// returned too, so state-dispatch tests can assert the I/O that rode each
// branch (AC-D2/D3).
func newServiceWithStore() (*service.Service, *configmap.Store, *agent.StubClient) {
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ag := agent.NewStubClient()
	svc := service.New(k8s.NewStubOrchestrator("sessions"), store, criu.NewStubCheckpointer(true), ag)
	return svc, store, ag
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

// A disabled checkpoint strategy must fail closed. In particular, Snapshot
// must not reclaim the live pod or persist synthetic checkpoint metadata.
func TestSnapshotDisabledLeavesActiveSessionAndPodIntact(t *testing.T) {
	ctx := context.Background()
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	svc := service.New(orch, store, criu.NewStubCheckpointer(false), agent.NewStubClient())

	created, err := svc.Create(ctx, session.CreateRequest{Name: "checkpoint-disabled"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	originalPod := created.Pod

	frozen, err := svc.Snapshot(ctx, created.ID)
	if !errors.Is(err, session.ErrCheckpointDisabled) {
		t.Fatalf("snapshot err = %v, want ErrCheckpointDisabled", err)
	}
	if frozen != nil {
		t.Errorf("snapshot result = %+v, want nil on a disabled strategy", frozen)
	}

	stored, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after rejected snapshot: %v", err)
	}
	if stored.State != session.StateActive {
		t.Errorf("state = %q, want active", stored.State)
	}
	if stored.Pod != originalPod {
		t.Errorf("pod = %q, want original live pod %q", stored.Pod, originalPod)
	}
	if stored.Checkpoint != nil {
		t.Errorf("checkpoint = %+v, want nil", stored.Checkpoint)
	}
	if got := orch.RunningCount(); got != 1 {
		t.Errorf("running pods = %d, want 1", got)
	}
}

type abortTrackingCheckpointer struct {
	abortCalls           int
	abortPod             k8s.PodRef
	abortErr             error
	onAbort              func()
	abortRelease         <-chan struct{}
	abortToken           string
	checkpointCalls      int
	checkpointErr        error
	checkpointGeneration string
	onCheckpoint         func(string)
	waitCheckpointCancel bool
	restoreCalls         int
	onRestore            func()
	waitRestoreCancel    bool
}

func (*abortTrackingCheckpointer) Enabled() bool { return true }

func (*abortTrackingCheckpointer) Checkpoint(context.Context, k8s.PodRef) (*session.Checkpoint, error) {
	return &session.Checkpoint{Ref: "archive", AbortToken: "generation-1"}, nil
}

func (c *abortTrackingCheckpointer) CheckpointWithGeneration(ctx context.Context, _ k8s.PodRef, generation string) (*session.Checkpoint, error) {
	c.checkpointCalls++
	c.checkpointGeneration = generation
	if c.onCheckpoint != nil {
		c.onCheckpoint(generation)
	}
	if c.waitCheckpointCancel {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.checkpointErr != nil {
		return nil, c.checkpointErr
	}
	return &session.Checkpoint{Ref: "archive", AbortToken: generation}, nil
}

func (c *abortTrackingCheckpointer) Restore(ctx context.Context, _ *session.Checkpoint, _ k8s.PodRef) error {
	c.restoreCalls++
	if c.onRestore != nil {
		c.onRestore()
	}
	if c.waitRestoreCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (c *abortTrackingCheckpointer) AbortCheckpoint(_ context.Context, pod k8s.PodRef, cp *session.Checkpoint) error {
	c.abortCalls++
	c.abortPod = pod
	c.abortToken = cp.AbortToken
	if c.onAbort != nil {
		c.onAbort()
	}
	if c.abortRelease != nil {
		<-c.abortRelease
	}
	return c.abortErr
}

type controlledRenewStore struct {
	storeport.StateStore
	err     error
	release <-chan struct{}
	started chan struct{}
	once    sync.Once
}

func (s *controlledRenewStore) Renew(ctx context.Context, _, _ string) error {
	s.once.Do(func() {
		if s.started != nil {
			close(s.started)
		}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return s.err
	}
}

type unlockContextStore struct {
	storeport.StateStore
	result chan error
}

func (s *unlockContextStore) Unlock(ctx context.Context, id, token string) error {
	err := s.StateStore.Unlock(ctx, id, token)
	s.result <- errors.Join(ctx.Err(), err)
	return err
}

// commitThenErrorStore models the outcome-ambiguous Kubernetes update case:
// the aggregate session CAS is durably committed, but the caller loses the
// response. Restore must confirm the authoritative record instead of deleting
// the pod that record now names.
type commitThenErrorStore struct {
	storeport.StateStore
	err error
}

func (s *commitThenErrorStore) CompareAndSwapSession(
	ctx context.Context,
	id, token string,
	expectedState session.State,
	expectedTxn *session.SnapshotTransaction,
	next *session.Session,
) error {
	if err := s.StateStore.CompareAndSwapSession(ctx, id, token, expectedState, expectedTxn, next); err != nil {
		return err
	}
	if next.State == session.StateActive {
		return s.err
	}
	return nil
}

// deleteBeforeRestoreCASStore models metadata disappearing after RestoreInto
// succeeds but before the restored pod can be committed to the aggregate.
type deleteBeforeRestoreCASStore struct {
	storeport.StateStore
}

func (s *deleteBeforeRestoreCASStore) CompareAndSwapSession(
	ctx context.Context,
	id, token string,
	expectedState session.State,
	expectedTxn *session.SnapshotTransaction,
	next *session.Session,
) error {
	if expectedState == session.StateSnapshot && next.State == session.StateActive {
		if err := s.StateStore.Delete(ctx, id, token); err != nil {
			return err
		}
		return session.ErrNotFound
	}
	return s.StateStore.CompareAndSwapSession(
		ctx, id, token, expectedState, expectedTxn, next,
	)
}

type legacyCheckpointer struct{ criu.Checkpointer }

type recoveryAbortAgent struct {
	*agent.StubClient
	abortCalls int
	abortPod   string
	abortToken string
}

func (a *recoveryAbortAgent) AbortCheckpoint(_ context.Context, pod, generation string) error {
	a.abortCalls++
	a.abortPod = pod
	a.abortToken = generation
	return nil
}

func TestSnapshotStopFailureLeavesDurableCommitForRecovery(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := &abortTrackingCheckpointer{}
	svc := service.New(
		orch,
		store,
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)

	created, err := svc.Create(ctx, session.CreateRequest{
		Name: "archive-stop-failure", WorkloadType: session.WorkloadTypeClaudeCode,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orch.stopErr = errors.New("delete pod failed")

	if _, err := svc.Snapshot(ctx, created.ID); !errors.Is(err, orch.stopErr) {
		t.Fatalf("snapshot err = %v, want stop error", err)
	}
	if ckpt.abortCalls != 0 {
		t.Fatalf("abort calls = %d, want 0 after durable commit decision", ckpt.abortCalls)
	}
	stored, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after failed stop: %v", err)
	}
	if stored.State != session.StateActive || stored.Pod != created.Pod || stored.SnapshotTransaction == nil {
		t.Fatalf("stored after failed stop = %+v, want active pod plus pending commit", stored)
	}
	if stored.SnapshotTransaction.Phase != session.SnapshotPhaseCommitting ||
		stored.SnapshotTransaction.Checkpoint == nil || stored.SnapshotTransaction.Checkpoint.Ref != "archive" {
		t.Fatalf("pending transaction = %+v, want durable committing archive", stored.SnapshotTransaction)
	}

	// A new control-plane process can re-read the commit record, retry the
	// idempotent delete, and finish without another checkpoint or stale abort.
	orch.stopErr = nil
	restarted := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	frozen, err := restarted.Snapshot(ctx, created.ID)
	if err != nil {
		t.Fatalf("recover snapshot: %v", err)
	}
	if frozen.State != session.StateSnapshot || frozen.Pod != "" || frozen.Checkpoint == nil {
		t.Fatalf("recovered snapshot = %+v", frozen)
	}
	if ckpt.checkpointCalls != 1 {
		t.Fatalf("checkpoint calls = %d, want original call only", ckpt.checkpointCalls)
	}
	restored, err := restarted.Restore(ctx, created.ID)
	if err != nil {
		t.Fatalf("restore recovered snapshot: %v", err)
	}
	if restored.Model != session.PlatformDefaultModel || orch.ModelFor(created.ID) != session.PlatformDefaultModel {
		t.Fatalf("restored model = %q, orchestrator = %q, want platform default", restored.Model, orch.ModelFor(created.ID))
	}
}

func TestSnapshotPersistsPrepareAndCommitBeforeSideEffects(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := &abortTrackingCheckpointer{}
	svc := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	created, err := svc.Create(ctx, session.CreateRequest{Name: "durable-order", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	ckpt.onCheckpoint = func(generation string) {
		stored, getErr := store.Get(ctx, created.ID)
		if getErr != nil {
			t.Errorf("get preparing transaction: %v", getErr)
			return
		}
		txn := stored.SnapshotTransaction
		if txn == nil || txn.Phase != session.SnapshotPhasePreparing || txn.Generation != generation || txn.SourcePod != created.Pod {
			t.Errorf("transaction at checkpoint = %+v, want durable prepare", txn)
		}
		if len(generation) != 32 {
			t.Errorf("generation length = %d, want 32 hex chars", len(generation))
		}
	}
	orch.beforeStop = func() {
		stored, getErr := store.Get(ctx, created.ID)
		if getErr != nil {
			t.Errorf("get committing transaction: %v", getErr)
			return
		}
		txn := stored.SnapshotTransaction
		if txn == nil || txn.Phase != session.SnapshotPhaseCommitting || txn.Checkpoint == nil || txn.Checkpoint.Ref != "archive" {
			t.Errorf("transaction at stop = %+v, want durable commit", txn)
		}
	}

	frozen, err := svc.Snapshot(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.SnapshotTransaction != nil {
		t.Fatalf("completed snapshot retained transaction: %+v", frozen.SnapshotTransaction)
	}
}

func TestPreparingSnapshotRecoversAfterControlPlaneRestart(t *testing.T) {
	ctx := context.Background()
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := &abortTrackingCheckpointer{}
	svc := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	created, err := svc.Create(ctx, session.CreateRequest{Name: "prepare-crash", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	created.SnapshotTransaction = &session.SnapshotTransaction{
		Generation: generation,
		SourcePod:  created.Pod,
		Phase:      session.SnapshotPhasePreparing,
	}
	if err := store.Put(ctx, created); err != nil {
		t.Fatal(err)
	}

	restarted := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	if _, err := restarted.Write(ctx, created.ID, "after restart"); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
	if ckpt.abortCalls != 1 || ckpt.abortPod.Name != created.Pod || ckpt.abortToken != generation {
		t.Fatalf("abort = (%d,%q,%q), want exact durable generation", ckpt.abortCalls, ckpt.abortPod.Name, ckpt.abortToken)
	}
	stored, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateActive || stored.Pod != created.Pod || stored.SnapshotTransaction != nil || orch.RunningCount() != 1 {
		t.Fatalf("recovered preparing session = %+v, running=%d", stored, orch.RunningCount())
	}
}

func TestCheckpointFailureAbortsAndClearsDurablePrepare(t *testing.T) {
	ctx := context.Background()
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := &abortTrackingCheckpointer{checkpointErr: errors.New("s3 unavailable")}
	svc := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	created, err := svc.Create(ctx, session.CreateRequest{Name: "archive-failure", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(ctx, created.ID); !errors.Is(err, ckpt.checkpointErr) {
		t.Fatalf("snapshot err = %v, want checkpoint error", err)
	}
	stored, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ckpt.abortCalls != 1 || ckpt.abortToken != ckpt.checkpointGeneration {
		t.Fatalf("abort = (%d,%q), checkpoint generation=%q", ckpt.abortCalls, ckpt.abortToken, ckpt.checkpointGeneration)
	}
	if stored.SnapshotTransaction != nil || stored.State != session.StateActive || stored.Pod != created.Pod || orch.RunningCount() != 1 {
		t.Fatalf("failed checkpoint mutated live session: %+v running=%d", stored, orch.RunningCount())
	}
}

func TestCommittingSnapshotRecoversCrashBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stateCASOnly bool
	}{
		{name: "after Stop before finalize"},
		{name: "after state CAS before aggregate finalize", stateCASOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			orch := k8s.NewStubOrchestrator("sessions")
			store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
			ckpt := &abortTrackingCheckpointer{}
			creator := service.New(
				orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
				service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
			)
			created, err := creator.Create(ctx, session.CreateRequest{
				Name: tc.name, WorkloadType: session.WorkloadTypeClaudeCode,
			})
			if err != nil {
				t.Fatal(err)
			}
			created.SnapshotTransaction = &session.SnapshotTransaction{
				Generation: "0123456789abcdef0123456789abcdef",
				Owner:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SourcePod:  created.Pod,
				Phase:      session.SnapshotPhaseCommitting,
				Checkpoint: &session.Checkpoint{Ref: "s3://bucket/archive.tar"},
			}
			if err := store.Put(ctx, created); err != nil {
				t.Fatal(err)
			}
			if err := orch.Stop(ctx, k8s.PodRef{Name: created.Pod, Namespace: "sessions"}); err != nil {
				t.Fatal(err)
			}
			if tc.stateCASOnly {
				if err := store.CompareAndSwapState(
					ctx, created.ID, session.StateActive, session.StateSnapshot,
				); err != nil {
					t.Fatal(err)
				}
			}

			restarted := service.New(
				orch, store, criu.NewStubCheckpointer(false), agent.NewStubClient(),
				service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
			)
			frozen, err := restarted.Snapshot(ctx, created.ID)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if frozen.State != session.StateSnapshot || frozen.Pod != "" ||
				frozen.Checkpoint == nil || frozen.Checkpoint.Ref != "s3://bucket/archive.tar" ||
				frozen.SnapshotTransaction != nil {
				t.Fatalf("recovered snapshot = %+v", frozen)
			}
			if ckpt.checkpointCalls != 0 || ckpt.abortCalls != 0 {
				t.Fatalf("recovery checkpoint/abort calls = %d/%d, want 0/0", ckpt.checkpointCalls, ckpt.abortCalls)
			}
			if orch.RunningCount() != 0 {
				t.Fatalf("running pods = %d, want 0", orch.RunningCount())
			}
		})
	}
}

func TestPreparingRecoveryOwnerClaimFencesExpiredHolderCommit(t *testing.T) {
	ctx := context.Background()
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	abortEntered := make(chan struct{})
	abortRelease := make(chan struct{})
	var abortOnce sync.Once
	ckpt := &abortTrackingCheckpointer{
		onAbort:      func() { abortOnce.Do(func() { close(abortEntered) }) },
		abortRelease: abortRelease,
	}
	creator := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	created, err := creator.Create(ctx, session.CreateRequest{
		Name: "owner-fence", WorkloadType: session.WorkloadTypeClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	created.SnapshotTransaction = &session.SnapshotTransaction{
		Generation: "0123456789abcdef0123456789abcdef",
		Owner:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourcePod:  created.Pod,
		Phase:      session.SnapshotPhasePreparing,
	}
	if err := store.Put(ctx, created); err != nil {
		t.Fatal(err)
	}

	restarted := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	writeDone := make(chan error, 1)
	go func() {
		_, err := restarted.Write(ctx, created.ID, "after recovery")
		writeDone <- err
	}()
	select {
	case <-abortEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not reach abort after owner claim")
	}
	claimed, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.SnapshotTransaction == nil ||
		claimed.SnapshotTransaction.Owner == created.SnapshotTransaction.Owner {
		t.Fatalf("prepare was not claimed by new owner: %+v", claimed.SnapshotTransaction)
	}

	expected := *created.SnapshotTransaction
	committing := expected
	committing.Phase = session.SnapshotPhaseCommitting
	committing.Checkpoint = &session.Checkpoint{Ref: "s3://bucket/stale.tar"}
	staleNext := *created
	staleNext.SnapshotTransaction = &committing
	if err := store.CompareAndSwapSession(
		ctx, created.ID, expected.Owner, session.StateActive, &expected, &staleNext,
	); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("expired owner commit err=%v, want ErrConflict", err)
	}
	close(abortRelease)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("recovered write: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered write did not finish")
	}
	stored, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateActive || stored.Pod != created.Pod || stored.SnapshotTransaction != nil {
		t.Fatalf("owner-fenced recovery result = %+v", stored)
	}
}

func TestHungLeaseRenewCancelsPrepareBeforeCommitSideEffects(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	baseStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	started := make(chan struct{})
	store := &controlledRenewStore{StateStore: baseStore, started: started}
	ckpt := &abortTrackingCheckpointer{waitCheckpointCancel: true}
	svc := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
		service.WithLeaseRenewInterval(2*time.Millisecond),
	)
	created, err := svc.Create(ctx, session.CreateRequest{
		Name: "hung-renew", WorkloadType: session.WorkloadTypeClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(ctx, created.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("snapshot err=%v, want heartbeat deadline", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("Renew was not called")
	}
	stored, err := baseStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SnapshotTransaction == nil ||
		stored.SnapshotTransaction.Phase != session.SnapshotPhasePreparing ||
		stored.State != session.StateActive || stored.Pod != created.Pod {
		t.Fatalf("renew timeout did not preserve prepare: %+v", stored)
	}
	if len(orch.stopped) != 0 || ckpt.abortCalls != 0 {
		t.Fatalf("stop/abort calls = %d/%d, want 0/0", len(orch.stopped), ckpt.abortCalls)
	}

	ckpt.waitCheckpointCancel = false
	restarted := service.New(
		orch, baseStore, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	if _, err := restarted.Write(ctx, created.ID, "resume"); err != nil {
		t.Fatalf("recover timed-out prepare: %v", err)
	}
	if ckpt.abortCalls != 1 {
		t.Fatalf("recovery abort calls=%d, want 1", ckpt.abortCalls)
	}
}

func TestCanceledSnapshotUsesFreshUnlockContext(t *testing.T) {
	ctx := context.Background()
	baseStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	store := &unlockContextStore{StateStore: baseStore, result: make(chan error, 1)}
	entered := make(chan struct{})
	var enteredOnce sync.Once
	ckpt := &abortTrackingCheckpointer{
		waitCheckpointCancel: true,
		onCheckpoint: func(string) {
			enteredOnce.Do(func() { close(entered) })
		},
	}
	svc := service.New(
		k8s.NewStubOrchestrator("sessions"), store,
		criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	created, err := svc.Create(ctx, session.CreateRequest{
		Name: "cancel-unlock", WorkloadType: session.WorkloadTypeClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshotCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, snapshotErr := svc.Snapshot(snapshotCtx, created.ID)
		done <- snapshotErr
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot err=%v, want context.Canceled", err)
	}
	if err := <-store.result; err != nil {
		t.Fatalf("unlock used canceled context or failed: %v", err)
	}
	if err := baseStore.Lock(ctx, created.ID, "next-holder"); err != nil {
		t.Fatalf("lease remained after canceled snapshot: %v", err)
	}
	_ = baseStore.Unlock(ctx, created.ID, "next-holder")
}

func TestLeaseRenewFailureCancelsStopAndLeavesCommitRecoverable(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{
		StubOrchestrator: k8s.NewStubOrchestrator("sessions"),
		waitStopCancel:   true,
	}
	baseStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	release := make(chan struct{})
	renewFailure := errors.New("lease ownership lost")
	store := &controlledRenewStore{StateStore: baseStore, err: renewFailure, release: release}
	var releaseOnce sync.Once
	orch.beforeStop = func() { releaseOnce.Do(func() { close(release) }) }
	ckpt := &abortTrackingCheckpointer{}
	svc := service.New(
		orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
		service.WithLeaseRenewInterval(time.Millisecond),
	)
	created, err := svc.Create(ctx, session.CreateRequest{
		Name: "commit-renew-loss", WorkloadType: session.WorkloadTypeClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(ctx, created.ID); !errors.Is(err, renewFailure) {
		t.Fatalf("snapshot err=%v, want renewal failure", err)
	}
	stored, err := baseStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SnapshotTransaction == nil ||
		stored.SnapshotTransaction.Phase != session.SnapshotPhaseCommitting ||
		stored.SnapshotTransaction.Checkpoint == nil ||
		stored.State != session.StateActive || stored.Pod != created.Pod {
		t.Fatalf("renew loss did not preserve committing txn: %+v", stored)
	}
	if ckpt.abortCalls != 0 {
		t.Fatalf("commit renewal loss made %d aborts, want 0", ckpt.abortCalls)
	}

	orch.beforeStop = nil
	orch.waitStopCancel = false
	restarted := service.New(
		orch, baseStore, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	frozen, err := restarted.Snapshot(ctx, created.ID)
	if err != nil {
		t.Fatalf("recover commit: %v", err)
	}
	if frozen.State != session.StateSnapshot || frozen.SnapshotTransaction != nil {
		t.Fatalf("recovered commit = %+v", frozen)
	}
	if ckpt.checkpointCalls != 1 || ckpt.abortCalls != 0 {
		t.Fatalf("checkpoint/abort calls=%d/%d, want 1/0", ckpt.checkpointCalls, ckpt.abortCalls)
	}
}

func TestRestoreLeaseRenewFailureCancelsTransferAndKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	baseStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := &abortTrackingCheckpointer{}
	normal := service.New(
		orch, baseStore, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	created, err := normal.Create(ctx, session.CreateRequest{
		Name: "restore-renew-loss", WorkloadType: session.WorkloadTypeClaudeCode,
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := normal.Snapshot(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	renewFailure := errors.New("restore lease lost")
	wrapped := &controlledRenewStore{
		StateStore: baseStore,
		err:        renewFailure,
		release:    release,
	}
	var releaseOnce sync.Once
	ckpt.onRestore = func() { releaseOnce.Do(func() { close(release) }) }
	ckpt.waitRestoreCancel = true
	restorer := service.New(
		orch, wrapped, criu.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
		service.WithLeaseRenewInterval(time.Millisecond),
	)
	if _, err := restorer.Restore(ctx, created.ID); !errors.Is(err, renewFailure) {
		t.Fatalf("restore err=%v, want renewal failure", err)
	}
	stored, err := baseStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateSnapshot || stored.Pod != "" ||
		stored.Checkpoint == nil || stored.Checkpoint.Ref != frozen.Checkpoint.Ref ||
		stored.SnapshotTransaction != nil {
		t.Fatalf("failed restore corrupted snapshot: %+v", stored)
	}
	if ckpt.restoreCalls != 1 {
		t.Fatalf("restore calls=%d, want 1", ckpt.restoreCalls)
	}
}

func TestRestoreTreatsCommittedCASWithLostResponseAsSuccess(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	baseStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	normal := service.New(orch, baseStore, criu.NewStubCheckpointer(true), agent.NewStubClient())

	created, err := normal.Create(ctx, session.CreateRequest{Name: "restore-commit-response-lost"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normal.Snapshot(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	stopsBeforeRestore := len(orch.stopped)
	injected := errors.New("update response lost")
	restorer := service.New(
		orch,
		&commitThenErrorStore{StateStore: baseStore, err: injected},
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
	)

	restored, err := restorer.Restore(ctx, created.ID)
	if err != nil {
		t.Fatalf("restore returned committed CAS response error: %v", err)
	}
	if restored.State != session.StateActive || restored.Pod == "" || restored.Checkpoint != nil {
		t.Fatalf("restored session = %+v", restored)
	}
	stored, err := baseStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateActive || stored.Pod != restored.Pod ||
		stored.Checkpoint != nil || stored.SnapshotTransaction != nil {
		t.Fatalf("authoritative session = %+v, restored = %+v", stored, restored)
	}
	if got := len(orch.stopped) - stopsBeforeRestore; got != 0 {
		t.Fatalf("restore cleaned up committed pod %d time(s), want 0", got)
	}
	if got := orch.RunningCount(); got != 1 {
		t.Fatalf("running pods = %d, want committed restore pod", got)
	}
}

func TestRestoreCleansPodWhenRecordDisappearsBeforeFinalCAS(t *testing.T) {
	ctx := context.Background()
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	baseStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	normal := service.New(orch, baseStore, criu.NewStubCheckpointer(true), agent.NewStubClient())

	created, err := normal.Create(ctx, session.CreateRequest{Name: "restore-record-deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normal.Snapshot(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	stopsBeforeRestore := len(orch.stopped)
	restorer := service.New(
		orch,
		&deleteBeforeRestoreCASStore{StateStore: baseStore},
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
	)

	if _, err := restorer.Restore(ctx, created.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("restore err = %v, want ErrNotFound", err)
	}
	if got := len(orch.stopped) - stopsBeforeRestore; got != 1 {
		t.Fatalf("restore cleaned up %d pods, want 1", got)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods after record deletion = %d, want 0", got)
	}
	if _, err := baseStore.Get(ctx, created.ID); err != session.ErrNotFound {
		t.Fatalf("get after record deletion err = %v, want ErrNotFound", err)
	}
}

func TestClaudeLegacyStrategyFailsClosedAndGateOffRecoveryStillAborts(t *testing.T) {
	ctx := context.Background()
	t.Run("legacy strategy", func(t *testing.T) {
		orch := k8s.NewStubOrchestrator("sessions")
		store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
		svc := service.New(
			orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient(),
			service.WithWorkloadCheckpointer(
				session.WorkloadTypeClaudeCode,
				legacyCheckpointer{Checkpointer: criu.NewStubCheckpointer(true)},
			),
		)
		created, err := svc.Create(ctx, session.CreateRequest{
			Name: "legacy", WorkloadType: session.WorkloadTypeClaudeCode,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Snapshot(ctx, created.ID); err == nil {
			t.Fatal("legacy Claude strategy unexpectedly snapshotted")
		}
		stored, _ := store.Get(ctx, created.ID)
		if stored.State != session.StateActive || stored.Pod != created.Pod ||
			stored.SnapshotTransaction != nil || orch.RunningCount() != 1 {
			t.Fatalf("legacy fail-closed result=%+v running=%d", stored, orch.RunningCount())
		}
	})

	t.Run("gate off recovery", func(t *testing.T) {
		orch := k8s.NewStubOrchestrator("sessions")
		store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
		creator := service.New(orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient())
		created, err := creator.Create(ctx, session.CreateRequest{
			Name: "gate-off-recovery", WorkloadType: session.WorkloadTypeClaudeCode,
		})
		if err != nil {
			t.Fatal(err)
		}
		const generation = "0123456789abcdef0123456789abcdef"
		created.SnapshotTransaction = &session.SnapshotTransaction{
			Generation: generation,
			Owner:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SourcePod:  created.Pod,
			Phase:      session.SnapshotPhasePreparing,
		}
		if err := store.Put(ctx, created); err != nil {
			t.Fatal(err)
		}
		ag := &recoveryAbortAgent{StubClient: agent.NewStubClient()}
		restarted := service.New(orch, store, criu.NewStubCheckpointer(false), ag)
		if _, err := restarted.Write(ctx, created.ID, "recover"); err != nil {
			t.Fatalf("gate-off recovery: %v", err)
		}
		if ag.abortCalls != 1 || ag.abortPod != created.Pod || ag.abortToken != generation {
			t.Fatalf("fallback abort=(%d,%q,%q)", ag.abortCalls, ag.abortPod, ag.abortToken)
		}
	})
}

// TestReadDispatchesOnState covers the uniform resume-on-access read policy
// (AC-C2): active serves in place, idle is promoted to active, snapshot is
// restored to active — and in every non-active case the session ends active.
func TestReadDispatchesOnState(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newServiceWithStore()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "notebook-alpha"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// active: served directly.
	res, err := svc.Read(ctx, sess.ID, 0)
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
	res, err = svc.Read(ctx, sess.ID, 0)
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
	res, err = svc.Read(ctx, sess.ID, 0)
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
	svc, store, _ := newServiceWithStore()

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

// TestReadWriteMapToAgentIO: write forwards the payload to the session's pod
// through the AgentClient (AC-D2) and read returns the offset-cursored delta
// of what accumulated there with a nextOffset cursor (AC-D3) — across state
// branches, since idle is promoted before serving.
func TestReadWriteMapToAgentIO(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newServiceWithStore()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "shell-io"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Write(ctx, sess.ID, "echo A\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := svc.Read(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Payload != "echo A\n" {
		t.Fatalf("read payload = %q, want the written payload back from the agent", res.Payload)
	}
	if res.NextOffset != int64(len("echo A\n")) {
		t.Fatalf("nextOffset = %d, want %d", res.NextOffset, len("echo A\n"))
	}

	// Cursor read with no new output: empty delta, cursor unchanged.
	res2, err := svc.Read(ctx, sess.ID, res.NextOffset)
	if err != nil {
		t.Fatalf("cursor read: %v", err)
	}
	if res2.Payload != "" || res2.NextOffset != res.NextOffset {
		t.Fatalf("cursor read = (%q, %d), want empty delta at cursor %d", res2.Payload, res2.NextOffset, res.NextOffset)
	}

	// The idle branch serves I/O too (uniform resume-on-access): promote is
	// followed by the same agent write against the still-held pod.
	if err := store.CompareAndSwapState(ctx, sess.ID, session.StateActive, session.StateIdle); err != nil {
		t.Fatalf("force idle: %v", err)
	}
	if _, err := svc.Write(ctx, sess.ID, "echo B\n"); err != nil {
		t.Fatalf("idle write: %v", err)
	}
	res3, err := svc.Read(ctx, sess.ID, res.NextOffset)
	if err != nil {
		t.Fatalf("delta read: %v", err)
	}
	if res3.Payload != "echo B\n" {
		t.Fatalf("delta read = %q, want only the new output (AC-D3)", res3.Payload)
	}

	// offset 0 still replays the whole history, and nothing is a stub.
	full, err := svc.Read(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("full read: %v", err)
	}
	if full.Payload != "echo A\necho B\n" {
		t.Fatalf("full read = %q, want the ordered full history", full.Payload)
	}
}

// failingAgent errors every shell I/O call, to prove Read/Write surface agent
// failures instead of swallowing them.
type failingAgent struct{ err error }

func (f failingAgent) Write(context.Context, string, string) error { return f.err }
func (f failingAgent) Read(context.Context, string, int64) (string, int64, error) {
	return "", 0, f.err
}

func TestReadWriteSurfaceAgentErrors(t *testing.T) {
	ctx := context.Background()
	svc := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(true),
		failingAgent{err: errors.New("agent unreachable")},
	)
	sess, err := svc.Create(ctx, session.CreateRequest{Name: "broken-io"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Write(ctx, sess.ID, "x"); err == nil {
		t.Fatal("write succeeded despite agent failure")
	}
	if _, err := svc.Read(ctx, sess.ID, 0); err == nil {
		t.Fatal("read succeeded despite agent failure")
	}
}

// reachTrackingOrchestrator wraps the stub to observe/steer the AC-D1 shell
// reachability verification: it counts Reach calls, can fail them, and records
// which pods were stopped.
type reachTrackingOrchestrator struct {
	*k8s.StubOrchestrator
	reachCalls     int
	reachErr       error
	stopped        []k8s.PodRef
	stopErr        error
	beforeStop     func()
	waitStopCancel bool
}

func (o *reachTrackingOrchestrator) Reach(context.Context, k8s.PodRef) error {
	o.reachCalls++
	return o.reachErr
}

func (o *reachTrackingOrchestrator) Stop(ctx context.Context, ref k8s.PodRef) error {
	o.stopped = append(o.stopped, ref)
	if o.beforeStop != nil {
		o.beforeStop()
	}
	if o.waitStopCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if o.stopErr != nil {
		return o.stopErr
	}
	return o.StubOrchestrator.Stop(ctx, ref)
}

func newTrackedService() (*service.Service, *reachTrackingOrchestrator, *configmap.Store) {
	orch := &reachTrackingOrchestrator{StubOrchestrator: k8s.NewStubOrchestrator("sessions")}
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	return service.New(orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient()), orch, store
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

// Termination shares the lifecycle Lease with snapshot/restore. A competing
// holder must make deletion fail before the live pod or metadata is touched.
func TestTerminateConflictsWithLifecycleLock(t *testing.T) {
	ctx := context.Background()
	svc, orch, stateStore := newTrackedService()

	created, err := svc.Create(ctx, session.CreateRequest{Name: "locked-session"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := stateStore.Lock(ctx, created.ID, "snapshot-owner"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer stateStore.Unlock(ctx, created.ID, "snapshot-owner")

	if err := svc.Terminate(ctx, created.ID); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("terminate while locked err = %v, want ErrConflict", err)
	}
	if len(orch.stopped) != 0 {
		t.Fatalf("stopped pods = %d, want 0 while another lifecycle operation holds the Lease", len(orch.stopped))
	}
	if _, err := svc.Get(ctx, created.ID); err != nil {
		t.Fatalf("session was removed despite lock conflict: %v", err)
	}
}

// A blocked pod deletion must keep the lifecycle Lease alive instead of
// allowing another control-plane replica to take it over mid-termination.
func TestTerminateRenewsLeaseWhileStoppingPod(t *testing.T) {
	ctx := context.Background()
	stateStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	renewStarted := make(chan struct{})
	renewRelease := make(chan struct{})
	store := &controlledRenewStore{
		StateStore: stateStore,
		started:    renewStarted,
		release:    renewRelease,
	}
	orch := &reachTrackingOrchestrator{
		StubOrchestrator: k8s.NewStubOrchestrator("sessions"),
	}
	orch.beforeStop = func() {
		select {
		case <-renewStarted:
			close(renewRelease)
		case <-time.After(time.Second):
			t.Fatal("termination never renewed its lifecycle Lease")
		}
	}
	svc := service.New(
		orch,
		store,
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
		service.WithLeaseRenewInterval(50*time.Millisecond),
	)

	created, err := svc.Create(ctx, session.CreateRequest{Name: "slow-delete"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Terminate(ctx, created.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if len(orch.stopped) != 1 {
		t.Fatalf("stopped pods = %d, want 1", len(orch.stopped))
	}
	if _, err := stateStore.Get(ctx, created.ID); err != session.ErrNotFound {
		t.Fatalf("get after terminate err = %v, want ErrNotFound", err)
	}
}

// Losing the Lease cancels an in-flight Stop and preserves metadata so a new
// holder can retry deletion safely.
func TestTerminateLeaseLossPreservesSession(t *testing.T) {
	ctx := context.Background()
	stateStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	renewRelease := make(chan struct{})
	close(renewRelease)
	store := &controlledRenewStore{
		StateStore: stateStore,
		err:        session.ErrConflict,
		release:    renewRelease,
	}
	orch := &reachTrackingOrchestrator{
		StubOrchestrator: k8s.NewStubOrchestrator("sessions"),
		waitStopCancel:   true,
	}
	svc := service.New(
		orch,
		store,
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
		service.WithLeaseRenewInterval(time.Millisecond),
	)

	created, err := svc.Create(ctx, session.CreateRequest{Name: "lost-delete"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Terminate(ctx, created.ID); !errors.Is(err, session.ErrConflict) {
		t.Fatalf("terminate after Lease loss err = %v, want ErrConflict", err)
	}
	if _, err := stateStore.Get(ctx, created.ID); err != nil {
		t.Fatalf("session removed after Lease loss: %v", err)
	}
	if got := orch.RunningCount(); got != 1 {
		t.Fatalf("running pods after cancelled Stop = %d, want 1", got)
	}
}

// Deletion is a terminal cleanup path: it owner-fences a crashed snapshot
// transaction and reclaims both recorded pod references without depending on
// the workload's recovery/abort endpoint.
func TestTerminateClaimsInFlightSnapshotAndReclaimsPods(t *testing.T) {
	ctx := context.Background()
	stateStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	orch := &reachTrackingOrchestrator{
		StubOrchestrator: k8s.NewStubOrchestrator("sessions"),
	}
	checkpointer := &abortTrackingCheckpointer{
		abortErr: errors.New("agent cannot reopen admission"),
	}
	svc := service.New(orch, stateStore, checkpointer, agent.NewStubClient())

	created, err := svc.Create(ctx, session.CreateRequest{Name: "stuck-snapshot"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, err := stateStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get created: %v", err)
	}
	stored.SnapshotTransaction = &session.SnapshotTransaction{
		Generation: "crashed-generation",
		Owner:      "crashed-owner",
		SourcePod:  "orphaned-source-pod",
		Phase:      session.SnapshotPhasePreparing,
	}
	if err := stateStore.Put(ctx, stored); err != nil {
		t.Fatalf("seed snapshot transaction: %v", err)
	}

	var ownerAtStop string
	orch.beforeStop = func() {
		current, getErr := stateStore.Get(ctx, created.ID)
		if getErr != nil {
			t.Fatalf("get during Stop: %v", getErr)
		}
		ownerAtStop = current.SnapshotTransaction.Owner
	}
	if err := svc.Terminate(ctx, created.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if ownerAtStop == "" || ownerAtStop == "crashed-owner" {
		t.Fatalf("snapshot owner at Stop = %q, want a newly claimed fencing token", ownerAtStop)
	}
	if checkpointer.abortCalls != 0 {
		t.Fatalf("abort calls = %d, want 0 for terminal deletion", checkpointer.abortCalls)
	}
	if len(orch.stopped) != 2 {
		t.Fatalf("stopped pods = %d, want current and source pods", len(orch.stopped))
	}
	if _, err := stateStore.Get(ctx, created.ID); err != session.ErrNotFound {
		t.Fatalf("get after terminate err = %v, want ErrNotFound", err)
	}
}
