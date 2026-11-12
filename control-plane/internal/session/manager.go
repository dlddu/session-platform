package session

import (
	"context"
	"io"
)

type CreateRequest struct {
	Name string
	// WorkloadType selects the data plane workload (AC-E1). Empty means
	// unspecified and creates a shell session; an unknown value is rejected
	// with ErrInvalidInput (the API maps that to 400).
	WorkloadType WorkloadType
	// Model selects the Claude Code model for this session (AC-E6). Empty means
	// PlatformDefaultModel for claude-code and is invalid when supplied for a
	// shell session. It is immutable after creation.
	Model string
	// Plugins selects the marketplace plugins (AC-L1). nil means the workload
	// type's platform default; a non-nil list replaces it and is validated by
	// NormalizePlugins. It is immutable after creation.
	Plugins []string
	// ReasoningEffort is the session's reasoning effort (AC-E8). Empty leaves
	// it unset; NormalizeReasoningEffort decides which types take one.
	ReasoningEffort string
	// Tenant names the owning tenant (AC-K2). The API fills it from the
	// caller; the manager stores it verbatim.
	Tenant string
}

// ReadResult is the state-branched result of a Read (AC-C2).
type ReadResult struct {
	Session *Session
	// Path records which state branch served the read (e.g. "active",
	// "idle->active->read", "snapshot->restore->read").
	Path string
	// Payload is workload output accumulated after the requested offset
	// (stdout/stderr merged, order preserved — AC-D3/AC-E3).
	Payload string
	// NextOffset is the cursor to pass as offset on the next Read to receive
	// only new output; offset 0 replays the full history (AC-D3/AC-E3).
	NextOffset int64
}

// WriteResult is the state-branched result of a Write (AC-C3).
type WriteResult struct {
	Session *Session
	Path    string
}

// PluginChanges is what a plugin refresh did inside the session (AC-J7):
// State is "ready", or "unconfigured" when the session has no marketplace;
// the lists name managed skill directories.
type PluginChanges struct {
	State    string   `json:"state"`
	Revision string   `json:"revision,omitempty"`
	Added    []string `json:"added"`
	Updated  []string `json:"updated"`
	Removed  []string `json:"removed"`
}

// PluginRefreshResult is the state-branched result of RefreshPlugins.
type PluginRefreshResult struct {
	Session *Session
	Path    string
	Changes PluginChanges
}

// Manager is the primary port the API depends on. It owns session lifecycle
// and orchestration, delegating to the adapter ports.
//
// AC mapping:
//   - Create    → AC-A1, AC-A2 (provision one dedicated pod, go active),
//     AC-E1 (workload type selection: default shell, immutable afterwards).
//   - Get/List  → V5 (single source of truth for session state).
//   - Read      → AC-C2 plus the workload output cursor (AC-D3/AC-E3).
//   - Stream    → passive, cursor-resumable live workload output. It never
//     restores a snapshot or refreshes idle activity by itself.
//   - Write     → AC-C3 plus shell stdin or queued agent prompt (AC-D2/AC-E2).
//   - Switch    → AC-C4 (free switching; restore snapshot, no-op if active).
//   - Snapshot  → AC-B1 (workload archive + reclaim on idle).
//   - Restore   → AC-B2 (restore the workload into a new pod).
//   - Terminate → AC-A3 (reclaim resources).
//   - Recover   → AC-B4 (put a deleted session back from its last archive).
//   - RefreshPlugins → AC-J7 (fetch a codex-chat or codex-task session's plugin skills again,
//     between turns, keeping its conversation).
type Manager interface {
	Create(ctx context.Context, req CreateRequest) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)
	Read(ctx context.Context, id string, offset int64) (*ReadResult, error)
	Stream(ctx context.Context, id string, offset int64) (io.ReadCloser, error)
	Write(ctx context.Context, id, payload string, opts WriteOptions) (*WriteResult, error)
	Switch(ctx context.Context, id string) (*Session, error)
	Snapshot(ctx context.Context, id string) (*Session, error)
	Restore(ctx context.Context, id string) (*Session, error)
	Terminate(ctx context.Context, id string) error
	RefreshPlugins(ctx context.Context, id string) (*PluginRefreshResult, error)
	// Recover calls admit with the recovered record before anything is
	// written, so the caller decides who may see it (AC-K2).
	Recover(ctx context.Context, id string, admit func(*Session) error) (*Session, error)
}
