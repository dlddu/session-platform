// Package session defines the core domain model for the session platform:
// the Session entity, its lifecycle State, and the SessionManager port that
// the REST API depends on. Orchestration of the actual data plane workload is
// delegated to the adapter ports (see internal/adapter).
package session

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// State is the lifecycle state of a session. The state machine and its
// atomicity requirement are specified in docs/prd/lifecycle.md (AC-B1/AC-B2)
// and docs/prd/state-api.md (AC-C1).
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
// means creating a new session. AC-A1 and AC-A2 (docs/prd/architecture.md) hold
// for every type; the pod set they govern is Session.Pod plus
// Session.AuxiliaryPods.
type WorkloadType string

const (
	// WorkloadTypeShell — a PTY-attached interactive shell, the default when
	// the create request omits the type (AC-D1~D5, docs/prd/shell-workload.md).
	WorkloadTypeShell WorkloadType = "shell"
	// WorkloadTypeClaudeCode — the Claude Code CLI workload
	// (docs/prd/claude-code-workload.md, AC-E2~E6).
	WorkloadTypeClaudeCode WorkloadType = "claude-code"

	// PlatformDefaultModel is the stable session-level alias for the model
	// selected by the platform's optional ConfigMap configuration (falling back to
	// the Claude Code installation default). (AC-E6)
	PlatformDefaultModel = "platform-default"

	// MaxClaudePromptBytes is the decoded UTF-8 prompt limit.
	MaxClaudePromptBytes = 1 << 20
)

// Model identifiers are at most 128 characters. In addition to ordinary
// provider/model names, a single leading '~' is accepted for OpenRouter's
// moving "latest" aliases (for example, ~anthropic/claude-opus-latest).
var modelNamePattern = regexp.MustCompile(`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`)

// Valid reports whether w is a known workload type. The empty string is not
// valid here — it means "unspecified", which NormalizeWorkloadType folds to the
// default. Use that on the way in and this to reject explicit garbage.
func (w WorkloadType) Valid() bool {
	switch w {
	case WorkloadTypeShell, WorkloadTypeClaudeCode, WorkloadTypeMiniMaxCode,
		WorkloadTypeCodexChat, WorkloadTypeCodexTask:
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
// A request or tenant grant naming "codex" is refused: every caller moved to
// codex-chat (AC-J1); only stored records keep the old name, read through
// NormalizeStoredWorkloadType.
func NormalizeWorkloadType(w WorkloadType) (WorkloadType, error) {
	if w == "" {
		return WorkloadTypeShell, nil
	}
	if !w.Valid() {
		return "", ErrInvalidInput
	}
	return w, nil
}

// NormalizeStoredWorkloadType is NormalizeWorkloadType for a decoded session
// record. Records written before the codex split carry "codex", which is
// codex-chat (AC-J1); refusing it would fail every List that meets one.
func NormalizeStoredWorkloadType(w WorkloadType) (WorkloadType, error) {
	if w == legacyWorkloadTypeCodex {
		return WorkloadTypeCodexChat, nil
	}
	return NormalizeWorkloadType(w)
}

// NormalizeModel validates the workload-specific model setting (AC-E6).
// Shell sessions do not have a model and reject one rather than silently
// accepting a meaningless immutable setting. The agent types resolve an omitted
// model to PlatformDefaultModel; concrete model identifiers are kept verbatim
// after trimming and are passed to the CLI as a single argv element.
// minimax-code shares this contract unchanged (AC-G1 → AC-E6), so the agent
// types must not drift apart here.
func NormalizeModel(workload WorkloadType, model string) (string, error) {
	model = strings.TrimSpace(model)
	switch workload {
	case WorkloadTypeShell:
		if model != "" {
			return "", ErrInvalidInput
		}
		return "", nil
	case WorkloadTypeClaudeCode, WorkloadTypeMiniMaxCode,
		WorkloadTypeCodexChat, WorkloadTypeCodexTask:
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

// MaxSessionPlugins bounds a create request's plugin list (AC-L1). A plugin is
// installed on every boot and restore, so the bound keeps one request from
// turning pod start into an unbounded marketplace walk.
const MaxSessionPlugins = 16

// pluginNamePattern is a marketplace plugin name: one path segment the data
// plane can use as a directory name and Claude Code as the left half of
// "<plugin>@<marketplace>".
var pluginNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// NormalizePlugins validates the session's marketplace plugin selection
// (AC-L1). nil means unspecified: the session gets its workload type's
// platform-default plugins, exactly as before the field existed. A non-nil list
// replaces that default outright, so it must be non-empty, free of duplicates
// and made of valid names. Only the types whose agent loads marketplace plugins
// take one; shell rejects it rather than storing a setting
// nothing would honour. The returned slice is a copy in request order.
func NormalizePlugins(workload WorkloadType, plugins []string) ([]string, error) {
	if plugins == nil {
		return nil, nil
	}
	if !TakesPlugins(workload) {
		return nil, ErrInvalidInput
	}
	if len(plugins) == 0 || len(plugins) > MaxSessionPlugins {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]bool, len(plugins))
	out := make([]string, 0, len(plugins))
	for _, name := range plugins {
		if !pluginNamePattern.MatchString(name) || seen[name] {
			return nil, ErrInvalidInput
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// TakesPlugins reports whether a create request may carry plugins for the
// workload type (AC-L1).
func TakesPlugins(workload WorkloadType) bool {
	switch workload {
	case WorkloadTypeClaudeCode, WorkloadTypeMiniMaxCode, WorkloadTypeCodexChat, WorkloadTypeCodexTask:
		return true
	default:
		return false
	}
}

// NormalizeReasoningEffort validates the session's reasoning effort (AC-E8).
// Empty means unset — the CLI's own default — and is valid for every type; a
// value is valid only when the type's catalog entry lists it, so the catalog
// is the one place a type's efforts are declared. Callers that must reject an
// explicitly empty value (the create API) do so before calling this.
func NormalizeReasoningEffort(workload WorkloadType, effort string) (string, error) {
	if effort == "" {
		return "", nil
	}
	for _, info := range workloadTypeCatalog {
		if info.Type == workload && slices.Contains(info.ReasoningEfforts, effort) {
			return effort, nil
		}
	}
	return "", ErrInvalidInput
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

// Checkpoint captures metadata for a workload snapshot: the durable filesystem
// archive every workload type freezes into.
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
// one data plane *workload* pod, plus any session-scoped auxiliary pods that
// serve it (AC-A2). When State is StateSnapshot every pod of the session is
// reclaimed and Checkpoint is populated instead.
type Session struct {
	ID string `json:"id"`
	// WorkloadType is fixed at creation (AC-E1); no API changes it and
	// snapshot/restore carry it across. Read stored values through
	// NormalizeWorkloadType.
	WorkloadType WorkloadType `json:"workloadType,omitempty"`
	// Model is meaningful only for the agent workload types (claude-code and
	// minimax-code) and, like the workload type, is fixed at creation.
	// "platform-default" delegates selection to the platform-managed ConfigMap and
	// then the CLI's installation fallback (AC-E6, reused verbatim by AC-G1).
	Model string `json:"model,omitempty"`
	// Plugins is the session's marketplace plugin selection (AC-L1), fixed at
	// creation and carried across snapshot/restore. Absent means the workload
	// type's platform default; present, it is the exact set the agent loads.
	Plugins []string `json:"plugins,omitempty"`
	// ReasoningEffort is the session's reasoning effort for the types whose
	// catalog entry lists efforts (AC-E8, AC-M4). Empty leaves it to the CLI;
	// like the model it is fixed at creation and carried across a restore.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// DoubleMemory selects the workload container's doubled memory
	// request/limit over the platform default, fixed at creation and carried
	// across snapshot/restore exactly like Model and ReasoningEffort. False
	// (the omitted default) keeps every client's existing behavior.
	DoubleMemory bool `json:"doubleMemory,omitempty"`
	// Tenant is the tenant that owns the session (docs/prd/multitenancy.md,
	// AC-K2), fixed at creation. Empty means operator-owned: sessions created
	// before tenancy existed, or by the operator without naming a tenant.
	Tenant string `json:"tenant,omitempty"`
	Name   string `json:"name"`
	State  State  `json:"state"`
	// Pod is the session's dedicated *workload* pod — the one running the
	// workload its type selects. It is the 1:1 subject of AC-A2 and is empty
	// when the session is snapshotted/reclaimed.
	Pod string `json:"pod,omitempty"`
	// AuxiliaryPods names the session-scoped pods that serve the workload pod
	// without running the workload themselves (AC-A2's auxiliary-pod clause;
	// AC-F4's helper pod is the first of them). They are session-exclusive,
	// share the workload pod's lifetime, and are reclaimed with it (AC-A3) —
	// so this is empty exactly when Pod is. shell and claude-code provision
	// none, leaving it absent from the wire; a type behind the approval gate
	// (HasApprovalGate) provisions exactly one — its helper pod.
	AuxiliaryPods []string    `json:"auxiliaryPods,omitempty"`
	CreatedAt     time.Time   `json:"createdAt"`
	LastAccess    time.Time   `json:"lastAccess"`           // last read/write; drives idle/snapshot timing (AC-B1)
	Checkpoint    *Checkpoint `json:"checkpoint,omitempty"` // present only when State == StateSnapshot
	// SnapshotTransaction is internal recovery metadata, never part of the API.
	// The ConfigMap adapter deliberately encodes it in its durable representation
	// even though ordinary JSON marshaling omits it.
	SnapshotTransaction *SnapshotTransaction `json:"-"`
}

// IdleFor returns how long the session has been without a read/write as of now.
func (s *Session) IdleFor(now time.Time) time.Duration {
	return now.Sub(s.LastAccess)
}

// Pods returns every pod that belongs to the session, workload pod first,
// skipping empty names. Lifecycle code that reclaims, restores or deletes a
// session works on this set rather than on Pod alone: AC-A3's reclamation and
// AC-B1/AC-B2's freeze and restore hold across the workload pod *and* its
// auxiliary pods (AC-F4). A snapshotted session has no pods, so this is empty.
func (s *Session) Pods() []string {
	pods := make([]string, 0, 1+len(s.AuxiliaryPods))
	if s.Pod != "" {
		pods = append(pods, s.Pod)
	}
	for _, p := range s.AuxiliaryPods {
		if p != "" {
			pods = append(pods, p)
		}
	}
	return pods
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
	// ErrUnauthenticated and ErrForbidden are the tenancy boundary's errors
	// (AC-K1, AC-K3): a bearer token that does not prove a ServiceAccount, and
	// a proven caller asking for something its tenant may not have.
	// ErrWorkloadBusy reports an operation that runs only between turns —
	// a plugin refresh (AC-J7) — asked for while one runs or waits.
	ErrWorkloadBusy = errors.New("workload is busy")
	// ErrPluginRefreshFailed reports a plugin marketplace the session could not
	// fetch again; the skills it had are kept (AC-J7).
	ErrPluginRefreshFailed = errors.New("plugin refresh failed")
	// ErrAuthPoolExhausted reports that every login slot of the session type's
	// auth pool is held, so no pod was made; a restore leaves the session
	// frozen (AC-N2).
	ErrAuthPoolExhausted = errors.New("auth pool exhausted")

	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)
