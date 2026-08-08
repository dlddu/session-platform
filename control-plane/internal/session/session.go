// Package session defines the core domain model for the session platform:
// the Session entity, its lifecycle State, and the SessionManager port that
// the REST API depends on. Orchestration of the actual data plane workload is
// delegated to the adapter ports (see internal/adapter).
package session

import (
	"errors"
	"time"
)

// State is the lifecycle state of a session.
//
// The state machine (see docs/prd/lifecycle.md, docs/prd/state-api.md):
//
//	active  ──idle 60m──▶ idle ──idle 60m total──▶ snapshot
//	  ▲                     │                          │
//	  └──────── access ◀────┴──── access (restore) ◀───┘
//
// Transitions between these states MUST be atomic (AC-C1).
type State string

const (
	// StateActive — session has a live, dedicated data plane pod and serves
	// read/write directly. (AC-A2)
	StateActive State = "active"
	// StateIdle — pod is still held but the session has had no read/write for
	// a while; it is a candidate for snapshotting. (AC-B1)
	StateIdle State = "idle"
	// StateSnapshot — session has been checkpointed via CRIU and its pod
	// reclaimed; accessing it triggers a restore. (AC-B1, AC-B2, AC-A3)
	StateSnapshot State = "snapshot"
)

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StateActive, StateIdle, StateSnapshot:
		return true
	default:
		return false
	}
}

// WorkloadType selects which data plane workload a session runs (AC-E1). It is
// chosen at creation and immutable for the session's lifetime — changing it
// means creating a new session. The control plane provisions a different data
// plane workload per type; AC-A1 (the control plane never runs the workload
// itself) and AC-A2 (one dedicated pod per session) hold for every type.
type WorkloadType string

const (
	// WorkloadTypeShell — a PTY-attached interactive shell, the default when
	// the create request omits the type (AC-D1~D5, docs/prd/shell-workload.md).
	WorkloadTypeShell WorkloadType = "shell"
	// WorkloadTypeClaudeCode — the Claude Code CLI workload
	// (docs/prd/claude-code-workload.md, AC-E2~E6).
	WorkloadTypeClaudeCode WorkloadType = "claude-code"
)

// Valid reports whether w is a known workload type. The empty string is not
// valid here — it means "unspecified", which NormalizeWorkloadType folds to the
// default. Use that on the way in and this to reject explicit garbage.
func (w WorkloadType) Valid() bool {
	switch w {
	case WorkloadTypeShell, WorkloadTypeClaudeCode:
		return true
	default:
		return false
	}
}

// NormalizeWorkloadType folds "unspecified" to the default type (AC-E1: an
// omitted workloadType creates a shell session, exactly as before the type axis
// existed) and rejects any other unknown value with ErrInvalidInput.
//
// It doubles as the read path for stored sessions: records written before the
// type axis existed carry no workloadType, so decoding them yields "" — which
// this resolves to shell, the only type those sessions could have been.
func NormalizeWorkloadType(w WorkloadType) (WorkloadType, error) {
	if w == "" {
		return WorkloadTypeShell, nil
	}
	if !w.Valid() {
		return "", ErrInvalidInput
	}
	return w, nil
}

// MaxIdle is the maximum idle duration before a session is snapshotted.
// The operational trigger that enforces it is service.IdleReaper (AC-B1).
//
// TODO(policy): 60m is the maximum idle limit from AC-B1 and the reaper now
// enforces the plain "idle >= MaxIdle -> snapshot" rule. The finer trigger
// policy — grace periods, per-session overrides, and whether to freeze a
// shell running a long foreground job that has merely gone client-idle
// (AC-D5) — remains a deferred product decision.
const MaxIdle = 60 * time.Minute

// Checkpoint captures the metadata of a CRIU checkpoint for a snapshotted
// session. The actual image bytes live wherever the Checkpointer adapter
// stores them; this is just the reference the control plane tracks.
type Checkpoint struct {
	Ref       string    `json:"ref"`                 // opaque checkpoint identifier
	SizeBytes int64     `json:"sizeBytes"`           // checkpoint image size
	CreatedAt time.Time `json:"createdAt"`           // when the snapshot was taken
	Reclaimed string    `json:"reclaimed,omitempty"` // human-readable reclaimed resources, e.g. "2 vCPU · 4 GB"
}

// Session is the aggregate root: one logical session mapped 1:1 to (at most)
// one data plane pod (AC-A2). When State is StateSnapshot the pod is reclaimed
// and Checkpoint is populated instead.
type Session struct {
	ID string `json:"id"`
	// WorkloadType is fixed at creation and never mutated afterwards (AC-E1);
	// no API changes it and snapshot/restore carry it across. Sessions stored
	// before the type axis existed decode as "" — read it through
	// NormalizeWorkloadType, which resolves that to the shell default.
	WorkloadType WorkloadType `json:"workloadType,omitempty"`
	Name         string       `json:"name"`
	State        State        `json:"state"`
	Pod          string       `json:"pod,omitempty"` // data plane pod name; empty when snapshotted/reclaimed
	CreatedAt    time.Time    `json:"createdAt"`
	LastAccess   time.Time    `json:"lastAccess"`           // last read/write; drives idle/snapshot timing (AC-B1)
	Checkpoint   *Checkpoint  `json:"checkpoint,omitempty"` // present only when State == StateSnapshot
}

// IdleFor returns how long the session has been without a read/write as of now.
func (s *Session) IdleFor(now time.Time) time.Duration {
	return now.Sub(s.LastAccess)
}

// Domain errors returned by SessionManager. The API layer maps these to HTTP
// status codes (see internal/api).
var (
	ErrNotFound     = errors.New("session not found")
	ErrInvalidState = errors.New("session in invalid state for operation")
	ErrConflict     = errors.New("session state changed concurrently")
	ErrInvalidInput = errors.New("invalid input")
)
