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
	// Requests counts the user requests waiting on this task, absent when
	// none ever arrived. A reader of the page therefore learns that someone
	// asked something without a second call.
	Requests *LedgerRequestSummary `json:"requests,omitempty"`
	// Choices are what the session offers a person to say about this task,
	// written against this very version — a task that has moved serves none.
	Choices []LedgerChoice `json:"choices,omitempty"`
}

// LedgerChoice is one sentence the session offers for this task. The app shows
// the label, fills the request box with RequestText and sends the id back, so
// the session can tell which of its own sentences came back. It grants no tool,
// approval or merge.
type LedgerChoice struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	RequestText  string `json:"request_text"`
	BasisVersion int64  `json:"basis_version"`
	CreatedAt    string `json:"created_at"`
}

// LedgerTaskEvent is one line of a task's history: a named fact, written with
// the change it describes. It is not an agent log — no credential, token or
// transcript reaches it.
type LedgerTaskEvent struct {
	Seq       int64          `json:"seq"`
	TaskID    string         `json:"task_id"`
	At        string         `json:"at"`
	Kind      string         `json:"kind"`
	Actor     string         `json:"actor"`
	Summary   string         `json:"summary"`
	Detail    map[string]any `json:"detail,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
}

// LedgerTaskEventPage is one keyset page of a task's history. NextCursor is the
// seq to continue after and is zero on the last page.
type LedgerTaskEventPage struct {
	Events     []LedgerTaskEvent `json:"events"`
	NextCursor int64             `json:"next_cursor,omitempty"`
}

// LedgerEventPageLimit bounds one history page, as the task page is bounded.
const (
	LedgerEventPageLimitDefault = 20
	LedgerEventPageLimitMax     = 100
)

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

// A user request on a ledger task: what a person asked about one task, stored
// on the slot beside the task rather than inside it, so storing one leaves
// every task field — details, approval, cursors — as the session wrote them.
//
// The control plane stores it and never acts on it. It starts no turn, wakes
// nothing and decides nothing: the session picks the request up the next time
// it reads its own ledger, and the request's status is what says whether that
// has happened.

// LedgerRequestLifecycle values a stored request passes through. Only the
// session's own ack reaches processed or failed; reconciling is a claim that
// lapsed after a dispatch had been recorded, which is a state a person or the
// next parent has to resolve rather than retry blindly.
const (
	LedgerRequestPending     = "pending"
	LedgerRequestClaimed     = "claimed"
	LedgerRequestReconciling = "reconciling"
	LedgerRequestProcessed   = "processed"
	LedgerRequestFailed      = "failed"
)

// LedgerRequestTextMax bounds one request's words. It is a person's sentence
// about one task; the caller's own limit is expected to be smaller.
const LedgerRequestTextMax = 8192

// LedgerRequest is one stored request as every reader sees it. Status,
// dispatch state and result together are the whole answer to "has this been
// dealt with" — no reader has to infer it from the task document.
type LedgerRequest struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Seq       int64  `json:"seq"`
	Version   int64  `json:"version"`
	Text      string `json:"text"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`
	// ClaimOwner is which parent holds it; the claim's token never leaves the
	// slot, so no reader outside the session can settle someone else's claim.
	ClaimOwner      string `json:"claim_owner,omitempty"`
	ClaimExpiresAt  string `json:"claim_expires_at,omitempty"`
	OperationKey    string `json:"operation_key,omitempty"`
	OperationTarget string `json:"operation_target,omitempty"`
	DispatchState   string `json:"dispatch_state,omitempty"`
	DispatchedAt    string `json:"dispatched_at,omitempty"`
	RelatedSeq      *int64 `json:"related_seq,omitempty"`
	Result          string `json:"result,omitempty"`
	SettledAt       string `json:"settled_at,omitempty"`
}

// LedgerRequestInput is one request to store. ID is the caller's own stable id
// and the whole idempotency contract: the same id with the same task and text
// stores nothing twice, and the same id with different content is a conflict
// rather than a second request.
//
// Author is the caller's own derivation from whatever boundary authenticated
// the person. It is bookkeeping, exactly like a task's owner: nothing in the
// ledger grants a tool, an approval or a merge on the strength of it.
type LedgerRequestInput struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
	Author string `json:"author"`
	// ChoiceID, when set, is the session's own offered choice the person
	// picked; empty means they typed their own words.
	ChoiceID string `json:"choice_id,omitempty"`
}

// LedgerRequestStored is the answer to storing one. Replayed is true when the
// id was already stored with the same content, which is what makes a caller's
// retry of an answer it never saw safe.
type LedgerRequestStored struct {
	Request  LedgerRequest `json:"request"`
	Replayed bool          `json:"replayed"`
}

// LedgerRequestSummary rides along with a task in a ledger page so a reader
// sees that work is waiting without asking a second question.
type LedgerRequestSummary struct {
	Pending     int   `json:"pending"`
	Claimed     int   `json:"claimed"`
	Reconciling int   `json:"reconciling"`
	LastSeq     int64 `json:"last_seq"`
}
