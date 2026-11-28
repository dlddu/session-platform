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

	// WorkRows is the slot's agreed work records as the stub holds them, in id
	// order (AC-N9), and WorkTurnRows is each task's turns in turn-id order.
	// WorkTasks/WorkTurns page over them with the sidecar's own keyset rules,
	// so a test reads pages rather than one canned answer.
	WorkRows     []session.WorkTaskRecord
	WorkTurnRows map[string][]session.WorkTurn
	// WorkReads counts work pages served, so a test can show that reading the
	// records never turned into something else.
	WorkReads int
	// WorkRequests is what callers stored, in arrival order, with the same
	// id-idempotency and task-link rules the sidecar applies. Each one became a
	// turn on its task, which is where a test looks for it.
	WorkRequests []session.WorkRequestInput

	mu      sync.Mutex
	bufs    map[string][]byte
	steered map[string][]string
	// The lane half (AC-N11, stub_lane.go): one buffer and one write log per
	// (pod, lane), and what this stub's pod says about itself.
	laneBufs        map[laneKey][]byte
	laneWrites      map[laneKey][]string
	lanePaired      bool
	laneUnreachable bool
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
	buf := append([]byte(nil), c.bufs[pod]...)
	c.mu.Unlock()
	return c.streamBytes(buf, offset)
}

// streamBytes is the stub's SSE shape over one buffer, shared by the pod's own
// stream and a lane's (stub_lane.go) so both answer the same event contract.
func (c *StubClient) streamBytes(buf []byte, offset int64) (io.ReadCloser, error) {
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

// latestTurnID is the task's newest turn, which is the only turn whose offered
// choices are current. It is how the stub reproduces the slot's own staleness
// rule: a pick from an earlier turn is a screen the task has moved past.
func (c *StubClient) latestTurnID(taskID string) (int64, bool) {
	turns := c.WorkTurnRows[taskID]
	if len(turns) == 0 {
		return 0, false
	}
	return turns[len(turns)-1].ID, true
}

// CreateWorkRequest stores one request the way the sidecar does: the id is the
// idempotency key, the task has to be one this slot holds, a pick has to name
// the task's latest turn, and what is stored is a *turn* on that task rather
// than a row beside it.
func (c *StubClient) CreateWorkRequest(
	_ context.Context, _ string, in session.WorkRequestInput,
) (session.WorkRequestStored, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, stored := range c.WorkRequests {
		if stored.ID != in.ID {
			continue
		}
		if stored.TaskID != in.TaskID || stored.Text != in.Text {
			return session.WorkRequestStored{}, fmt.Errorf("%w: request id already used with different content",
				session.ErrConflict)
		}
		// A replay answers with the turn it already became, found by the origin
		// the slot recorded against it.
		for _, turn := range c.WorkTurnRows[stored.TaskID] {
			if turn.Request != nil && *turn.Request == stored.Text {
				return session.WorkRequestStored{Turn: turn, Replayed: true}, nil
			}
		}
		return session.WorkRequestStored{Replayed: true}, nil
	}
	found := false
	for _, row := range c.WorkRows {
		if row.Task.ID == in.TaskID {
			found = true
			break
		}
	}
	if !found {
		return session.WorkRequestStored{}, fmt.Errorf("%w: task not found", session.ErrNotFound)
	}
	// The slot checks a pick on the way in, not only on the way out: a turn the
	// task has moved past is refused, so a screen left open cannot act on a
	// question that is no longer being asked.
	if in.ChoiceTurnID != nil {
		latest, ok := c.latestTurnID(in.TaskID)
		if !ok || latest != *in.ChoiceTurnID {
			return session.WorkRequestStored{}, fmt.Errorf("%w: that choice is no longer offered for this task",
				session.ErrWorkChoiceStale)
		}
	}
	if c.WorkTurnRows == nil {
		c.WorkTurnRows = map[string][]session.WorkTurn{}
	}
	next := int64(1)
	for _, turns := range c.WorkTurnRows {
		for _, turn := range turns {
			if turn.ID >= next {
				next = turn.ID + 1
			}
		}
	}
	text := in.Text
	turn := session.WorkTurn{
		ID: next, TaskID: in.TaskID, Request: &text,
		Choices: []session.WorkChoice{}, Artifacts: []string{},
		RequestedAt: "2026-11-20T00:00:00Z",
		// app_request is what the slot records for a turn a person wrote
		// through this route, which is how a reader tells it from one the
		// session wrote itself.
		Origin: "app_request",
	}
	c.WorkTurnRows[in.TaskID] = append(c.WorkTurnRows[in.TaskID], turn)
	c.WorkRequests = append(c.WorkRequests, in)
	for i := range c.WorkRows {
		if c.WorkRows[i].Task.ID != in.TaskID {
			continue
		}
		c.WorkRows[i].TurnsTotal++
		stored := turn
		c.WorkRows[i].LatestTurn = &stored
	}
	return session.WorkRequestStored{Turn: turn, Replayed: false}, nil
}

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
// turn id, from whichever end the cursor names. A task the stub does not hold
// is not found rather than an empty page — an empty page reads as "no turns
// yet" and would hide a wrong id.
func (c *StubClient) WorkTurns(
	_ context.Context, _, taskID string, cursor session.WorkTurnCursor, limit int, openOnly bool,
) (session.WorkTurnPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WorkReads++
	if limit <= 0 {
		limit = session.WorkPageLimitDefault
	}
	var task session.WorkTask
	found := false
	for _, record := range c.WorkRows {
		if record.Task.ID == taskID {
			task, found = record.Task, true
			break
		}
	}
	if !found {
		return session.WorkTurnPage{Turns: []session.WorkTurn{}},
			fmt.Errorf("%w: task not found", session.ErrNotFound)
	}
	page := session.WorkTurnPage{
		TaskID: taskID, TaskVersion: task.Version, Task: task, Turns: []session.WorkTurn{},
	}
	// The rows are held oldest first; a newest-first page walks them the other
	// way, so both directions come off the same list in the order the slot
	// would have returned them.
	rows := c.WorkTurnRows[taskID]
	if cursor.Newest {
		page.Order = "desc"
		rows = make([]session.WorkTurn, 0, len(c.WorkTurnRows[taskID]))
		for i := len(c.WorkTurnRows[taskID]) - 1; i >= 0; i-- {
			rows = append(rows, c.WorkTurnRows[taskID][i])
		}
	}
	for _, turn := range rows {
		if cursor.Newest {
			if cursor.Before > 0 && turn.ID >= cursor.Before {
				continue
			}
		} else if turn.ID <= cursor.After {
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
