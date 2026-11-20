// The primitives the work records are written with: the id shape, the error
// classes a caller distinguishes, the live session relations the parent keeps
// beside a task, and the bounds a read outside the session is served under.
//
// They used to live in the ledger's own file, because the ledger was the only
// thing that had records. The ledger is gone (AC-N10) and these are not: an id
// shape and an error class are facts about this database, not about the records
// that happened to be in it first.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// workID is the id shape every record here uses, caller-generated so that an
// ambiguous first answer can be retried with the same id.
var workID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// workError is a refusal a caller acts on, by code: invalid_argument,
// not_found, conflict, schema_version, stale_choice or storage_error. A storage
// error never carries SQL or a payload.
type workError struct {
	Code      string `json:"code"`
	ErrorText string `json:"error"`
}

func (e workError) Error() string         { return e.ErrorText }
func workFail(code, message string) error { return workError{code, message} }

// workBookSession is one session relation in the parent's bookkeeping beside a
// task: which session, what it was for, how far the parent has read it
// (nextOffset, seq) and whether it is still live. It is the parent's own record
// — the agreed task carries a single session_id and nothing else — and it is
// what lets the parent pick up where it left off after collecting a worker's
// result.
type workBookSession struct {
	ID         string `json:"id"`
	Ownership  string `json:"ownership"`
	Purpose    string `json:"purpose"`
	NextOffset int64  `json:"nextOffset"`
	Seq        *int64 `json:"seq,omitempty"`
	EndSeq     *int64 `json:"end_seq,omitempty"`
	Cleanup    string `json:"cleanup"`
	Reason     string `json:"reason,omitempty"`
	NextAction string `json:"next_action,omitempty"`
}

// workClock is the clock the write paths outside a tool call read, replaced in
// tests so a stored request's time is not the test's own wall clock.
var workClock = time.Now

const (
	// requestSafeInt is the largest integer a JSON reader can be trusted to
	// carry unchanged. Versions, seqs and turn ids are refused above it rather
	// than silently rounded by a caller.
	requestSafeInt = 9007199254740991

	// dispatchIntended and dispatchDispatched are the two things a parent can
	// have recorded about an outside action: decided, and handed over. The
	// second cannot go back to the first — erasing the fact that something left
	// makes a later reader believe nothing did.
	dispatchIntended   = "intended"
	dispatchDispatched = "dispatched"

	// staleChoiceMessage is what a pick from a screen the task has moved past is
	// told. It is its own code (stale_choice) so the app can say "this screen is
	// old" and keep the words the person already typed.
	staleChoiceMessage = "that choice is no longer offered for this task"
)

// newClaimToken mints the proof that one turn is this parent's to act on. A
// timestamp is not an identity — two parents that both read a turn would hold
// the same one — so ownership is a token the holder has to present
// (session_work_turns.go).
func newClaimToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

const (
	// workViewCall bounds one hop in front of the sidecar. The sidecar bounds
	// the statement itself (sessionDBCallTimeout); this is kept short so a panel
	// read can never hold the agent open.
	workViewCall = 35 * time.Second

	// maxWorkViewBytes bounds a relayed page. The sidecar caps a page itself, so
	// this only guards against a body that is not what this code served.
	maxWorkViewBytes = 1 << 20
)

// workReporter is what a workload exposes to the relay: where this slot's
// sidecar is. A workload type with no session database has none, which is how
// the relay answers "this session keeps no work records" rather than an empty
// page.
type workReporter interface {
	sessionDBMCPURL() string
}

// workViewLimit reads the page size. An absent limit leaves the default; a
// limit that is not a number in range is refused rather than clamped, so a
// caller is never silently given a different page than it asked for.
func workViewLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > workListLimitMax {
		return 0, errors.New("limit must be a number from 1 to 100")
	}
	return limit, nil
}

func writeWorkViewError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": message})
}

// workViewStatus maps a refusal onto the status a reader outside the session
// acts on. It is one function so that every view — tasks, turns, a stored
// request — classifies the same refusal the same way.
func workViewStatus(code string) int {
	switch code {
	case "not_found":
		return http.StatusNotFound
	case "conflict", "schema_version", "stale_choice":
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}
