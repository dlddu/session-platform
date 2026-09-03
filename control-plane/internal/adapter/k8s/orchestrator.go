// Package k8s contains the PodOrchestrator port, its client-go implementation
// (client_orchestrator.go), and an in-memory stub used by isolated tests.
package k8s

import (
	"context"
	"fmt"
	"sync"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// PodRef identifies a data plane pod backing a session. IP is the pod's cluster
// IP, recorded by Start/RestoreInto once the pod is Ready; refs rebuilt from
// stored state (name only) leave it empty.
type PodRef struct {
	Name      string
	Namespace string
	IP        string
}

func (p PodRef) String() string { return p.Namespace + "/" + p.Name }

// WorkloadSpec is the immutable runtime configuration copied from a session into
// every fresh or restored pod.
type WorkloadSpec struct {
	Type  session.WorkloadType
	Model string
}

// PodOrchestrator provisions and reclaims the dedicated data plane pod for a
// session.
type PodOrchestrator interface {
	// Start provisions a new dedicated pod for sessionID and returns its ref.
	Start(ctx context.Context, sessionID string, workload WorkloadSpec) (PodRef, error)
	// Stop tears down the pod and reclaims its CPU/memory.
	Stop(ctx context.Context, ref PodRef) error
	// RestoreInto provisions a fresh restore-target pod carrying checkpointRef. It
	// keeps the session's immutable workload type and gets a unique name so a
	// terminating source pod cannot collide.
	RestoreInto(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (PodRef, error)
	// Reach opens and closes the workload agent's attach endpoint to prove
	// readiness. It moves no user I/O; Client.Read/Write own those semantics.
	Reach(ctx context.Context, ref PodRef) error
}

// StubOrchestrator is an in-memory, no-op PodOrchestrator. It tracks which
// pods it believes are running so tests can assert the 1:1 mapping and
// reclamation behaviour without a real cluster.
type StubOrchestrator struct {
	namespace string
	mu        sync.Mutex
	seq       int
	running   map[string]PodRef               // sessionID -> pod
	workload  map[string]session.WorkloadType // sessionID -> the type it was started with
	models    map[string]string               // sessionID -> immutable model setting
}

// NewStubOrchestrator returns a stub bound to the given namespace.
func NewStubOrchestrator(namespace string) *StubOrchestrator {
	if namespace == "" {
		namespace = "sessions"
	}
	return &StubOrchestrator{
		namespace: namespace,
		running:   map[string]PodRef{},
		workload:  map[string]session.WorkloadType{},
		models:    map[string]string{},
	}
}

func (o *StubOrchestrator) Start(_ context.Context, sessionID string, workload WorkloadSpec) (PodRef, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	// Model the one-live-pod-per-session mapping without a cluster.
	ref := PodRef{Name: fmt.Sprintf("sess-%s-%04x", sessionID, o.seq), Namespace: o.namespace}
	o.running[sessionID] = ref
	o.workload[sessionID] = workload.Type
	o.models[sessionID] = workload.Model
	return ref, nil
}

// WorkloadFor reports the type the stub last started a pod for.
func (o *StubOrchestrator) WorkloadFor(sessionID string) session.WorkloadType {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.workload[sessionID]
}

// ModelFor reports the model last copied into a session pod in tests.
func (o *StubOrchestrator) ModelFor(sessionID string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.models[sessionID]
}

// RunningCount reports how many pods the stub believes are running.
func (o *StubOrchestrator) RunningCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.running)
}

func (o *StubOrchestrator) Stop(_ context.Context, ref PodRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	// A missing namespace means the service reconstructed this ref from metadata.
	for id, r := range o.running {
		if r.Name == ref.Name && (ref.Namespace == "" || r.Namespace == ref.Namespace) {
			delete(o.running, id)
		}
	}
	return nil
}

func (o *StubOrchestrator) RestoreInto(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (PodRef, error) {
	// The stub cannot apply an archive, so it models only the fresh target pod.
	_ = checkpointRef
	return o.Start(ctx, sessionID, workload)
}

// Reach is a no-op: the stub has no agent to dial.
func (o *StubOrchestrator) Reach(context.Context, PodRef) error { return nil }
