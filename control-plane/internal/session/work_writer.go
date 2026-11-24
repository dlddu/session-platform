// Who writes a slot's work records, as the control plane sees it.
//
// A session writes its own until a monitor is bound to it, and from then on the
// monitor does. The fact lives in the slot, beside the records it governs, for
// one reason: a session that was already running when the pair was made cannot
// be told its role by the environment its pod was started with, and waiting for
// a configuration change to reach it would mean guessing when the old writer
// stopped. Asking the slot is not a guess — the answer and every write to those
// records are the same database (data-plane/cmd/agent/session_work_seal.go).
package session

// WorkWriter is the slot's answer: which side writes, since when, and at which
// epoch. The epoch is what makes a handover orderable and repeatable — a caller
// states the epoch it read, so a retry cannot hand the records over twice and a
// stale caller cannot undo a handover it never saw.
type WorkWriter struct {
	// Writer is WorkWriterSession or WorkWriterMonitor.
	Writer string `json:"writer"`
	Epoch  int64  `json:"epoch"`
	// Holder is the monitor session holding the records, empty when the session
	// writes its own.
	Holder    string `json:"holder,omitempty"`
	ChangedAt string `json:"changed_at,omitempty"`
}

const (
	// WorkWriterSession is a session that writes its own records: every session
	// with no monitor, which is every session on a deployment without the pair.
	WorkWriterSession = "session"
	// WorkWriterMonitor is a session whose bound monitor writes them.
	WorkWriterMonitor = "monitor"
)

// Sealed reports whether the session's own writes are closed.
func (w WorkWriter) Sealed() bool { return w.Writer == WorkWriterMonitor }

// WorkSeal is a handover, or its rollback.
type WorkSeal struct {
	Writer string `json:"writer"`
	Holder string `json:"holder"`
	// ExpectedEpoch is the epoch the caller read. A mismatch is refused rather
	// than overwritten: somebody else moved the seal in between, and which way
	// matters.
	ExpectedEpoch *int64 `json:"expected_epoch,omitempty"`
}

// WorkSealResult is what the slot did. Changed is false for a seal that was
// already in place, which is what a retried handover must look like.
type WorkSealResult struct {
	Writer  WorkWriter `json:"writer"`
	Changed bool       `json:"changed"`
}

// WorkCallResult is one work tool call's own answer, passed through as the slot
// gave it: the tool result and whether it was a refusal. The caller reads the
// same codes the session's own model reads.
type WorkCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// Text is the tool result's body, which is JSON the caller decodes.
func (r WorkCallResult) Text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}
