// Package session defines the core domain model: the Session entity, its
// lifecycle State, and the ports the REST API depends on.
package session

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// State is the lifecycle state of a session.
type State string

const (
	// StateActive holds a live data plane pod and serves read/write directly.
	StateActive State = "active"
	// StateIdle still holds the pod but has had no recent read/write.
	StateIdle State = "idle"
	// StateSnapshot has archived the workload and reclaimed the pod.
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

// WorkloadType selects which data plane workload a session runs. It is chosen
// at creation and immutable for the session's lifetime.
type WorkloadType string

const (
	// WorkloadTypeShell is a PTY-attached interactive shell, the default type.
	WorkloadTypeShell WorkloadType = "shell"
	// WorkloadTypeClaudeCode is the Claude Code CLI workload.
	WorkloadTypeClaudeCode WorkloadType = "claude-code"

	// PlatformDefaultModel defers model choice to the platform's Secret and then
	// the Claude Code installation default.
	PlatformDefaultModel = "platform-default"

	// MaxClaudePromptBytes is the decoded UTF-8 prompt limit.
	MaxClaudePromptBytes = 1 << 20
)

// A single leading '~' is accepted for OpenRouter's moving "latest" aliases
// (for example, ~anthropic/claude-opus-latest).
var modelNamePattern = regexp.MustCompile(`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`)

// Valid reports whether w is a known workload type.
func (w WorkloadType) Valid() bool {
	switch w {
	case WorkloadTypeShell, WorkloadTypeClaudeCode:
		return true
	default:
		return false
	}
}

// NormalizeWorkloadType folds an unspecified type to the default and rejects
// any other unknown value with ErrInvalidInput.
func NormalizeWorkloadType(w WorkloadType) (WorkloadType, error) {
	if w == "" {
		return WorkloadTypeShell, nil
	}
	if !w.Valid() {
		return "", ErrInvalidInput
	}
	return w, nil
}

// NormalizeModel validates the workload-specific model setting.
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
const MaxIdle = 60 * time.Minute

// Checkpoint captures metadata for a workload snapshot.
type Checkpoint struct {
	Ref       string    `json:"ref"`                 // opaque checkpoint identifier
	SizeBytes int64     `json:"sizeBytes"`           // checkpoint image size
	CreatedAt time.Time `json:"createdAt"`           // when the snapshot was taken
	Reclaimed string    `json:"reclaimed,omitempty"` // human-readable reclaimed resources, e.g. "2 vCPU · 4 GB"
	// AbortToken identifies the in-flight data-plane archive generation.
	AbortToken string `json:"-"`
}

// SnapshotPhase is the durable decision point of a crash-recoverable snapshot.
type SnapshotPhase string

const (
	SnapshotPhasePreparing  SnapshotPhase = "preparing"
	SnapshotPhaseCommitting SnapshotPhase = "committing"
)

// SnapshotTransaction records an in-flight filesystem archive so it can be
// recovered after the control plane exits.
type SnapshotTransaction struct {
	Generation string `json:"generation"`
	// Owner is the current control-plane Lease holder's fencing token.
	Owner      string        `json:"owner"`
	SourcePod  string        `json:"sourcePod"`
	Phase      SnapshotPhase `json:"phase"`
	Checkpoint *Checkpoint   `json:"checkpoint,omitempty"`
}

// Session is the aggregate root: one logical session mapped 1:1 to at most one
// data plane pod.
type Session struct {
	ID string `json:"id"`
	// WorkloadType is fixed at creation and never mutated afterwards.
	WorkloadType WorkloadType `json:"workloadType,omitempty"`
	// Model is meaningful only for claude-code sessions and is fixed at creation.
	Model      string      `json:"model,omitempty"`
	Name       string      `json:"name"`
	State      State       `json:"state"`
	Pod        string      `json:"pod,omitempty"` // data plane pod name; empty when snapshotted/reclaimed
	CreatedAt  time.Time   `json:"createdAt"`
	LastAccess time.Time   `json:"lastAccess"`           // last read/write; drives idle/snapshot timing (AC-B1)
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"` // present only when State == StateSnapshot
	// SnapshotTransaction is internal recovery metadata, never part of the API.
	SnapshotTransaction *SnapshotTransaction `json:"-"`
}

// IdleFor returns how long the session has been without a read/write as of now.
func (s *Session) IdleFor(now time.Time) time.Duration {
	return now.Sub(s.LastAccess)
}

// Domain errors returned by SessionManager.
var (
	ErrNotFound     = errors.New("session not found")
	ErrInvalidState = errors.New("session in invalid state for operation")
	ErrConflict     = errors.New("session state changed concurrently")
	ErrInvalidInput = errors.New("invalid input")
	// ErrRequestBodyTooLarge reports the public HTTP wire-body limit.
	ErrRequestBodyTooLarge = errors.New("request body exceeds size limit")
	// ErrWorkloadPromptTooLarge reports the per-write prompt byte limit.
	ErrWorkloadPromptTooLarge = errors.New("workload prompt exceeds size limit")
	// ErrWorkloadQueueFull is a transient admission failure; the caller may retry.
	ErrWorkloadQueueFull = errors.New("workload prompt queue is full")
	// ErrWorkloadOutputFull is terminal until the session is replaced.
	ErrWorkloadOutputFull = errors.New("workload output quota is full")
	// ErrCheckpointDisabled prevents reclaiming a live pod behind synthetic metadata.
	ErrCheckpointDisabled = errors.New("checkpoint strategy is disabled")
)
