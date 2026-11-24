package session

import (
	"encoding/json"
	"errors"
	"time"
)

// A claude-code turn's tool calls, decided one by one by the session owner
// while the turn runs (docs/prd/claude-code-workload.md AC-E2, 「턴 중 도구
// 승인」). The data plane agent holds the calls and binds each decision to the
// call it was taken on; the control plane only relays them, and is what checks
// that the caller owns the session.

// PendingToolApproval is one tool call the running turn waits on. The owner
// decides it as it stands: the arguments are the model's own, and a decision
// carries back the id, the argument hash and the epoch, so it cannot land on
// any other call or on another run of the session.
type PendingToolApproval struct {
	ID    string          `json:"id"`
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	// ArgsHash names this exact call — a sha256 over the tool and the
	// arguments the model asked for.
	ArgsHash string `json:"argsHash"`
	// ToolUseID is the SDK's own id for the call, for a reader matching it
	// against the turn's output.
	ToolUseID string `json:"toolUseId"`
	// Epoch names the run of the session that put the call up.
	Epoch     string    `json:"epoch"`
	AskedAt   time.Time `json:"askedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ToolApprovalPage is what the session owner reads: the run's epoch and the
// calls waiting in it, oldest first.
type ToolApprovalPage struct {
	Epoch     string                `json:"epoch"`
	Approvals []PendingToolApproval `json:"approvals"`
}

// Tool approval decisions. Nothing else is a decision: the platform policy's
// own denials are never put to a person, so there is no decision that turns
// one into an allow.
const (
	ToolApprovalApprove = "approve"
	ToolApprovalDeny    = "deny"
)

// ToolApprovalDecision is one decision on one call. Decision, ArgsHash and
// Epoch are required: a decision that does not name the exact call it was
// taken on is refused rather than applied to whatever is waiting.
type ToolApprovalDecision struct {
	Decision string `json:"decision"`
	ArgsHash string `json:"argsHash"`
	Epoch    string `json:"epoch"`
	Reason   string `json:"reason"`
	// DecidedBy is who the control plane saw take the decision, kept in the
	// session's record. It is the control plane's word, not the caller's.
	DecidedBy string `json:"decidedBy"`
}

// Valid reports whether the decision names a decision and the call it was
// taken on.
func (d ToolApprovalDecision) Valid() bool {
	if d.Decision != ToolApprovalApprove && d.Decision != ToolApprovalDeny {
		return false
	}
	return d.ArgsHash != "" && d.Epoch != ""
}

// WriteOptions is what the caller chose for the one turn a write queues
// (AC-E2). The zero value is the turn the platform runs by default.
type WriteOptions struct {
	// ToolApprovals asks for every tool call of the turn that the platform
	// policy allows to wait for the session owner's decision. The policy's
	// denials stay denials: they are never put to a person.
	ToolApprovals bool
	// Lane is which conversation of a manager pod this write is for (AC-N11).
	// The zero value is the person's chat, so a caller that knows nothing about
	// lanes writes where it always did.
	Lane Lane
}

var (
	// ErrToolApprovalNotFound is a call that is not waiting: never asked,
	// already decided, closed with its turn, or past its window.
	ErrToolApprovalNotFound = errors.New("tool call not found or no longer waiting")
	// ErrToolApprovalStale is a decision taken on another call or on another
	// run of the session — the call the owner read is not the one in front of
	// the turn now.
	ErrToolApprovalStale = errors.New("the decision was taken on another call or an earlier run")
	// ErrToolApprovalsUnsupported is a write asking for the owner's decision
	// on a workload that cannot put its tool calls to a person.
	ErrToolApprovalsUnsupported = errors.New("this workload cannot put its tool calls to the session owner")
)
