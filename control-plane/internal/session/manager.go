package session

import "context"

// CreateRequest is the input to Manager.Create.
type CreateRequest struct {
	Name string
}

// ReadResult is the state-branched result of a Read (AC-C2).
type ReadResult struct {
	Session *Session
	// Path records which state branch served the read (e.g. "active",
	// "idle->read", "snapshot->restore"). It exists so the scaffolding can
	// assert dispatch behaviour before the real read paths are implemented.
	Path string
	// Payload is the (stub) session data. Real reads return workload data.
	Payload string
}

// WriteResult is the state-branched result of a Write (AC-C3).
type WriteResult struct {
	Session *Session
	Path    string
}

// Manager is the primary port the API depends on. It owns session lifecycle
// and orchestration, delegating to the adapter ports. This is the
// "SessionManager" of the design docs.
//
// AC mapping:
//   - Create    → AC-A1, AC-A2 (provision one dedicated pod, go active).
//   - Get/List  → V5 (single source of truth for session state).
//   - Read      → AC-C2 (state-branched read).
//   - Write     → AC-C3 (state-branched write).
//   - Switch    → AC-C4 (free switching; restore snapshot, no-op if active).
//   - Snapshot  → AC-B1 (checkpoint + reclaim on idle).
//   - Restore   → AC-B2 (restore checkpoint into a new pod).
//   - Terminate → AC-A3 (reclaim resources).
type Manager interface {
	Create(ctx context.Context, req CreateRequest) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)
	Read(ctx context.Context, id string) (*ReadResult, error)
	Write(ctx context.Context, id, payload string) (*WriteResult, error)
	Switch(ctx context.Context, id string) (*Session, error)
	Snapshot(ctx context.Context, id string) (*Session, error)
	Restore(ctx context.Context, id string) (*Session, error)
	Terminate(ctx context.Context, id string) error
}
