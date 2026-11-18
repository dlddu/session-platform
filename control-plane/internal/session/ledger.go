package session

import "errors"

// The chat slot's task ledger, read only (docs/prd/agent-auth-pool.md AC-N6).
// The ledger is the session database's own bookkeeping, written by the session
// through its `db` MCP; the control plane never writes it and offers no SQL.
// It belongs to the *slot*, not to the session, so it carries over to whichever
// chat session holds the slot next — which is why a read is routed through the
// pod holding the slot now rather than through any record of the session.

// LedgerTaskSession is one session a ledger task has used. A task collects
// these as it goes, so the last entry is the state the task is in now and the
// earlier ones are its history.
type LedgerTaskSession struct {
	ID         string `json:"id"`
	Ownership  string `json:"ownership"`
	Purpose    string `json:"purpose"`
	Cleanup    string `json:"cleanup"`
	Reason     string `json:"reason,omitempty"`
	NextAction string `json:"next_action,omitempty"`
}

// LedgerTask is one task as the ledger holds it. Details is the task's bounded
// JSON — the ledger schema has no column for a task's kind, so a reader that
// cares reads it from here.
type LedgerTask struct {
	Title    string              `json:"title"`
	Status   string              `json:"status"`
	Stage    string              `json:"stage"`
	Owner    string              `json:"owner"`
	Sessions []LedgerTaskSession `json:"sessions"`
	Details  map[string]any      `json:"details,omitempty"`
}

// LedgerTaskRecord is one ledger row: its id, the version that row is at, and
// the task itself.
type LedgerTaskRecord struct {
	ID      string     `json:"id"`
	Version int64      `json:"version"`
	Task    LedgerTask `json:"task"`
}

// LedgerTaskPage is one keyset page. NextCursor is the id to continue after and
// is empty on the last page; a page is not a snapshot, so a row can repeat
// across pages and a reader dedupes by id.
type LedgerTaskPage struct {
	Tasks      []LedgerTaskRecord `json:"tasks"`
	NextCursor string             `json:"next_cursor"`
}

// LedgerTaskPageLimit bounds one page, matching the ledger's own cap.
const (
	LedgerTaskPageLimitDefault = 20
	LedgerTaskPageLimitMax     = 100
)

var (
	// ErrLedgerUnsupported is a session whose type keeps no task ledger. Only
	// codex-chat gets a session database.
	ErrLedgerUnsupported = errors.New("this workload keeps no task ledger")
	// ErrLedgerUnavailable is a session that keeps a ledger but has no live pod
	// to read its slot through. It is distinct from an empty ledger on purpose:
	// the rows are still on the slot's volume, so answering "no tasks" would
	// claim there is no work. Reading never starts a pod — that would make
	// opening a panel restore a snapshot.
	ErrLedgerUnavailable = errors.New("the session has no live pod to read its task ledger through")
)
