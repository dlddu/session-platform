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
	// workWriters is each pod's seal: who may write that slot's work records
	// (work_writer.go). A pod that is not here has never been handed over.
	workWriters map[string]session.WorkWriter
	// writerToken is the credential the stub's seal routes require, mirroring
	// the sidecar's MONITOR_PAIR_WRITER_TOKEN.
	writerToken string
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

// The seal that says who writes a slot's work records, as the stub holds it.
//
// It is per pod, because that is what it is in a real slot: the row sits in the
// database the pod mounts. The rule the stub enforces is the one the sidecar
// enforces — a monitor writes only a sealed slot, a session writes only an
// unsealed one — so a test that binds a pair and then writes exercises the
// ordering rather than a canned answer.

// WorkWriter answers this pod's seal; a pod that was never sealed is the
// session's own, which is every session with no monitor.
func (c *StubClient) WorkWriter(_ context.Context, pod, token string) (session.WorkWriter, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkWriterToken(token); err != nil {
		return session.WorkWriter{}, err
	}
	return c.writerOf(pod), nil
}

// WriterToken is what the stub's sidecar requires, so a test can show that a
// caller without it is refused the way a real sidecar refuses it.
func (c *StubClient) RequireWriterToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writerToken = token
}

func (c *StubClient) checkWriterToken(token string) error {
	if c.writerToken == "" || c.writerToken == token {
		return nil
	}
	return fmt.Errorf("%w: only the control plane may move this slot's writer", session.ErrForbidden)
}

func (c *StubClient) writerOf(pod string) session.WorkWriter {
	if c.workWriters == nil {
		c.workWriters = map[string]session.WorkWriter{}
	}
	got, ok := c.workWriters[pod]
	if !ok {
		return session.WorkWriter{Writer: session.WorkWriterSession}
	}
	return got
}

// SealWorkWriter moves the seal, with the same epoch rules the slot applies:
// the same seal again changes nothing, and a stale epoch is refused.
func (c *StubClient) SealWorkWriter(
	_ context.Context, pod, token string, in session.WorkSeal,
) (session.WorkSealResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkWriterToken(token); err != nil {
		return session.WorkSealResult{}, err
	}
	current := c.writerOf(pod)
	if in.ExpectedEpoch != nil && *in.ExpectedEpoch != current.Epoch {
		return session.WorkSealResult{}, fmt.Errorf("%w: expected_epoch does not match", session.ErrConflict)
	}
	holder := in.Holder
	if in.Writer == session.WorkWriterSession {
		holder = ""
	}
	if current.Writer == in.Writer && current.Holder == holder {
		return session.WorkSealResult{Writer: current, Changed: false}, nil
	}
	if in.Writer == session.WorkWriterSession && current.Sealed() &&
		in.Holder != "" && in.Holder != current.Holder {
		return session.WorkSealResult{}, fmt.Errorf("%w: held by %s", session.ErrConflict, current.Holder)
	}
	sealed := session.WorkWriter{Writer: in.Writer, Holder: holder, Epoch: current.Epoch + 1,
		ChangedAt: "2026-11-24T00:00:00Z"}
	c.workWriters[pod] = sealed
	return session.WorkSealResult{Writer: sealed, Changed: true}, nil
}

// CallWork carries one work tool call for the bound monitor. The stub answers
// the seal check for every tool and appends a turn for turn_append, which is
// what a test needs to show that the monitor's writes land in the same records
// the session read before the handover.
func (c *StubClient) CallWork(
	_ context.Context, pod, token, name string, args json.RawMessage,
) (session.WorkCallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.checkWriterToken(token); err != nil {
		return session.WorkCallResult{}, err
	}
	refuse := func(message string) session.WorkCallResult {
		raw, _ := json.Marshal(map[string]string{"code": "permission_denied", "error": message})
		return workCallResultOf(string(raw), true)
	}
	if !c.writerOf(pod).Sealed() {
		return refuse("these work records are the session's own"), nil
	}
	var call struct {
		TaskID string `json:"task_id"`
		Turn   struct {
			Request *string `json:"request"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(args, &call)
	if name != "turn_append" {
		raw, _ := json.Marshal(map[string]any{"called": name})
		return workCallResultOf(string(raw), false), nil
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
	turn := session.WorkTurn{ID: next, TaskID: call.TaskID, Request: call.Turn.Request,
		RequestedAt: "2026-11-24T00:00:00Z"}
	c.WorkTurnRows[call.TaskID] = append(c.WorkTurnRows[call.TaskID], turn)
	raw, _ := json.Marshal(map[string]any{"turn": turn})
	return workCallResultOf(string(raw), false), nil
}

func workCallResultOf(text string, isError bool) session.WorkCallResult {
	result := session.WorkCallResult{IsError: isError}
	result.Content = append(result.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: text})
	return result
}

// Written is what a session's pod has been told, so a test can show that
// something which claimed to wake a monitor actually queued it a turn.
func (c *StubClient) Written(pod string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.bufs[pod])
}
