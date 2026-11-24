// The parent's monitoring loops (AC-N10).
//
// A monitoring row was never a person's work: it is how the parent records that
// its own loop ran, what it last saw and whether it is still running. It lived
// in the ledger because the ledger was the only record there was, and it was
// deliberately *not* migrated into a task — presenting one as a task would
// invent work nobody asked for, and the app's panel has always had to filter
// them out by a convention in a details JSON (`details.kind == "monitor"`).
//
// So it gets a record of its own, with the shape it actually has: one row per
// loop, holding what it is, where it stands, and the note the parent keeps on
// it. Nothing reads it but the parent, and it is not on the read path a person
// sees — which is why the app no longer needs to know the convention at all.
//
// Lifecycle, which is the part the ledger never had: a put replaces the row
// under its own counter, and a loop that is finished is deleted rather than
// left behind as a `completed` row nothing will read again. Retirement applies
// the same rule to what it adopts (session_work_retire.go): a finished loop is
// not carried.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	monitorGetTool    = "monitor_get"
	monitorListTool   = "monitor_list"
	monitorPutTool    = "monitor_put"
	monitorDeleteTool = "monitor_delete"

	// monitorStateRunning is a loop that is still going, and monitorStateStopped
	// one that has stopped and is expected to start again. There is deliberately
	// no "completed": a loop that is done is deleted, because a record of a loop
	// that will never run again is the stale copy this area exists to stop
	// accumulating.
	monitorStateRunning = "running"
	monitorStateStopped = "stopped"

	// monitorNoteMax bounds one loop's note for the reason a task's text is
	// bounded: the slot's storage budget is shared with the session's archive,
	// so a note is a note rather than a log.
	monitorNoteMax = 4096
)

var monitorStates = []string{monitorStateRunning, monitorStateStopped}

func isMonitorState(s string) bool {
	for _, candidate := range monitorStates {
		if candidate == s {
			return true
		}
	}
	return false
}

// workMonitor is one monitoring loop as the parent keeps it.
type workMonitor struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
	Note  string `json:"note"`
	// Version is this row's compare-and-swap value: a put names the value it
	// read, and the swap is atomic, so of two parents that both read this row
	// exactly one can replace it.
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

// workMonitorInput is one loop as a writer gives it. It is a full replacement,
// like the task record it sits beside: a partial update of a note is how two
// parents lose each other's writing.
type workMonitorInput struct {
	Title string `json:"title"`
	State string `json:"state"`
	Note  string `json:"note"`
}

func monitorToolDefinitions() []map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	record := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{"type": "string", "description": "What this loop watches."},
			"state": map[string]any{"type": "string", "description": "running or stopped. A loop that is finished for good is deleted instead."},
			"note":  map[string]any{"type": "string", "description": "What the loop last saw, as the parent keeps it (at most 4096 characters)."},
		},
		"required": []string{"title", "state", "note"}, "additionalProperties": false,
	}
	return []map[string]any{
		{
			"name":        monitorGetTool,
			"description": "One monitoring loop of this parent's own: title, state, the note it keeps, and the version a write must hold. Reading changes nothing. A loop that has never been written answers not_found — this record is the parent's, so an empty row is not invented for it.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": str()},
				"required": []string{"id"}, "additionalProperties": false},
		},
		{
			"name":        monitorListTool,
			"description": "This parent's monitoring loops, id-keyset ordered, with after_id and limit (1..100, default 20). These are not tasks and never appear in task_list: a monitoring row is the parent's own bookkeeping, and showing it as work would invent work nobody asked for.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"after_id": str(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": workListLimitMax, "default": workListLimitDefault},
			}, "required": []string{}, "additionalProperties": false},
		},
		{
			"name":        monitorPutTool,
			"description": "Creates (expected_version 0) or fully replaces (expected_version = the row's current version) one monitoring loop. Full replacement: every field is required. The version increments on success and a stale version is a conflict, so of two parents that both read the row exactly one wins. request_id replays the original committed answer.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 0},
				"monitor":          record,
			}, "required": []string{"id", "request_id", "expected_version", "monitor"}, "additionalProperties": false},
		},
		{
			"name":        monitorDeleteTool,
			"description": "Removes one monitoring loop. This is how a loop that is finished for good leaves — a row kept as \"completed\" for ever is a record nothing will read again. Unlike a task id, a monitor id is not retired: the same loop can be recorded again later under the same name.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
			}, "required": []string{"id", "request_id", "expected_version"}, "additionalProperties": false},
		},
	}
}

func validateMonitorTool(name string, a workArgs) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if name == monitorListTool {
		if a.Limit < 0 || a.Limit > workListLimitMax {
			return bad("invalid pagination")
		}
		if a.AfterID != "" && !workID.MatchString(a.AfterID) {
			return bad("invalid after_id")
		}
		return nil
	}
	if !workID.MatchString(a.ID) {
		return bad("id must be 1..128 letters, digits, _ or -")
	}
	if name == monitorGetTool {
		return nil
	}
	if a.ExpectedVersion == nil || *a.ExpectedVersion < 0 || *a.ExpectedVersion > requestSafeInt {
		return bad("a safe nonnegative expected_version is required")
	}
	if name == monitorDeleteTool {
		if *a.ExpectedVersion == 0 {
			return bad("expected_version must be the row's current version")
		}
		return nil
	}
	if a.Monitor == nil {
		return bad("monitor required")
	}
	if strings.TrimSpace(a.Monitor.Title) == "" || len(a.Monitor.Title) > workTitleMax {
		return bad("title must be 1..512 characters")
	}
	if !isMonitorState(a.Monitor.State) {
		return bad("state must be " + strings.Join(monitorStates, " or ") +
			" — a loop that is finished for good is removed with " + monitorDeleteTool)
	}
	if len(a.Monitor.Note) > monitorNoteMax {
		return bad("note must be at most 4096 characters — it is a note, not a log")
	}
	return nil
}

func runMonitorTool(ctx context.Context, tx *sql.Tx, name string, a workArgs, now time.Time) (any, error) {
	switch name {
	case monitorGetTool:
		return readMonitor(ctx, tx, a.ID)
	case monitorListTool:
		return listMonitors(ctx, tx, a)
	case monitorPutTool:
		return putMonitor(ctx, tx, a, now)
	case monitorDeleteTool:
		return deleteMonitor(ctx, tx, a)
	}
	return nil, workFail("invalid_argument", "unknown monitor tool")
}

func readMonitor(ctx context.Context, tx *sql.Tx, id string) (workMonitor, error) {
	var row workMonitor
	err := tx.QueryRowContext(ctx,
		"SELECT id,title,state,note,version,updated_at FROM sp_work_monitor WHERE id=?", id).
		Scan(&row.ID, &row.Title, &row.State, &row.Note, &row.Version, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return workMonitor{}, workFail("not_found", "this slot has no monitoring row with that id")
	}
	return row, err
}

func listMonitors(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = workListLimitDefault
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id,title,state,note,version,updated_at FROM sp_work_monitor
		 WHERE id>? ORDER BY id LIMIT ?`, a.AfterID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workMonitor{}
	for rows.Next() {
		var row workMonitor
		if err := rows.Scan(&row.ID, &row.Title, &row.State, &row.Note, &row.Version, &row.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cursor := ""
	if len(out) > limit {
		out = out[:limit]
		cursor = out[len(out)-1].ID
	}
	return map[string]any{"monitors": out, "next_cursor": cursor}, nil
}

func putMonitor(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	var current int64
	err := tx.QueryRowContext(ctx, "SELECT version FROM sp_work_monitor WHERE id=?", a.ID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	stamp := workNow(now)
	if errors.Is(err, sql.ErrNoRows) {
		if *a.ExpectedVersion != 0 {
			return nil, workFail("conflict", "no monitoring row with that id; expected_version 0 creates one")
		}
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO sp_work_monitor (id,title,state,note,version,updated_at) VALUES (?,?,?,?,1,?)",
			a.ID, a.Monitor.Title, a.Monitor.State, a.Monitor.Note, stamp); err != nil {
			return nil, err
		}
		return readMonitor(ctx, tx, a.ID)
	}
	if *a.ExpectedVersion == 0 {
		return nil, workFail("conflict", "that monitoring row exists; replace it with its current version")
	}
	// One conditional UPDATE, so of two parents holding the same version exactly
	// one lands: comparing and then writing would let both through.
	result, err := tx.ExecContext(ctx,
		`UPDATE sp_work_monitor SET title=?,state=?,note=?,version=version+1,updated_at=?
		 WHERE id=? AND version=?`,
		a.Monitor.Title, a.Monitor.State, a.Monitor.Note, stamp, a.ID, *a.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, workFail("conflict", "that monitoring row moved; read it again")
	}
	return readMonitor(ctx, tx, a.ID)
}

func deleteMonitor(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	result, err := tx.ExecContext(ctx,
		"DELETE FROM sp_work_monitor WHERE id=? AND version=?", a.ID, *a.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		var present int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_work_monitor WHERE id=?", a.ID).Scan(&present); err != nil {
			return nil, err
		}
		if present == 0 {
			return nil, workFail("not_found", "this slot has no monitoring row with that id")
		}
		return nil, workFail("conflict", "that monitoring row moved; read it again")
	}
	return map[string]any{"id": a.ID, "deleted": true}, nil
}

// adoptMonitorRow writes one monitoring loop retirement carried out of the
// ledger. It never overwrites a row the parent has already written here: the
// parent's own write is the current one, and the ledger copy it came from has
// been standing still since.
// monitorAdoptionRefusals is why a loop cannot be carried as it stands, or
// nothing when it can.
//
// The adoption writes sp_work_monitor directly, so it does not pass through
// validateMonitorTool, and the table has no CHECK to fall back on. A row the
// tool would refuse is one monitor_get reads and monitor_put can never
// replace — monitor_put is a *full* replacement — so the parent could never
// record its own loop again: readable for ever, writable never.
//
// The answer is to refuse, and it is worth saying why rather than making the
// row fit. An earlier attempt cut the note to the bound. That was wrong. The
// note is not scratch a parent rewrites on its next pass: it is where the
// parent keeps the approval it is acting under, the claim it holds and the
// cursor it resumes from, and it is JSON. Cutting it destroys state nothing
// else holds and leaves the remainder unparseable — strictly worse than not
// moving the row at all. Refusing keeps the ledger row byte for byte as it is
// and leaves the decision with a person, which is the only honest option when
// this code cannot tell what the bytes mean.
//
// Each refusal says what to do, because a refusal nobody can act on is a dead
// end. The whole run stops on any of them (verifyRetirement) and the
// transaction rolls back, so nothing is half-moved.
func monitorAdoptionRefusals(row workMonitor) []string {
	refusals := []string{}
	where := "sp_ledger_tasks monitoring row " + row.ID
	if strings.TrimSpace(row.Title) == "" {
		refusals = append(refusals, where+" has no title, and "+monitorPutTool+
			" requires one, so the carried row could never be written again. Give the loop a title in the"+
			" ledger, or remove the row deliberately, and run again — nothing was changed.")
	}
	if len(row.Title) > workTitleMax {
		refusals = append(refusals, fmt.Sprintf(
			"%s has a %d-byte title, over the %d this record holds. Shorten it in the ledger and run again —"+
				" it is not cut here, because this code cannot tell which part matters.",
			where, len(row.Title), workTitleMax))
	}
	if len(row.Note) > monitorNoteMax {
		refusals = append(refusals, fmt.Sprintf(
			"%s keeps %d bytes of state, over the %d a note holds. It is not cut: that state is where the"+
				" approval in force, the claim held and the cursor to resume from live, and cutting it would"+
				" destroy what nothing else has and leave the rest unparseable. Reduce what the loop keeps in"+
				" the ledger — or record the loop with %s yourself and delete the ledger row — then run again."+
				" Nothing was changed.",
			where, len(row.Note), monitorNoteMax, monitorPutTool))
	}
	if !isMonitorState(row.State) {
		refusals = append(refusals, fmt.Sprintf(
			"%s would be recorded as %q, which is not a state this record has (%s). A loop that is finished for"+
				" good is removed with %s rather than recorded as finished, so which of the two this is needs a"+
				" person. Nothing was changed.",
			where, row.State, strings.Join(monitorStates, " or "), monitorDeleteTool))
	}
	return refusals
}

func adoptMonitorRow(ctx context.Context, tx *sql.Tx, row workMonitor) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_monitor (id,title,state,note,version,updated_at)
		 VALUES (?,?,?,?,1,?) ON CONFLICT(id) DO NOTHING`,
		row.ID, row.Title, row.State, row.Note, row.UpdatedAt)
	return err
}
