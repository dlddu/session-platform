// The control plane's hops onto the agreed work records (AC-N9, AC-N10),
// relayed by the session pod's agent from the sidecar that mounts the slot's
// database: two reads and the one write.
//
// The reads are GETs carrying only a page size, a cursor and a task id: there
// is no SQL on this path, and reading changes nothing in the session. A pod
// that predates these routes answers 404 for an unknown path, which is reported
// as unsupported rather than as empty work — an empty page would claim the slot
// has no work when the truth is that this image cannot be asked.
//
// The write is a POST of a person's words, which the slot turns into the next
// turn of a task. It is as passive as the reads: it starts no turn of the
// session's own and wakes nothing.
//
// Keep the paths in sync with data-plane/cmd/agent (agentWorkTasksPath,
// agentWorkTurnsPath, agentWorkRequestsPath).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// WorkTasks reads one keyset page of the slot's work tasks.
func (c *HTTPClient) WorkTasks(
	ctx context.Context, pod string, limit int, afterID, status, sessionID string,
) (session.WorkTaskPage, error) {
	page := session.WorkTaskPage{Tasks: []session.WorkTaskRecord{}}
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	for name, value := range map[string]string{
		"after_id": afterID, "status": status, "session_id": sessionID,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	if err := c.readWork(ctx, pod, "/work/tasks", query, "work tasks", &page); err != nil {
		return session.WorkTaskPage{Tasks: []session.WorkTaskRecord{}}, err
	}
	if page.Tasks == nil {
		page.Tasks = []session.WorkTaskRecord{}
	}
	return page, nil
}

// WorkTurns reads one keyset page of one task's turns, from whichever end
// cursor names. openOnly narrows it to the turns that still need the parent.
func (c *HTTPClient) WorkTurns(
	ctx context.Context, pod, taskID string, cursor session.WorkTurnCursor, limit int, openOnly bool,
) (session.WorkTurnPage, error) {
	page := session.WorkTurnPage{Turns: []session.WorkTurn{}}
	query := url.Values{}
	query.Set("task_id", taskID)
	switch {
	case cursor.Newest:
		// Set even when it is zero: on this route the parameter's presence is
		// what asks for the newest-first page, and zero is "from the newest
		// turn".
		query.Set("before_turn_id", strconv.FormatInt(cursor.Before, 10))
	case cursor.After > 0:
		query.Set("after_turn_id", strconv.FormatInt(cursor.After, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if openOnly {
		query.Set("open", "true")
	}
	if err := c.readWork(ctx, pod, "/work/turns", query, "work turns", &page); err != nil {
		return session.WorkTurnPage{Turns: []session.WorkTurn{}}, err
	}
	if page.Turns == nil {
		page.Turns = []session.WorkTurn{}
	}
	return page, nil
}

// CreateWorkRequest stores one person's request as the next turn of a task.
//
// The slot answers 201 for a first store and 200 for a replay of the same id
// with the same words, and the distinction is carried back rather than
// flattened: a caller retrying an answer it never saw has to be able to tell
// "stored now" from "already stored" without comparing the turn itself.
func (c *HTTPClient) CreateWorkRequest(
	ctx context.Context, pod string, in session.WorkRequestInput,
) (session.WorkRequestStored, error) {
	var empty session.WorkRequestStored
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return empty, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return empty, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL(ip)+"/work/requests", bytes.NewReader(body))
	if err != nil {
		return empty, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return empty, fmt.Errorf("store a work request in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return empty, fmt.Errorf("store a work request in session pod %s: %w", pod, err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return empty, workWriteError(pod, resp.StatusCode, raw)
	}
	var stored session.WorkRequestStored
	if err := json.Unmarshal(raw, &stored); err != nil {
		return empty, fmt.Errorf("store a work request in session pod %s: decode: %w", pod, err)
	}
	// The slot's status is the authority on which of the two happened; the
	// body's own flag is what it agrees with. Trusting only the body would make
	// a future field rename silently report every store as a first one.
	stored.Replayed = resp.StatusCode == http.StatusOK
	return stored, nil
}

// workWriteError classifies a refused write by the slot's own error code rather
// than by status alone. The codes do not map onto statuses one to one — a
// conflict is a request id reused with different words, a fence held by the
// move, or a pick from a turn the task has moved past, and a caller's next
// action differs for each. Guessing from 409 would tell somebody to retry when
// what they need is to re-read the task.
func workWriteError(pod string, status int, raw []byte) error {
	var decoded struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &decoded)
	message := strings.TrimSpace(decoded.Error)
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	switch decoded.Code {
	case "stale_choice":
		return fmt.Errorf("%w: %s", session.ErrWorkChoiceStale, message)
	case "not_found":
		return fmt.Errorf("%w: %s", session.ErrNotFound, message)
	case "invalid_argument":
		return fmt.Errorf("%w: %s", session.ErrInvalidInput, message)
	case "conflict":
		return fmt.Errorf("%w: %s", session.ErrConflict, message)
	case "work_unavailable":
		return fmt.Errorf("%w: %s", session.ErrWorkUnavailable, message)
	}
	// No code this image knows: fall back to the status, which the agent sets
	// even when the sidecar could not be reached at all.
	switch status {
	case http.StatusNotImplemented:
		return fmt.Errorf("%w: %s", session.ErrWorkUnsupported, message)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", session.ErrNotFound, message)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", session.ErrInvalidInput, message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", session.ErrConflict, message)
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %s", session.ErrWorkUnavailable, message)
	default:
		return fmt.Errorf("store a work request in session pod %s: agent returned %d: %s", pod, status, message)
	}
}

// readWork is the one hop both reads make, so both classify an answer the same
// way. 409 is the migration holding the database — a retry, not a failure —
// and 404 from the route itself cannot be told apart from 404 for an unknown
// task, so the agent's body is carried into the message and the caller is told
// the task was not found rather than that the slot keeps no work.
func (c *HTTPClient) readWork(
	ctx context.Context, pod, path string, query url.Values, what string, out any,
) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	target := c.baseURL(ip) + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("read %s in session pod %s: %w", what, pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusNotImplemented:
			return fmt.Errorf("%w: %s", session.ErrWorkUnsupported, message)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", session.ErrNotFound, message)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", session.ErrWorkUnavailable, message)
		case http.StatusBadRequest:
			return fmt.Errorf("%w: %s", session.ErrInvalidInput, message)
		default:
			return fmt.Errorf("read %s in session pod %s: agent returned %d: %s",
				what, pod, resp.StatusCode, message)
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("read %s in session pod %s: decode: %w", what, pod, err)
	}
	return nil
}
