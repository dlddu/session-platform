// The control plane's reads of the agreed work records (AC-N9), relayed by the
// session pod's agent from the sidecar that mounts the slot's database.
//
// Both reads are GETs carrying only a page size, a cursor and a task id: there
// is no SQL on this path, and reading changes nothing in the session. A pod
// that predates these routes answers 404 for an unknown path, which is reported
// as unsupported rather than as empty work — an empty page would claim the slot
// has no work when the truth is that this image cannot be asked.
package agent

import (
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

// WorkTurns reads one keyset page of one task's turns, oldest first. openOnly
// narrows it to the turns that still need the parent.
func (c *HTTPClient) WorkTurns(
	ctx context.Context, pod, taskID string, afterTurnID int64, limit int, openOnly bool,
) (session.WorkTurnPage, error) {
	page := session.WorkTurnPage{Turns: []session.WorkTurn{}}
	query := url.Values{}
	query.Set("task_id", taskID)
	if afterTurnID > 0 {
		query.Set("after_turn_id", strconv.FormatInt(afterTurnID, 10))
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
			return fmt.Errorf("%w: %s", session.ErrLedgerUnsupported, message)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", session.ErrNotFound, message)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", session.ErrLedgerUnavailable, message)
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
