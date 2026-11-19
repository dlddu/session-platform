package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// StubClient is an in-memory Client for tests: Write appends the payload to a
// per-pod buffer and Read serves offset deltas from it — the same cursor
// semantics as the real agent's scrollback (AC-D3), minus the shell actually
// running anything.
type StubClient struct {
	// SteerOutcome overrides what Steer answers (AC-J9); nil means applied.
	SteerOutcome *session.Steered

	// LedgerRows is the chat slot's task ledger as the stub holds it, in id
	// order (AC-N6). LedgerTasks pages over it with the same keyset rules the
	// sidecar uses, so a test reads pages rather than one canned answer.
	LedgerRows []session.LedgerTaskRecord
	// LedgerReads counts ledger pages served, so a test can show that reading
	// a panel never turned into something else.
	LedgerReads int
	// LedgerRequests is what callers stored, in arrival order, with the same
	// id-idempotency and task-link rules the sidecar applies.
	LedgerRequests []session.LedgerRequest
	// LedgerEventRows is one task's history as the stub holds it, keyed by task.
	LedgerEventRows map[string][]session.LedgerTaskEvent

	// WorkRows is the slot's agreed work records as the stub holds them, in id
	// order (AC-N9), and WorkTurnRows is each task's turns in turn-id order.
	// WorkTasks/WorkTurns page over them with the sidecar's own keyset rules,
	// so a test reads pages rather than one canned answer.
	WorkRows     []session.WorkTaskRecord
	WorkTurnRows map[string][]session.WorkTurn
	// WorkReads counts work pages served, so a test can show that reading the
	// records never turned into something else.
	WorkReads int

	mu      sync.Mutex
	bufs    map[string][]byte
	steered map[string][]string
}

var _ Client = (*StubClient)(nil)

// NewStubClient returns an empty stub.
func NewStubClient() *StubClient {
	return &StubClient{bufs: map[string][]byte{}}
}

func (c *StubClient) Write(_ context.Context, pod, payload string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bufs[pod] = append(c.bufs[pod], payload...)
	return nil
}

// RefreshPlugins reports a session with no marketplace: the stub has no
// sidecar to clone one.
func (c *StubClient) RefreshPlugins(context.Context, string) (session.PluginChanges, error) {
	return session.PluginChanges{State: "unconfigured", Added: []string{}, Updated: []string{}, Removed: []string{}}, nil
}

// LedgerTasks pages over LedgerRows the way the sidecar pages over the ledger
// (AC-N6): ids in order, after_id as the cursor, and an empty next_cursor only
// on the last page.
func (c *StubClient) LedgerTasks(_ context.Context, _ string, limit int, afterID string) (session.LedgerTaskPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LedgerReads++
	if limit <= 0 {
		limit = session.LedgerTaskPageLimitDefault
	}
	page := session.LedgerTaskPage{Tasks: []session.LedgerTaskRecord{}}
	for _, row := range c.LedgerRows {
		if afterID != "" && row.ID <= afterID {
			continue
		}
		if len(page.Tasks) == limit {
			page.NextCursor = page.Tasks[len(page.Tasks)-1].ID
			break
		}
		page.Tasks = append(page.Tasks, row)
	}
	return page, nil
}

// PendingQuestions reports none: the stub runs no turn that could ask.
func (c *StubClient) PendingQuestions(context.Context, string) ([]session.PendingQuestion, error) {
	return []session.PendingQuestion{}, nil
}

// Steer records the message and reports it applied, so a test can see the
// relay reach the pod. SteerOutcome lets a test ask for the rejection path
// instead — the one the real agent gives when no turn is running.
func (c *StubClient) Steer(_ context.Context, pod, text string) (session.Steered, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.steered == nil {
		c.steered = map[string][]string{}
	}
	c.steered[pod] = append(c.steered[pod], text)
	if c.SteerOutcome != nil {
		return *c.SteerOutcome, nil
	}
	return session.Steered{Status: session.SteerStatusApplied, TurnID: "stub-turn"}, nil
}

// Steered is what Steer was given for pod, in order.
func (c *StubClient) Steered(pod string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.steered[pod]...)
}

// AnswerQuestion finds nothing waiting, for the same reason.
func (c *StubClient) AnswerQuestion(context.Context, string, string, map[string]string) error {
	return session.ErrQuestionNotFound
}

func (c *StubClient) Read(_ context.Context, pod string, offset int64) (string, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := c.bufs[pod]
	n := int64(len(buf))
	if offset < 0 || offset >= n {
		return "", n, nil
	}
	return string(buf[offset:]), n, nil
}

func (c *StubClient) Stream(_ context.Context, pod string, offset int64) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := append([]byte(nil), c.bufs[pod]...)
	n := int64(len(buf))
	if offset < 0 {
		return nil, fmt.Errorf("negative stream offset")
	}
	var event bytes.Buffer
	if offset > n {
		body, _ := json.Marshal(struct {
			NextOffset int64 `json:"nextOffset"`
		}{NextOffset: n})
		_, _ = fmt.Fprintf(&event, "id: %d\nevent: reset\ndata: %s\n\n", n, body)
		return io.NopCloser(bytes.NewReader(event.Bytes())), nil
	}
	if offset == n {
		return io.NopCloser(bytes.NewBufferString(": stub stream idle\n\n")), nil
	}
	payload := buf[offset:]
	body, _ := json.Marshal(struct {
		Offset        int64  `json:"offset"`
		PayloadBase64 string `json:"payloadBase64"`
		NextOffset    int64  `json:"nextOffset"`
	}{
		Offset:        offset,
		PayloadBase64: base64.StdEncoding.EncodeToString(payload),
		NextOffset:    n,
	})
	_, _ = fmt.Fprintf(&event, "id: %d\nevent: output\ndata: %s\n\n", n, body)
	return io.NopCloser(bytes.NewReader(event.Bytes())), nil
}

// offersChoice says whether the task still offers the picked choice: it has to
// be one of the task's own, written against the version the task now stands at,
// and to agree with the basis the caller saw.
func offersChoice(task session.LedgerTaskRecord, in session.LedgerRequestInput) bool {
	for _, c := range task.Choices {
		if c.ID != in.ChoiceID {
			continue
		}
		if c.BasisVersion != task.Version {
			return false
		}
		return in.ChoiceBasisVersion == 0 || in.ChoiceBasisVersion == c.BasisVersion
	}
	return false
}

// CreateLedgerRequest stores one request the way the sidecar does: the id is
// the idempotency key, the task has to be one this ledger holds, and the stored
// row never touches the task it names.
func (c *StubClient) CreateLedgerRequest(_ context.Context, _ string, in session.LedgerRequestInput) (session.LedgerRequestStored, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, stored := range c.LedgerRequests {
		if stored.ID != in.ID {
			continue
		}
		if stored.TaskID != in.TaskID || stored.Text != in.Text {
			return session.LedgerRequestStored{}, fmt.Errorf("%w: request id already used with different content", session.ErrConflict)
		}
		return session.LedgerRequestStored{Request: stored, Replayed: true}, nil
	}
	found := false
	var task session.LedgerTaskRecord
	for _, row := range c.LedgerRows {
		if row.ID == in.TaskID {
			found, task = true, row
			break
		}
	}
	if !found {
		return session.LedgerRequestStored{}, fmt.Errorf("%w: task not found", session.ErrNotFound)
	}
	// The sidecar checks a pick on the way in, not only on the way out: a
	// choice it no longer offers is refused, so a screen left open while the
	// task moved cannot act on an old sentence.
	if in.ChoiceID != "" && !offersChoice(task, in) {
		return session.LedgerRequestStored{}, fmt.Errorf("%w: that choice is no longer offered for this task",
			session.ErrLedgerChoiceStale)
	}
	stored := session.LedgerRequest{
		ID: in.ID, TaskID: in.TaskID, Seq: int64(len(c.LedgerRequests) + 1), Version: 1,
		Text: in.Text, Author: in.Author, Status: session.LedgerRequestPending,
		CreatedAt: "2026-11-18T00:00:00Z",
	}
	c.LedgerRequests = append(c.LedgerRequests, stored)
	return session.LedgerRequestStored{Request: stored, Replayed: false}, nil
}

// LedgerEvents pages LedgerEventRows the way the sidecar pages the history: by
// the row's own seq, and only the task that was asked for.
func (c *StubClient) LedgerEvents(_ context.Context, _, taskID string, afterSeq int64, limit int) (session.LedgerTaskEventPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit <= 0 {
		limit = session.LedgerEventPageLimitDefault
	}
	page := session.LedgerTaskEventPage{Events: []session.LedgerTaskEvent{}}
	for _, row := range c.LedgerEventRows[taskID] {
		if row.Seq <= afterSeq {
			continue
		}
		if len(page.Events) == limit {
			page.NextCursor = page.Events[len(page.Events)-1].Seq
			break
		}
		page.Events = append(page.Events, row)
	}
	return page, nil
}

// WorkTasks pages over WorkRows the way the sidecar pages the work records
// (AC-N9): ids in order, after_id as the cursor, and an empty next_cursor only
// on the last page. status and session_id filter the same way, with the literal
// "none" selecting the unassigned tasks.
func (c *StubClient) WorkTasks(
	_ context.Context, _ string, limit int, afterID, status, sessionID string,
) (session.WorkTaskPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorkReads++
	if limit <= 0 {
		limit = session.WorkPageLimitDefault
	}
	page := session.WorkTaskPage{Tasks: []session.WorkTaskRecord{}}
	for _, record := range c.WorkRows {
		if record.Task.ID <= afterID {
			continue
		}
		if status != "" && record.Task.Status != status {
			continue
		}
		if sessionID != "" {
			if sessionID == "none" {
				if record.Task.SessionID != nil {
					continue
				}
			} else if record.Task.SessionID == nil || *record.Task.SessionID != sessionID {
				continue
			}
		}
		if len(page.Tasks) == limit {
			page.NextCursor = page.Tasks[len(page.Tasks)-1].Task.ID
			break
		}
		page.Tasks = append(page.Tasks, record)
	}
	return page, nil
}

// WorkTurns pages WorkTurnRows the way the sidecar pages a task's turns: by
// turn id, oldest first. A task the stub does not hold is not found rather than
// an empty page — an empty page reads as "no turns yet" and would hide a wrong
// id.
func (c *StubClient) WorkTurns(
	_ context.Context, _, taskID string, afterTurnID int64, limit int, openOnly bool,
) (session.WorkTurnPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorkReads++
	if limit <= 0 {
		limit = session.WorkPageLimitDefault
	}
	version := int64(0)
	found := false
	for _, record := range c.WorkRows {
		if record.Task.ID == taskID {
			version, found = record.Task.Version, true
			break
		}
	}
	if !found {
		return session.WorkTurnPage{Turns: []session.WorkTurn{}},
			fmt.Errorf("%w: task not found", session.ErrNotFound)
	}
	page := session.WorkTurnPage{TaskID: taskID, TaskVersion: version, Turns: []session.WorkTurn{}}
	for _, turn := range c.WorkTurnRows[taskID] {
		if turn.ID <= afterTurnID {
			continue
		}
		if openOnly && (turn.Request == nil || turn.EndedAt != nil) {
			continue
		}
		if len(page.Turns) == limit {
			page.NextCursor = page.Turns[len(page.Turns)-1].ID
			break
		}
		page.Turns = append(page.Turns, turn)
	}
	return page, nil
}
