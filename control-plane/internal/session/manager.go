package session

import (
	"context"
	"io"
)

// CreateRequest is the input to Manager.Create.
type CreateRequest struct {
	Name string
	// WorkloadType selects the data plane workload; empty creates a shell session.
	WorkloadType WorkloadType
	// Model selects the Claude Code model; empty resolves to PlatformDefaultModel.
	Model string
}

// ReadResult is the state-branched result of a Read.
type ReadResult struct {
	Session *Session
	// Path records which state branch served the read.
	Path string
	// Payload is workload output accumulated after the requested offset.
	Payload string
	// NextOffset is the cursor for the next Read; offset 0 replays full history.
	NextOffset int64
}

// WriteResult is the state-branched result of a Write.
type WriteResult struct {
	Session *Session
	Path    string
}

// Manager is the primary port the API depends on. It owns session lifecycle
// and orchestration, delegating to the adapter ports.
type Manager interface {
	Create(ctx context.Context, req CreateRequest) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)
	Read(ctx context.Context, id string, offset int64) (*ReadResult, error)
	Stream(ctx context.Context, id string, offset int64) (io.ReadCloser, error)
	Write(ctx context.Context, id, payload string) (*WriteResult, error)
	Switch(ctx context.Context, id string) (*Session, error)
	Snapshot(ctx context.Context, id string) (*Session, error)
	Restore(ctx context.Context, id string) (*Session, error)
	Terminate(ctx context.Context, id string) error
}
