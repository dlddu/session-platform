// The agreed work records as they leave the control plane (AC-N9, AC-N10): a
// task row, a turn row, and the one write — a person's words, which become the
// next turn of a task.
//
// These replaced the ledger shapes rather than sitting beside them. The ledger
// page had nowhere to put what the agreed model holds — a task's description
// and completion criteria, and a turn's instruction, the session's own final
// answer, the parent's explanation, its question and the artifacts — so a
// caller that only had it could show that work exists but never the work
// itself. Keeping both would have meant two shapes for one truth, and the
// slot's ledger is gone (AC-N10): there is nothing behind the second shape to
// read.
package session

import "errors"

var (
	// ErrWorkUnsupported is a session whose type keeps no work records. Only
	// codex-chat gets a session database.
	ErrWorkUnsupported = errors.New("this workload keeps no work records")
	// ErrWorkUnavailable is a session that keeps work records but has no live
	// pod to read its slot through. It is distinct from empty work on purpose:
	// the rows are still on the slot's volume, so answering "no tasks" would
	// claim there is no work. Reading never starts a pod — that would make
	// opening a panel restore a snapshot.
	ErrWorkUnavailable = errors.New("the session has no live pod to read its work records through")
	// ErrWorkChoiceStale is a pick from a turn the task has moved past. It is
	// its own class because the caller's answer is different: the screen is out
	// of date, so re-reading the task is what helps, and the words the person
	// already typed are still good.
	ErrWorkChoiceStale = errors.New("that offered choice is no longer current")
)

// WorkTask is one task: who has it, what it is, how it will be known to be
// done, where it stands, and the version a writer has to hold to change it.
//
// A nil SessionID is "not assigned to a session". A nil Description,
// CompletionCriteria, CreatedAt or UpdatedAt is "not recorded" — which is what
// a task migrated from a ledger that never kept the field carries, and is
// deliberately not an empty string: the app says nothing is recorded rather
// than showing a blank as if it were the answer.
type WorkTask struct {
	ID                 string  `json:"id"`
	SessionID          *string `json:"session_id"`
	Title              string  `json:"title"`
	Description        *string `json:"description"`
	CompletionCriteria *string `json:"completion_criteria"`
	Status             string  `json:"status"`
	Version            int64   `json:"version"`
	CreatedAt          *string `json:"created_at"`
	UpdatedAt          *string `json:"updated_at"`
	// StoredStatus is the word the slot actually holds, carried only when Status
	// is not it — which today means a task an older image wrote as `blocked`,
	// whose status the slot resolved from evidence beside it. StatusBasis names
	// the field that decided. Both are absent for every task written since, and
	// neither is a column of the agreed record: they are here so that a
	// resolution made about somebody's work can be checked rather than taken on
	// trust (AC-N13).
	StoredStatus string `json:"stored_status,omitempty"`
	StatusBasis  string `json:"status_basis,omitempty"`
}

// WorkChoice is one sentence a turn offers for the next one. It is a sentence
// to send, not a permission: it grants no tool approval and no merge.
type WorkChoice struct {
	Label       string `json:"label"`
	RequestText string `json:"request_text"`
}

// WorkTurn is one exchange on a task. Request is the person's words verbatim;
// Instruction is what the parent passed to the session doing the work;
// Response is that session's own final answer, never a summary the parent
// wrote — that is Explanation. A nil field is one that has not happened or was
// never recorded, so a reader shows the turn as far as it has got.
type WorkTurn struct {
	ID          int64        `json:"id"`
	TaskID      string       `json:"task_id"`
	SessionSeq  *int64       `json:"session_seq"`
	Request     *string      `json:"request"`
	Instruction *string      `json:"instruction"`
	Response    *string      `json:"response"`
	Explanation *string      `json:"explanation"`
	Question    *string      `json:"question"`
	Choices     []WorkChoice `json:"choices"`
	Artifacts   []string     `json:"artifacts"`
	RequestedAt string       `json:"requested_at"`
	StartedAt   *string      `json:"started_at"`
	EndedAt     *string      `json:"ended_at"`
	// Origin is where this turn came from: empty for one the session wrote,
	// "user_request" for one migrated from a ledger request, "app_request" for
	// one a person wrote through the request route after the cutover. It
	// carries no claim token, operation key or dispatch state — what the parent
	// handed to another system is its own bookkeeping.
	Origin string `json:"origin,omitempty"`
}

// WorkTaskRecord is one task with the two facts a reader would otherwise need
// a call per task for. Migrated says the task came from the ledger, which is
// how a reader knows the ledger also holds history and parent bookkeeping about
// it; a task created after the cutover has neither and is not shown as if it
// did.
type WorkTaskRecord struct {
	Task       WorkTask  `json:"task"`
	TurnsTotal int64     `json:"turns_total"`
	LatestTurn *WorkTurn `json:"latest_turn,omitempty"`
	Migrated   bool      `json:"migrated,omitempty"`
}

// WorkTaskPage is one keyset page of work tasks. NextCursor is the id to
// continue after and is empty on the last page; a page is not a snapshot, so a
// row can repeat across pages and a reader dedupes by id.
type WorkTaskPage struct {
	Tasks      []WorkTaskRecord `json:"tasks"`
	NextCursor string           `json:"next_cursor"`
}

// WorkTurnPage is one keyset page of one task's turns, oldest id first.
// TaskVersion is the version those turns were read against, so a caller that
// then writes holds the value the slot will check.
type WorkTurnPage struct {
	TaskID      string `json:"task_id"`
	TaskVersion int64  `json:"task_version"`
	// Task travels with its turns, so a reader showing one task has its
	// description and completion criteria beside the exchanges rather than
	// needing a second call that could land on a different version.
	Task       WorkTask   `json:"task"`
	Turns      []WorkTurn `json:"turns"`
	NextCursor int64      `json:"next_cursor,omitempty"`
}

// WorkPageLimit bounds one page of either kind, matching the work records' own
// cap.
const (
	WorkPageLimitDefault = 20
	WorkPageLimitMax     = 100
)

// WorkRequestTextMax bounds one request's words. It is a person's sentence
// about one task; the caller's own limit is expected to be smaller.
const WorkRequestTextMax = 8192

// WorkRequestInput is one person's request to store. It becomes the next turn
// of the task — a person's words are an exchange on the work, not a row beside
// it, which is why there is no separate request record to read back.
//
// ID is the caller's own stable id and the whole idempotency contract: the same
// id with the same task and text stores nothing twice, and the same id with
// different content is a conflict rather than a second turn.
//
// Author is the caller's own derivation from whatever boundary authenticated
// the person. It is bookkeeping, exactly like a task's owner: nothing here
// grants a tool, an approval or a merge on the strength of it.
type WorkRequestInput struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
	Author string `json:"author"`
	// ChoiceTurnID, when set, is the turn whose offered choices the person
	// picked from. The slot refuses a pick from a turn that is no longer the
	// task's latest, so a screen left open while the task moved on cannot act
	// on it. Free text leaves it nil and is never refused for staleness.
	//
	// It replaced the ledger's (choice_id, basis_version) pair: what makes a
	// pick current is the turn it was offered on still being the latest, and
	// a version carried beside a choice id was a second way of saying that
	// which could disagree with the first.
	ChoiceTurnID *int64 `json:"choice_turn_id,omitempty"`
}

// WorkRequestStored is the answer to storing one: the turn the request became.
// Replayed is true when the id was already stored with the same content, which
// is what makes a caller's retry of an answer it never saw safe.
type WorkRequestStored struct {
	Turn     WorkTurn `json:"turn"`
	Replayed bool     `json:"replayed"`
}

// WorkStatuses is the task status domain: the ledger's own with the two
// statuses that meant one thing folded into one. `review` is where a task sits
// when a person has to settle something, and `blocked` is not here because it
// never said anything `review` did not (AC-N13).
//
// Folding is not renaming, and no status is added in its place. Keep in step
// with the slot's own domain (data-plane/cmd/agent/session_work.go,
// workStatuses) — a status the slot writes and this filter refuses would be
// work nobody out here can select.
var WorkStatuses = []string{"planned", "running", "review", "completed", "unknown"}

// WorkStatusBlockedLegacy is the word an older image wrote for what `review`
// now holds. It is accepted on every filter as an alias and is in no domain
// list: a filter somebody saved while it was a status still finds the work it
// was saved to find.
const WorkStatusBlockedLegacy = "blocked"

// IsWorkStatus reports whether raw is a status the work records use. A filter
// outside the domain is refused rather than answered with an empty page, which
// would read as "no such work" instead of "no such status".
func IsWorkStatus(raw string) bool {
	for _, candidate := range WorkStatuses {
		if candidate == raw {
			return true
		}
	}
	return false
}

// IsWorkStatusFilter reports whether raw is a status filter this API accepts:
// the domain, or the legacy alias.
//
// The alias is deliberately **not** resolved here, and the caller's own word is
// what travels to the slot. A slot running an image older than this one still
// has `blocked` as a status of its own, and sending it `review` instead would
// miss exactly the rows the caller asked for; sending its own word back gets
// the right answer out of either image. The slot this image talks to resolves
// the alias itself (data-plane/cmd/agent/session_work_review.go).
func IsWorkStatusFilter(raw string) bool {
	return raw == WorkStatusBlockedLegacy || IsWorkStatus(raw)
}
