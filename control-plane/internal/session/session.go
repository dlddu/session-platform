// Package session defines the core domain model for the session platform:
// the Session entity, its lifecycle State, and the SessionManager port that
// the REST API depends on. Orchestration of the actual data plane workload is
// delegated to the adapter ports (see internal/adapter).
package session

import (
	"errors"
	"regexp"
	"strings"
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
	// StateSnapshot — the workload has been archived and its pod reclaimed;
	// accessing it triggers its workload-specific restore. (AC-B1, AC-B2, AC-A3)
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

	// PlatformDefaultModel is the stable session-level alias for the model
	// selected by the platform's Claude Code installation. Keeping the alias in
	// session metadata makes the model choice explicit and immutable without
	// hard-coding a vendor model version into the API contract (AC-E6).
	PlatformDefaultModel = "platform-default"

	// MaxClaudePromptBytes is the decoded UTF-8 prompt limit.
	MaxClaudePromptBytes = 1 << 20
)

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

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

// NormalizeModel validates the workload-specific model setting (AC-E6).
// Shell sessions do not have a model and reject one rather than silently
// accepting a meaningless immutable setting. Claude Code sessions resolve an
// omitted model to PlatformDefaultModel; concrete model identifiers are kept
// verbatim after trimming and are passed to the CLI as a single argv element.
func NormalizeModel(workload WorkloadType, model string) (string, error) {
	model = strings.TrimSpace(model)
	switch workload {
	case WorkloadTypeShell:
		if model != "" {
			return "", ErrInvalidInput
		}
		return "", nil
	case WorkloadTypeClaudeCode:
		if model == "" {
			return PlatformDefaultModel, nil
		}
		if !modelNamePattern.MatchString(model) {
			return "", ErrInvalidInput
		}
		return model, nil
	default:
		return "", ErrInvalidInput
	}
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

// Checkpoint captures metadata for a workload snapshot. Shell sessions store
// CRIU images plus scrollback; filesystem workloads store a durable archive.
type Checkpoint struct {
	Ref       string    `json:"ref"`                 // opaque checkpoint identifier
	SizeBytes int64     `json:"sizeBytes"`           // checkpoint image size
	CreatedAt time.Time `json:"createdAt"`           // when the snapshot was taken
	Reclaimed string    `json:"reclaimed,omitempty"` // human-readable reclaimed resources, e.g. "2 vCPU · 4 GB"
	// AbortToken identifies the in-flight data-plane archive generation. It is
	// intentionally transient: the service only needs it until the source pod is
	// reclaimed, and it must never become API or durable session metadata.
	AbortToken string `json:"-"`
}

// SnapshotPhase is the durable decision point of a crash-recoverable archive
// snapshot. Preparing means the source pod must be kept and its admission
// barrier rolled back after a restart. Committing means the archive reference
// is durable and pod reclamation must be completed idempotently.
type SnapshotPhase string

const (
	SnapshotPhasePreparing  SnapshotPhase = "preparing"
	SnapshotPhaseCommitting SnapshotPhase = "committing"
)

// SnapshotTransaction records just enough of an in-flight filesystem archive
// to recover after the control plane exits. It is hidden from the public
// Session JSON; the ConfigMap StateStore persists it through its private storage
// envelope.
type SnapshotTransaction struct {
	Generation string `json:"generation"`
	// Owner is the current control-plane Lease holder's fencing token. Recovery
	// claims a preparing transaction by changing Owner before touching the agent.
	Owner      string        `json:"owner"`
	SourcePod  string        `json:"sourcePod"`
	Phase      SnapshotPhase `json:"phase"`
	Checkpoint *Checkpoint   `json:"checkpoint,omitempty"`
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
	// Model is meaningful only for claude-code sessions and, like the workload
	// type, is fixed at creation. "platform-default" delegates selection to the
	// platform-managed Claude Code installation (AC-E6).
	Model      string      `json:"model,omitempty"`
	Name       string      `json:"name"`
	State      State       `json:"state"`
	Pod        string      `json:"pod,omitempty"` // data plane pod name; empty when snapshotted/reclaimed
	CreatedAt  time.Time   `json:"createdAt"`
	LastAccess time.Time   `json:"lastAccess"`           // last read/write; drives idle/snapshot timing (AC-B1)
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"` // present only when State == StateSnapshot
	// SnapshotTransaction is internal recovery metadata, never part of the API.
	// The ConfigMap adapter deliberately encodes it in its durable representation
	// even though ordinary JSON marshaling omits it.
	SnapshotTransaction *SnapshotTransaction `json:"-"`
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
	// ErrRequestBodyTooLarge reports the public HTTP wire-body limit.
	ErrRequestBodyTooLarge = errors.New("request body exceeds size limit")
	// ErrWorkloadPromptTooLarge reports the per-write prompt byte limit.
	ErrWorkloadPromptTooLarge = errors.New("workload prompt exceeds size limit")
	// ErrWorkloadQueueFull is a transient admission failure: the per-session
	// workload already has the maximum number of accepted prompts queued.
	ErrWorkloadQueueFull = errors.New("workload prompt queue is full")
	// ErrWorkloadOutputFull is terminal for further writes until the session is
	// replaced: its bounded append-only output history reached the hard quota.
	ErrWorkloadOutputFull = errors.New("workload output quota is full")
	// ErrCheckpointDisabled prevents the gate-off checkpointer from deleting a
	// live pod behind synthetic metadata. Callers may retry after the workload's
	// real snapshot strategy is enabled.
	ErrCheckpointDisabled = errors.New("checkpoint strategy is disabled")
)
