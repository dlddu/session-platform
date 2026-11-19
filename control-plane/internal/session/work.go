// The agreed work records as they leave the control plane (AC-N9): a task row
// and a turn row, and nothing else.
//
// These exist beside the ledger types rather than replacing them because the
// ledger shapes cannot carry what the agreed model holds — a task's description
// and completion criteria, and a turn's instruction, the session's own final
// answer, the parent's explanation, its question and the artifacts. A caller
// that only had the ledger page could show that work exists but never the work
// itself.
package session

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
	TaskID      string     `json:"task_id"`
	TaskVersion int64      `json:"task_version"`
	Turns       []WorkTurn `json:"turns"`
	NextCursor  int64      `json:"next_cursor,omitempty"`
}

// WorkPageLimit bounds one page of either kind, matching the work records' own
// cap.
const (
	WorkPageLimitDefault = 20
	WorkPageLimitMax     = 100
)

// WorkStatuses is the task status domain, the ledger's own unchanged: a
// migration that renamed a status would move work a person had already seen as
// blocked or completed.
var WorkStatuses = []string{"planned", "running", "blocked", "review", "completed", "unknown"}

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
