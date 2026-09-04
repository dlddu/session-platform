// Package k8s contains the PodOrchestrator port, its client-go implementation
// (client_orchestrator.go), and an in-memory stub used by isolated tests. Both
// model the same workload-aware pod lifecycle contract.
package k8s

import (
	"context"
	"fmt"
	"sync"

	"github.com/dlddu/session-platform/control-plane/internal/session"
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

// SessionPods is the set of pods one session owns (AC-A2, AC-F4).
// Provisioning returns the whole set because reclamation, freeze and restore
// hold across it, not across the workload pod alone.
type SessionPods struct {
	Workload  PodRef
	Auxiliary []PodRef
}

// All returns every pod in the set, workload pod first, skipping unset refs so
// a partially reclaimed set never yields a nameless pod.
func (s SessionPods) All() []PodRef {
	refs := make([]PodRef, 0, 1+len(s.Auxiliary))
	if s.Workload.Name != "" {
		refs = append(refs, s.Workload)
	}
	for _, ref := range s.Auxiliary {
		if ref.Name != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// Names returns the auxiliary pod names in order, for the session record's
// AuxiliaryPods field. It is nil when there are none, keeping the field off the
// wire for the workload types that provision only a workload pod.
func (s SessionPods) Names() []string {
	if len(s.Auxiliary) == 0 {
		return nil
	}
	names := make([]string, 0, len(s.Auxiliary))
	for _, ref := range s.Auxiliary {
		names = append(names, ref.Name)
	}
	return names
}

// WorkloadSpec is the immutable runtime configuration copied from a session
// into every fresh or restored pod (Model: see session.NormalizeModel).
type WorkloadSpec struct {
	Type  session.WorkloadType
	Model string
}

// PodOrchestrator provisions and reclaims the pods that back a session — its
// dedicated workload pod and any session-scoped auxiliary pods.
//
// AC mapping:
//   - Start       → AC-A1, AC-A2, AC-E1, AC-F4
//   - Stop        → AC-A3
//   - RestoreInto → AC-B2
//   - Reach       → AC-D1/AC-E1
type PodOrchestrator interface {
	// Start provisions the session's fresh pod set.
	Start(ctx context.Context, sessionID string, workload WorkloadSpec) (SessionPods, error)
	// Stop tears down the given pods and reclaims their CPU/memory.
	Stop(ctx context.Context, refs ...PodRef) error
	// RestoreInto provisions a fresh restore-target pod set carrying
	// checkpointRef, under unique names (see restorePodName).
	RestoreInto(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (SessionPods, error)
	// Reach opens and closes the workload agent's attach endpoint to prove
	// readiness. It moves no user I/O; Client.Read/Write own those semantics.
	// (AC-D1/AC-E1)
	Reach(ctx context.Context, ref PodRef) error
}

// StubOrchestrator is an in-memory, no-op PodOrchestrator. It tracks which
// pods it believes are running so tests can assert the 1:1 mapping and
// reclamation behaviour without a real cluster.
type StubOrchestrator struct {
	namespace string
	mu        sync.Mutex
	seq       int
	auxiliary int
	running   map[string]SessionPods          // sessionID -> the session's pod set
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
		running:   map[string]SessionPods{},
		workload:  map[string]session.WorkloadType{},
		models:    map[string]string{},
	}
}

// SetAuxiliaryPods makes subsequent Start/RestoreInto calls provision n
// auxiliary pods alongside the workload pod, independently of the workload type
// the stub is asked for. It is how in-process tests exercise the multi-pod half
// of the AC-A2/AC-A3/AC-B2 contract without standing up the real orchestrator —
// whose pod shapes are asserted against a fake clientset in control-plane/test.
// Set it before the session under test is created.
func (o *StubOrchestrator) SetAuxiliaryPods(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.auxiliary = n
}

func (o *StubOrchestrator) Start(_ context.Context, sessionID string, workload WorkloadSpec) (SessionPods, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	pods := SessionPods{Workload: o.newRefLocked(sessionID)}
	for i := 0; i < o.auxiliary; i++ {
		pods.Auxiliary = append(pods.Auxiliary, o.newRefLocked(sessionID))
	}
	o.running[sessionID] = pods
	o.workload[sessionID] = workload.Type
	o.models[sessionID] = workload.Model
	return pods, nil
}

func (o *StubOrchestrator) newRefLocked(sessionID string) PodRef {
	o.seq++
	return PodRef{Name: fmt.Sprintf("sess-%s-%04x", sessionID, o.seq), Namespace: o.namespace}
}

// WorkloadFor reports the type the stub last started a pod for, so in-process
// tests can assert the session's type reached the orchestrator (AC-E1).
func (o *StubOrchestrator) WorkloadFor(sessionID string) session.WorkloadType {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.workload[sessionID]
}

// ModelFor reports the model last copied into a session pod in tests (AC-E6).
func (o *StubOrchestrator) ModelFor(sessionID string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.models[sessionID]
}

// RunningCount reports how many pods the stub believes are running. It counts
// individual pods, not sessions, so a session with auxiliary pods is only fully
// reclaimed when this reaches zero.
func (o *StubOrchestrator) RunningCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, pods := range o.running {
		n += len(pods.All())
	}
	return n
}

// SessionsCount reports how many sessions still hold at least one pod.
func (o *StubOrchestrator) SessionsCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.running)
}

func (o *StubOrchestrator) Stop(_ context.Context, refs ...PodRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ref := range refs {
		o.stopLocked(ref)
	}
	return nil
}

func (o *StubOrchestrator) stopLocked(ref PodRef) {
	// A missing namespace means the service reconstructed this ref from metadata.
	matches := func(r PodRef) bool {
		return r.Name == ref.Name && (ref.Namespace == "" || r.Namespace == ref.Namespace)
	}
	for id, pods := range o.running {
		if matches(pods.Workload) {
			pods.Workload = PodRef{}
		}
		// Filter into a fresh slice: the one Start returned is still held by the
		// caller and must not be rewritten underneath it.
		var kept []PodRef
		for _, aux := range pods.Auxiliary {
			if !matches(aux) {
				kept = append(kept, aux)
			}
		}
		pods.Auxiliary = kept
		if pods.Workload.Name == "" && len(pods.Auxiliary) == 0 {
			delete(o.running, id)
			continue
		}
		o.running[id] = pods
	}
}

func (o *StubOrchestrator) RestoreInto(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (SessionPods, error) {
	// The stub cannot apply an archive, so it models only the fresh target pods.
	_ = checkpointRef
	return o.Start(ctx, sessionID, workload)
}

// Reach is a no-op: the stub has no agent to dial, and its pods are always
// considered reachable. The real dial lives in ClientOrchestrator.
func (o *StubOrchestrator) Reach(context.Context, PodRef) error { return nil }
