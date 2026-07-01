// Package k8s contains the PodOrchestrator port and an in-memory stub
// implementation. The real implementation will drive data plane pods through
// client-go; here we only model the contract and a no-op lifecycle so the
// happy path runs end-to-end without a cluster.
package k8s

import (
	"context"
	"fmt"
	"sync"
)

// PodRef identifies a data plane pod backing a session. IP is the pod's
// cluster IP, recorded by Start/RestoreInto once the pod is Ready so the
// control plane can dial the session agent; refs rebuilt from stored state
// (name only) leave it empty.
type PodRef struct {
	Name      string
	Namespace string
	IP        string
}

func (p PodRef) String() string { return p.Namespace + "/" + p.Name }

// PodOrchestrator provisions and reclaims the dedicated data plane pod for a
// session.
//
// AC mapping:
//   - Start   → AC-A1 (control plane orchestrates, workload runs in the pod),
//     AC-A2 (one dedicated pod per session).
//   - Stop    → AC-A3 (resources reclaimed on terminate/snapshot).
//   - RestoreInto → AC-B2 (restore a checkpoint into a *new* pod).
//   - Reach   → AC-D1 (the pod's PTY shell is reachable from the control
//     plane before the session counts as active).
type PodOrchestrator interface {
	// Start provisions a new dedicated pod for sessionID and returns its ref.
	Start(ctx context.Context, sessionID string) (PodRef, error)
	// Stop tears down the pod and reclaims its CPU/memory (AC-A3).
	Stop(ctx context.Context, ref PodRef) error
	// RestoreInto provisions a fresh pod that a checkpoint will be restored
	// into (AC-B2). The checkpoint bytes are applied by the Checkpointer.
	RestoreInto(ctx context.Context, sessionID string) (PodRef, error)
	// Reach proves the session shell agent in ref's pod is reachable by
	// opening its attach stream and closing it again (AC-D1). It moves no
	// payload — the stdin/stdout semantics on the stream are J5-S2/S3.
	Reach(ctx context.Context, ref PodRef) error
}

// StubOrchestrator is an in-memory, no-op PodOrchestrator. It tracks which
// pods it believes are running so tests can assert the 1:1 mapping and
// reclamation behaviour without a real cluster.
type StubOrchestrator struct {
	namespace string
	mu        sync.Mutex
	seq       int
	running   map[string]PodRef // sessionID -> pod
}

// NewStubOrchestrator returns a stub bound to the given namespace.
func NewStubOrchestrator(namespace string) *StubOrchestrator {
	if namespace == "" {
		namespace = "sessions"
	}
	return &StubOrchestrator{namespace: namespace, running: map[string]PodRef{}}
}

func (o *StubOrchestrator) Start(_ context.Context, sessionID string) (PodRef, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	// TODO(client-go): create a Pod (or a Deployment of 1) from the data plane
	// base image, wait for Ready, then return its ref. Enforce 1:1 by naming
	// the pod after the session (AC-A2).
	ref := PodRef{Name: fmt.Sprintf("sess-%s-%04x", sessionID, o.seq), Namespace: o.namespace}
	o.running[sessionID] = ref
	return ref, nil
}

func (o *StubOrchestrator) Stop(_ context.Context, ref PodRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	// TODO(client-go): delete the Pod and confirm resources are reclaimed (AC-A3).
	for id, r := range o.running {
		if r == ref {
			delete(o.running, id)
		}
	}
	return nil
}

func (o *StubOrchestrator) RestoreInto(ctx context.Context, sessionID string) (PodRef, error) {
	// TODO(criu): for a real restore this provisions a pod prepared to accept a
	// CRIU image; the Checkpointer then restores into it (AC-B2). The stub just
	// starts a fresh pod.
	return o.Start(ctx, sessionID)
}

// Reach is a no-op: the stub has no agent to dial, and its pods are always
// considered reachable. The real dial lives in ClientOrchestrator.
func (o *StubOrchestrator) Reach(context.Context, PodRef) error { return nil }
