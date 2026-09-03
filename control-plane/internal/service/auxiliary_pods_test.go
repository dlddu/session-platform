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

// These tests cover the half of AC-A2 that only became expressible once the
// session record stopped naming a single pod: a session owns one *workload*
// pod plus the session-scoped auxiliary pods that serve it (AC-F4's helper pod
// is the first such pod), and the lifecycle holds across that whole set —
// reclamation on freeze and terminate (AC-A3), re-provisioning on restore
// (AC-B2), and no leaks when provisioning fails part way.
//
// No workload type provisions an auxiliary pod yet: AC-F1's approval-gated type
// is not implemented, and shell/claude-code need none. The stub's
// SetAuxiliaryPods knob stands in for that future type so the contract is
// pinned before its first consumer arrives.

func newAuxService(aux int) (*service.Service, *k8s.StubOrchestrator, *configmap.Store) {
	orch := k8s.NewStubOrchestrator("sessions")
	orch.SetAuxiliaryPods(aux)
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	return service.New(orch, store, criu.NewStubCheckpointer(true), agent.NewStubClient()), orch, store
}

// TestCreateRecordsTheWholePodSet: creating a session with auxiliary pods
// records the workload pod as Pod and the rest as AuxiliaryPods, and Pods()
// returns the set workload-pod-first (AC-A2).
func TestCreateRecordsTheWholePodSet(t *testing.T) {
	ctx := context.Background()
	svc, orch, store := newAuxService(1)

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "with-helper"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Pod == "" {
		t.Fatal("create should record the workload pod")
	}
	if len(sess.AuxiliaryPods) != 1 {
		t.Fatalf("auxiliary pods = %v, want exactly 1 (AC-F4: one session-scoped helper)", sess.AuxiliaryPods)
	}
	if sess.AuxiliaryPods[0] == sess.Pod {
		t.Fatal("the auxiliary pod must be a distinct pod, not the workload pod again")
	}
	pods := sess.Pods()
	if len(pods) != 2 || pods[0] != sess.Pod || pods[1] != sess.AuxiliaryPods[0] {
		t.Fatalf("Pods() = %v, want [workload auxiliary]", pods)
	}
	if got := orch.RunningCount(); got != 2 {
		t.Fatalf("running pods = %d, want 2 (workload + auxiliary)", got)
	}

	// The set must survive the store round trip, or reclamation after a control
	// plane restart would only ever see the workload pod.
	stored, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(stored.AuxiliaryPods) != 1 || stored.AuxiliaryPods[0] != sess.AuxiliaryPods[0] {
		t.Fatalf("stored auxiliary pods = %v, want %v", stored.AuxiliaryPods, sess.AuxiliaryPods)
	}
}

// TestSnapshotReclaimsTheWholePodSet: freezing a session reclaims every pod it
// owns, not just the workload pod — AC-A3's reclamation and AC-F4's "the helper
// pod's lifetime is the session's" hold together. The record must forget both,
// so a snapshotted session names no pod at all.
func TestSnapshotReclaimsTheWholePodSet(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newAuxService(1)

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "freeze-helper"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	frozen, err := svc.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods after snapshot = %d, want 0 (AC-A3 covers the auxiliary pod too)", got)
	}
	if frozen.Pod != "" || len(frozen.AuxiliaryPods) != 0 {
		t.Fatalf("snapshotted session still names pods: pod=%q auxiliary=%v", frozen.Pod, frozen.AuxiliaryPods)
	}
	if len(frozen.Pods()) != 0 {
		t.Fatalf("Pods() on a snapshotted session = %v, want empty", frozen.Pods())
	}
}

// TestRestoreProvisionsAFreshPodSet: restoring brings back the whole set, with
// new names (AC-B2). The auxiliary pod holds no session state, so it is
// recreated rather than restored — but the session must end up owning it again.
func TestRestoreProvisionsAFreshPodSet(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newAuxService(1)

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "restore-helper"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	originalPods := sess.Pods()
	if _, err := svc.Snapshot(ctx, sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored, err := svc.Restore(ctx, sess.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := orch.RunningCount(); got != 2 {
		t.Fatalf("running pods after restore = %d, want 2 (workload + auxiliary)", got)
	}
	if len(restored.AuxiliaryPods) != 1 {
		t.Fatalf("restored auxiliary pods = %v, want exactly 1", restored.AuxiliaryPods)
	}
	for _, old := range originalPods {
		for _, now := range restored.Pods() {
			if old == now {
				t.Fatalf("restore reused the reclaimed pod name %q; every pod must be fresh (AC-B2)", old)
			}
		}
	}
}

// TestTerminateReclaimsTheWholePodSet: deleting a session reclaims every pod it
// owns (AC-A3). Reclaiming only the workload pod would leak the helper pod for
// the cluster's lifetime, since nothing else references it.
func TestTerminateReclaimsTheWholePodSet(t *testing.T) {
	ctx := context.Background()
	svc, orch, _ := newAuxService(1)

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "delete-helper"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Terminate(ctx, sess.ID); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Fatalf("running pods after terminate = %d, want 0 (AC-A3 covers the auxiliary pod too)", got)
	}
}

// TestFailedCreateReclaimsTheWholePodSet: a session that never becomes active
// must leave nothing behind. Reach fails here, and the rollback has to reclaim
// the auxiliary pods started alongside the unreachable workload pod — they are
// never recorded anywhere, so a leak here is unrecoverable.
func TestFailedCreateReclaimsTheWholePodSet(t *testing.T) {
	ctx := context.Background()
	stub := k8s.NewStubOrchestrator("sessions")
	stub.SetAuxiliaryPods(1)
	orch := &reachTrackingOrchestrator{
		StubOrchestrator: stub,
		reachErr:         errors.New("workload agent unreachable"),
	}
	svc := service.New(
		orch,
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
	)

	if _, err := svc.Create(ctx, session.CreateRequest{Name: "unreachable"}); !errors.Is(err, orch.reachErr) {
		t.Fatalf("create err = %v, want the reach failure", err)
	}
	if got := stub.RunningCount(); got != 0 {
		t.Fatalf("running pods after a failed create = %d, want 0 (the auxiliary pod leaked)", got)
	}
	if len(orch.stopped) != 2 {
		t.Fatalf("stopped pods = %v, want both the workload pod and its auxiliary pod", orch.stopped)
	}
}

// TestReachTargetsOnlyTheWorkloadPod: auxiliary pods run no session agent, so
// the AC-D1/AC-E1 readiness probe must dial the workload pod alone (AC-F4).
func TestReachTargetsOnlyTheWorkloadPod(t *testing.T) {
	ctx := context.Background()
	stub := k8s.NewStubOrchestrator("sessions")
	stub.SetAuxiliaryPods(2)
	orch := &reachTrackingOrchestrator{StubOrchestrator: stub}
	svc := service.New(
		orch,
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
	)

	if _, err := svc.Create(ctx, session.CreateRequest{Name: "one-probe"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if orch.reachCalls != 1 {
		t.Fatalf("reach calls = %d, want 1 — only the workload pod runs an agent", orch.reachCalls)
	}
}
