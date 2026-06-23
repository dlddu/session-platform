//go:build e2e

package e2e_test

import "testing"

// This file seeds the *deferred* e2e scenarios — the ones blocked on real
// adapters or lifecycle triggers that the α stub SUT cannot exercise. Each test
// skips with its blocking precondition and the AC it will verify, so a future
// PR that introduces the real adapter/trigger fills in the body and removes the
// t.Skip. Running the suite then surfaces each as "skipped" with the reason.
//
// The mapping from these placeholders to the documented scenarios/ACs lives in
// docs/test/e2e.md.

// AC-A1/A2 (real pod): the deployed control-plane creates a dedicated Pod object
// per session in the session namespace, 1:1.
// Blocked on: a real client-go PodOrchestrator — today the orchestrator is an
// in-memory stub, so the pod name is synthetic and no Pod object exists to assert.
func TestDeferred_RealPodProvisioned(t *testing.T) {
	t.Skip("deferred: needs the real client-go PodOrchestrator to assert a Pod object exists 1:1 (AC-A1/A2); fill when the k8s adapter lands")
}

// AC-A3 (real pod): terminating/snapshotting a session deletes its Pod and
// reclaims cluster resources.
// Blocked on: a real client-go PodOrchestrator (and the snapshot/terminate path).
func TestDeferred_RealPodReclaimed(t *testing.T) {
	t.Skip("deferred: needs the real client-go PodOrchestrator to assert Pod deletion + resource reclaim (AC-A3); fill when the k8s adapter lands")
}

// AC-B1: after 60m idle a session is checkpointed (CRIU) and transitions to
// snapshot with its pod reclaimed.
// Blocked on: an idle->snapshot trigger (reaper or test-only endpoint). The α SUT
// never leaves the active state, so there is no way to drive a snapshot here.
func TestDeferred_IdleToSnapshot(t *testing.T) {
	t.Skip("deferred: needs an idle->snapshot trigger (reaper or test endpoint) to reach the snapshot state (AC-B1); fill when the lifecycle trigger lands")
}

// AC-B2: accessing a snapshot session restores it into a new pod and goes active.
// Blocked on: a snapshot-state session (see IdleToSnapshot) + real CRIU.
func TestDeferred_SnapshotRestore(t *testing.T) {
	t.Skip("deferred: needs a snapshot-state session (AC-B1 trigger) to exercise restore-on-access (AC-B2); fill when snapshot + restore land")
}

// AC-B3: a restored session's in-memory state matches the pre-snapshot state.
// Blocked on: a verified CRIU runtime (ContainerCheckpoint feature gate) — the
// stub checkpointer carries no real process state. See docs/criu-verification.md.
func TestDeferred_CRIUIntegrity(t *testing.T) {
	t.Skip("deferred: needs a verified CRIU runtime to assert checkpoint/restore integrity (AC-B3); see docs/criu-verification.md")
}

// AC-C2 (idle/snapshot branches): read dispatches on a non-active state.
// Blocked on: idle/snapshot states (lifecycle trigger). The active-read branch
// is covered by TestReadSession_ActivePath in e2e_test.go.
func TestDeferred_ReadIdleAndSnapshotBranches(t *testing.T) {
	t.Skip("deferred: needs idle/snapshot states to assert the non-active read branches (AC-C2); the active branch is covered today")
}

// AC-C3 (idle/snapshot branches): write dispatches on a non-active state.
// Blocked on: idle/snapshot states (lifecycle trigger). The active-write branch
// is covered by TestWriteSession_ActivePath in e2e_test.go.
func TestDeferred_WriteIdleAndSnapshotBranches(t *testing.T) {
	t.Skip("deferred: needs idle/snapshot states to assert the non-active write branches (AC-C3); the active branch is covered today")
}

// AC-C1: concurrent restore/snapshot/switch on the same session converge to a
// single valid state with no torn transitions or duplicate pods.
// Blocked on: a real Redis-backed StateStore + multi-replica control-plane. The
// α SUT is a single replica with an in-memory store, so cross-replica atomicity
// cannot be exercised.
func TestDeferred_CrossReplicaAtomicity(t *testing.T) {
	t.Skip("deferred: needs a Redis-backed StateStore + multi-replica control-plane to assert atomic transitions (AC-C1); fill when the redis adapter lands")
}
