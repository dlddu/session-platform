package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// These tests pin the renewal contract a restore round depends on: an
// unconfirmed renewal is retried inside the validity already held, a
// demonstrated loss of ownership stops the round at once, and a round that
// stops never reclaims anything but the pods it provisioned itself.
//
// The regression they close is a real one. On 2026-11-17 a single lease GET
// exceeded its budget while a restore waited out a node scale-up; the round
// read that one failure as a lost lease, cancelled itself, and deleted the pod
// it had spent two minutes provisioning. The session survived only because the
// archive was untouched and a human retried.

// gateOrchestrator holds RestoreInto open so a test can let the renewal loop
// run while a round is mid-flight, which is where the incident happened.
type gateOrchestrator struct {
	k8s.PodOrchestrator
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGateOrchestrator(inner k8s.PodOrchestrator) *gateOrchestrator {
	return &gateOrchestrator{PodOrchestrator: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

func (o *gateOrchestrator) RestoreInto(ctx context.Context, sessionID, ref string, w k8s.WorkloadSpec) (k8s.SessionPods, error) {
	pods, err := o.PodOrchestrator.RestoreInto(ctx, sessionID, ref, w)
	if err != nil {
		return pods, err
	}
	o.once.Do(func() { close(o.entered) })
	select {
	case <-o.release:
	case <-ctx.Done():
		// provision() reclaims the pod it created when the wait it was in ends
		// badly; the double has to do the same or it reports leaks the real
		// orchestrator would never produce.
		_ = o.PodOrchestrator.Stop(context.Background(), pods.All()...)
		return k8s.SessionPods{}, ctx.Err()
	}
	return pods, nil
}

type leaseHarness struct {
	svc    *service.Service
	store  *configmap.Store
	orch   *k8s.StubOrchestrator
	gate   *gateOrchestrator
	client *fake.Clientset
}

// newLeaseHarness wires the real ConfigMap store over a fake API server so the
// Lease semantics under test are the adapter's own, not a mock's.
func newLeaseHarness(t *testing.T, leaseDur, renewEvery time.Duration) *leaseHarness {
	t.Helper()
	client := fake.NewSimpleClientset()
	st := configmap.NewStore(client, "sessions", configmap.WithLeaseDuration(leaseDur))
	stub := k8s.NewStubOrchestrator("sessions")
	gate := newGateOrchestrator(stub)
	svc := service.New(gate, st, archive.NewStubCheckpointer(true), agent.NewStubClient(),
		service.WithLeaseRenewInterval(renewEvery))
	return &leaseHarness{svc: svc, store: st, orch: stub, gate: gate, client: client}
}

// snapshotted creates a session and freezes it, which is the only state a
// restore can be called from.
func (h *leaseHarness) snapshotted(t *testing.T) string {
	t.Helper()
	sess, err := h.svc.Create(context.Background(), session.CreateRequest{Name: "lease-fixture"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := h.svc.Snapshot(context.Background(), sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return sess.ID
}

// awaitRound blocks until the round is inside the orchestrator. A round that
// ends before it gets there is a test setup failure, not a hang, so it is
// reported as one instead of running out the clock.
func (h *leaseHarness) awaitRound(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-h.gate.entered:
	case err := <-done:
		t.Fatalf("round ended before reaching the orchestrator: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("round never reached the orchestrator")
	}
}

func leaseGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}
}

// failLeaseUpdates makes the first n Lease writes after armed closes fail with
// err, then lets the rest through. Arming matters: the incident happened to
// renewals during a long round, not to the write that takes the lock, and a
// reactor that fires earlier tests the wrong thing — it breaks Lock and the
// round never starts.
func failLeaseUpdates(client *fake.Clientset, armed <-chan struct{}, n int32, err error) *atomic.Int32 {
	var seen atomic.Int32
	client.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case <-armed:
		default:
			return false, nil, nil
		}
		if seen.Add(1) <= n {
			return true, nil, err
		}
		return false, nil, nil
	})
	return &seen
}

// TestRestoreSurvivesUnconfirmedLeaseRenewal is the 2026-11-17 regression. One
// renewal fails in a way that says nothing about ownership; the round must
// keep its lease, finish, and keep the pod it provisioned.
func TestRestoreSurvivesUnconfirmedLeaseRenewal(t *testing.T) {
	h := newLeaseHarness(t, 4*time.Second, 40*time.Millisecond)
	id := h.snapshotted(t)
	failLeaseUpdates(h.client, h.gate.entered, 3, errors.New("etcdserver: request timed out"))

	done := make(chan error, 1)
	go func() { _, err := h.svc.Restore(context.Background(), id); done <- err }()

	h.awaitRound(t, done)
	// Let several renewal ticks run while the round is held open, so the
	// failing writes land inside the round rather than before it.
	time.Sleep(250 * time.Millisecond)
	close(h.gate.release)

	if err := <-done; err != nil {
		t.Fatalf("restore must survive unconfirmed renewals, got: %v", err)
	}
	sess, err := h.svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sess.State != session.StateActive {
		t.Errorf("state = %q, want active", sess.State)
	}
	if h.orch.RunningCount() == 0 {
		t.Error("the round deleted the pod it had provisioned after a renewal it could not confirm")
	}
}

// TestRestoreStopsWhenOwnershipIsLost is the other half of the contract: a
// renewal that reads a different holder is a fact, and the round must stop.
func TestRestoreStopsWhenOwnershipIsLost(t *testing.T) {
	h := newLeaseHarness(t, 4*time.Second, 40*time.Millisecond)
	id := h.snapshotted(t)

	// Hand the Lease to someone else the moment the round is mid-flight.
	h.client.PrependReactor("get", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case <-h.gate.entered:
		default:
			return false, nil, nil
		}
		obj, err := h.client.Tracker().Get(leaseGVR(), "sessions", action.(k8stesting.GetAction).GetName())
		if err != nil {
			return false, nil, err
		}
		lease, ok := obj.(*coordinationv1.Lease)
		if !ok {
			return false, nil, nil
		}
		taken := lease.DeepCopy()
		taken.Spec.HolderIdentity = &otherHolder
		return true, taken, nil
	})

	done := make(chan error, 1)
	go func() { _, err := h.svc.Restore(context.Background(), id); done <- err }()
	h.awaitRound(t, done)
	time.Sleep(150 * time.Millisecond)
	close(h.gate.release)

	err := <-done
	if err == nil {
		t.Fatal("restore must fail closed once the Lease names another holder")
	}
	sess, getErr := h.svc.Get(context.Background(), id)
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	if sess.State != session.StateSnapshot {
		t.Errorf("state = %q, want the session left snapshotted for the new holder", sess.State)
	}
	if h.orch.RunningCount() != 0 {
		t.Errorf("the round left %d pod(s) behind; a round that stops reclaims what it provisioned", h.orch.RunningCount())
	}
}

// TestRestoreStopsWhenValidityElapses covers the case the retry must not paper
// over: renewals that never confirm, for longer than the lease is valid.
func TestRestoreStopsWhenValidityElapses(t *testing.T) {
	h := newLeaseHarness(t, 1*time.Second, 40*time.Millisecond)
	id := h.snapshotted(t)
	failLeaseUpdates(h.client, h.gate.entered, 1<<30, errors.New("etcdserver: request timed out"))

	done := make(chan error, 1)
	go func() { _, err := h.svc.Restore(context.Background(), id); done <- err }()
	h.awaitRound(t, done)
	// Longer than the lease: the round must give up on its own, not wait here.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("restore must fail closed once its lease validity is gone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round kept running past its lease validity")
	}
	close(h.gate.release)
}

// TestConcurrentRestoresKeepOneRound pins the fence the retry leans on: only
// one round may hold the lock, so a retrying round can never be joined by a
// second one provisioning its own pods.
func TestConcurrentRestoresKeepOneRound(t *testing.T) {
	h := newLeaseHarness(t, 4*time.Second, 40*time.Millisecond)
	id := h.snapshotted(t)

	// The second round starts only once the first is provably inside its own,
	// so the race under test is the lock's, not the goroutine scheduler's.
	first := make(chan error, 1)
	go func() { _, err := h.svc.Restore(context.Background(), id); first <- err }()
	h.awaitRound(t, first)

	_, second := h.svc.Restore(context.Background(), id)
	if !errors.Is(second, session.ErrConflict) {
		t.Fatalf("second restore = %v, want it fenced out by the held lock", second)
	}
	close(h.gate.release)
	if err := <-first; err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if got := h.orch.RunningCount(); got != 1 {
		t.Errorf("running pods = %d, want 1 — a second round must not provision its own set", got)
	}
}

// TestRenewRetriesDoNotOutliveTheLease guards the property that makes the
// retry safe to add at all: retrying must not extend the window. The anchor is
// the last confirmed renewal, so a round whose renewals never confirm has to
// stop within roughly one lease of the last one that did.
func TestRenewRetriesDoNotOutliveTheLease(t *testing.T) {
	const leaseDur = 1 * time.Second
	h := newLeaseHarness(t, leaseDur, 40*time.Millisecond)
	id := h.snapshotted(t)
	failLeaseUpdates(h.client, h.gate.entered, 1<<30, errors.New("etcdserver: request timed out"))

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := h.svc.Restore(context.Background(), id); done <- err }()
	h.awaitRound(t, done)
	err := <-done
	elapsed := time.Since(start)
	close(h.gate.release)

	if err == nil {
		t.Fatal("round must stop when its renewals never confirm")
	}
	// One lease of slack on each side: the anchor can be up to a renewal
	// interval old when the failures start, and the loop checks on ticks.
	if elapsed > 3*leaseDur {
		t.Errorf("round ran %s on a %s lease — retries extended the validity", elapsed, leaseDur)
	}
}

// otherHolder is the identity a takeover test hands the Lease to. Any value
// other than the round's own token is a loss as far as the round is concerned.
var otherHolder = "another-replica"
