// The parent's own bookkeeping after the cutover (AC-N9).
//
// The agreed task and turn carry what a person said, was told, answered and was
// explained. They deliberately do not carry the parent's bookkeeping — approval
// scope, the session relations with their cursors and seqs, preservation state,
// cancellation notes, monitor rows — and a migration preserved all of that
// verbatim in sp_work_task_legacy beside the task.
//
// Preserved and unreachable would be the same as lost. A parent that has moved
// to the work tools still has to read an approval scope, advance a session
// cursor after collecting a worker's result, and record that a monitor loop ran.
// Before this file the legacy write fence refused every ledger write once a slot
// had migrated, which left the parent no way to do any of it — the records were
// there and nothing could touch them.
//
// Two paths, split by who owns the row:
//
//   - A task the work records own (there is an sp_work_tasks row) keeps its
//     bookkeeping here: work_legacy_get reads it, work_legacy_put replaces it,
//     under the work task's version. A ledger write to such a task stays
//     refused, because that is the divergence the fence exists for.
//   - A ledger row the work records do not own — a monitor row, which is the
//     parent's own bookkeeping and was never a person's work — stays writable
//     through the ledger tools it has always used. Nothing in the work records
//     reads it, so there is nothing for it to diverge from.
//
// Creating a *new* ledger task after the cutover stays refused either way: the
// app reads the work records, so a task written to the ledger now would be work
// nobody can see.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// workLegacyInput is the preserved bookkeeping of one migrated task as a writer
// gives it. Every field is replaced, as a ledger put replaced the whole task:
// a partial update of a cursor set is how two parents lose each other's work.
type workLegacyInput struct {
	Stage    string          `json:"stage"`
	Owner    string          `json:"owner"`
	Sessions []ledgerSession `json:"sessions"`
	Details  map[string]any  `json:"details"`
}

// workLegacyRecord is what a reader gets back: the input plus the facts that
// say what it belongs to and whether it is still a task.
type workLegacyRecord struct {
	TaskID       string          `json:"task_id"`
	Kind         string          `json:"kind"`
	LegacyStatus string          `json:"legacy_status"`
	Stage        string          `json:"stage"`
	Owner        string          `json:"owner"`
	Sessions     []ledgerSession `json:"sessions"`
	Details      map[string]any  `json:"details"`
	// TaskVersion is the work task's version this bookkeeping hangs off. It is
	// shown so a parent can see the state its notes are about; it is *not* the
	// value a writer holds. Zero means no work task owns this row — a preserved
	// monitor row, which is written through the ledger instead.
	TaskVersion int64 `json:"task_version"`
	// BookVersion is the compare-and-swap value for this lane: the writer names
	// it and the swap is atomic, so two parents that both read this record
	// cannot both replace it (session_work_guard.go). It moves only when the
	// bookkeeping is written, so advancing a cursor never invalidates the
	// choices a turn is offering.
	BookVersion int64 `json:"book_version"`
}

const (
	workLegacyGetTool = "work_legacy_get"
	workLegacyPutTool = "work_legacy_put"
)

func workLegacyToolDefinitions() []map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	record := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"stage": str(), "owner": str(),
			"sessions": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"details":  map[string]any{"type": "object"},
		},
		"required": []string{"stage", "owner", "sessions", "details"}, "additionalProperties": false,
	}
	return []map[string]any{
		{
			"name":        workLegacyGetTool,
			"description": "The parent's own bookkeeping beside one task: stage, owner, the session relations with their cursors and seqs, and the details JSON holding approval scope, preservation and cancellation state. This is what a migration preserved verbatim; it is not part of the agreed task, and nothing a person reads shows it. task_version is the work task's current version and is what work_legacy_put must hold. Reading changes nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"task_id": str()},
				"required": []string{"task_id"}, "additionalProperties": false},
		},
		{
			"name":        workLegacyPutTool,
			"description": "Replaces that bookkeeping for one task the work records own — advance a session cursor after collecting a worker's result, record an approval scope, note preservation or a cancellation. Full replacement: every field is required. expected_book_version must be the book_version work_legacy_get returned, and the swap is atomic, so of two parents that both read this record exactly one can replace it and the other is told to read again. It is a counter of its own, so writing bookkeeping moves neither the task's version nor the turn lane's — advancing a cursor never invalidates the choices a turn is offering. request_id replays the original committed answer. A monitor row the work records do not own is refused here; ledger_put remains its write path.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "request_id": str(),
				"expected_book_version": map[string]any{"type": "integer", "minimum": 1},
				"record":                record,
			}, "required": []string{"task_id", "request_id", "expected_book_version", "record"}, "additionalProperties": false},
		},
	}
}

// readWorkLegacy answers one task's preserved bookkeeping. A task that exists
// but has no preserved row answers an empty record rather than not_found: a
// task created after the cutover simply has no bookkeeping yet, and saying "no
// such task" about one that is right there would send a parent looking for a
// bug.
func readWorkLegacy(ctx context.Context, tx *sql.Tx, taskID string) (any, error) {
	version, owned, err := workTaskVersionOf(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	record := workLegacyRecord{
		TaskID: taskID, Sessions: []ledgerSession{}, Details: map[string]any{}, TaskVersion: version,
	}
	if owned {
		_, book, guardErr := workGuards(ctx, tx, taskID)
		if guardErr != nil {
			return nil, guardErr
		}
		record.BookVersion = book
	}
	var stage, owner, sessions, details, kind, legacyStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT stage,owner,sessions,details,kind,legacy_status
		 FROM sp_work_task_legacy WHERE task_id=?`, taskID).
		Scan(&stage, &owner, &sessions, &details, &kind, &legacyStatus)
	if errors.Is(err, sql.ErrNoRows) {
		if !owned {
			return nil, ledgerFail("not_found", "this slot has no task or preserved row with that id")
		}
		return record, nil
	}
	if err != nil {
		return nil, err
	}
	record.Stage, record.Owner, record.Kind, record.LegacyStatus = stage, owner, kind, legacyStatus
	if sessions != "" {
		var relations []ledgerSession
		if err := json.Unmarshal([]byte(sessions), &relations); err == nil && relations != nil {
			record.Sessions = relations
		}
	}
	if details != "" {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(details), &decoded); err == nil && decoded != nil {
			record.Details = decoded
		}
	}
	return record, nil
}

// putWorkLegacy replaces one task's preserved bookkeeping under the work task's
// version.
func putWorkLegacy(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	version, owned, err := workTaskVersionOf(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	if !owned {
		// Either there is no such task, or it is a preserved row the work
		// records do not own. The second keeps the ledger tools it always had,
		// and saying so is more use than a bare not_found.
		var preserved int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_work_task_legacy WHERE task_id=?", a.TaskID).Scan(&preserved); err != nil {
			return nil, err
		}
		if preserved != 0 {
			return nil, ledgerFail("conflict",
				"no work task owns that id — it is a preserved row, which is written with ledger_put as before")
		}
		return nil, ledgerFail("not_found", "task not found")
	}
	if a.ExpectedBookVersion == nil {
		return nil, ledgerFail("invalid_argument", "expected_book_version is required")
	}
	if _, err = swapWorkGuard(ctx, tx, a.TaskID, workGuardBook, *a.ExpectedBookVersion); err != nil {
		return nil, err
	}
	sessions, err := json.Marshal(a.Legacy.Sessions)
	if err != nil {
		return nil, err
	}
	details, err := json.Marshal(a.Legacy.Details)
	if err != nil {
		return nil, err
	}
	// The preserved row may not exist yet: a task created after the cutover has
	// no bookkeeping until a parent writes some. kind and legacy_status are the
	// migration's own facts, so an insert leaves them empty rather than
	// inventing a legacy status for a task that never had one.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_task_legacy
		   (task_id,legacy_version,legacy_status,kind,stage,owner,sessions,details,provenance)
		 VALUES (?,?,'','',?,?,?,?,?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   stage=excluded.stage, owner=excluded.owner,
		   sessions=excluded.sessions, details=excluded.details`,
		a.TaskID, version, a.Legacy.Stage, a.Legacy.Owner, string(sessions), string(details),
		`{"bookkeeping":"written by the parent through work_legacy_put"}`); err != nil {
		return nil, err
	}
	// The bookkeeping moved, so the task did something: its updated_at moves
	// with it, and its version deliberately does not.
	if err = touchWorkTask(ctx, tx, a.TaskID, now); err != nil {
		return nil, err
	}
	return readWorkLegacy(ctx, tx, a.TaskID)
}

// workTaskVersionOf reports a task's current version and whether the work
// records own it at all.
func workTaskVersionOf(ctx context.Context, tx *sql.Tx, taskID string) (int64, bool, error) {
	var version int64
	err := tx.QueryRowContext(ctx, "SELECT version FROM sp_work_tasks WHERE id=?", taskID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return version, true, nil
}

// legacyLedgerTaskWrite is a ledger write that changes a task row.
func legacyLedgerTaskWrite(name string) bool {
	return name == "ledger_put" || name == "ledger_delete"
}

// legacyLedgerOwnedWrite is a ledger write the work records took over outright:
// choices live on a turn now, and a request becomes one.
func legacyLedgerOwnedWrite(name string) bool {
	switch name {
	case "ledger_choices_put", "ledger_choices_clear",
		"ledger_requests_claim", "ledger_requests_dispatch", "ledger_requests_ack":
		return true
	}
	return false
}
