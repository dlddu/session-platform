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
	Stage    string            `json:"stage"`
	Owner    string            `json:"owner"`
	Sessions []workBookSession `json:"sessions"`
	Details  map[string]any    `json:"details"`
}

// workLegacyRecord is what a reader gets back: the input plus the facts that
// say what it belongs to and whether it is still a task.
type workLegacyRecord struct {
	TaskID       string            `json:"task_id"`
	Kind         string            `json:"kind"`
	LegacyStatus string            `json:"legacy_status"`
	Stage        string            `json:"stage"`
	Owner        string            `json:"owner"`
	Sessions     []workBookSession `json:"sessions"`
	Details      map[string]any    `json:"details"`
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
	// taskBookGetTool/taskBookPutTool are the parent's own record beside a task,
	// named for what it is rather than for where it came from. The bookkeeping
	// is not residue — a parent writes it on tasks that never saw a ledger — so
	// it outlives the move. The names the cutover shipped (work_legacy_get and
	// work_legacy_put) are gone; a call to one is answered with the name here
	// (session_work.go, workRemovedToolSuccessor).
	taskBookGetTool = "task_book_get"
	taskBookPutTool = "task_book_put"

	// workLegacyGetTool/workLegacyPutTool are those removed names, kept only so
	// that a parent still calling one is told which call does the job.
	workLegacyGetTool = "work_legacy_get"
	workLegacyPutTool = "work_legacy_put"
)

func taskBookToolDefinitions() []map[string]any {
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
	getDescription := "The parent's own bookkeeping beside one task: stage, owner, the session relations with their cursors and seqs, and the details JSON holding approval scope, preservation and cancellation state. It is not part of the agreed task, and nothing a person reads shows it. book_version is the compare-and-swap value " + taskBookPutTool + " must hold; task_version is the task state these notes are about, shown for reference. Reading changes nothing."
	putDescription := "Replaces that bookkeeping for one task the work records own — advance a session cursor after collecting a worker's result, record an approval scope, note preservation or a cancellation. Full replacement: every field is required, so read-modify-write rather than sending the one field that changed. expected_book_version must be the book_version the read returned, and the swap is atomic, so of two parents that both read this record exactly one can replace it and the other is told to read again. It is a counter of its own, so writing bookkeeping moves neither the task's version nor the turn lane's — advancing a cursor never invalidates the choices a turn is offering. request_id replays the original committed answer. A monitoring loop is not a task and is not written here: it has its own record (monitor_put)."
	getSchema := map[string]any{"type": "object", "properties": map[string]any{"task_id": str()},
		"required": []string{"task_id"}, "additionalProperties": false}
	putSchema := map[string]any{"type": "object", "properties": map[string]any{
		"task_id": str(), "request_id": str(),
		"expected_book_version": map[string]any{"type": "integer", "minimum": 1},
		"record":                record,
	}, "required": []string{"task_id", "request_id", "expected_book_version", "record"}, "additionalProperties": false}
	return []map[string]any{
		{"name": taskBookGetTool, "description": getDescription, "inputSchema": getSchema},
		{"name": taskBookPutTool, "description": putDescription, "inputSchema": putSchema},
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
		TaskID: taskID, Sessions: []workBookSession{}, Details: map[string]any{}, TaskVersion: version,
	}
	if owned {
		_, book, guardErr := workGuards(ctx, tx, taskID)
		if guardErr != nil {
			return nil, guardErr
		}
		record.BookVersion = book
	}
	book, found, err := readTaskBook(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if !found {
		if !owned {
			return nil, workFail("not_found", "this slot has no task or preserved row with that id")
		}
		return record, nil
	}
	stage, owner, sessions, details := book.stage, book.owner, book.sessions, book.details
	record.Stage, record.Owner, record.Kind, record.LegacyStatus = stage, owner, book.kind, book.legacyStatus
	if sessions != "" {
		var relations []workBookSession
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
	_, owned, err := workTaskVersionOf(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	if !owned {
		// Either there is no such task, or it is a preserved row the work
		// records do not own. The second keeps the ledger tools it always had,
		// and saying so is more use than a bare not_found.
		_, preserved, err := readTaskBook(ctx, tx, a.TaskID)
		if err != nil {
			return nil, err
		}
		if preserved {
			return nil, workFail("conflict",
				"no work task owns that id — it is a preserved row, which is written with ledger_put as before")
		}
		return nil, workFail("not_found", "task not found")
	}
	if a.ExpectedBookVersion == nil {
		return nil, workFail("invalid_argument", "expected_book_version is required")
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
	// The bookkeeping goes in its own record, not in the table that holds what a
	// migration preserved. Writing it there is what made "has bookkeeping" and
	// "came from the ledger" the same fact, so a parent that recorded an
	// approval scope on a task created after the cutover made that task read as
	// migrated — and sent a reader looking for a ledger history it never had.
	// The preserved copy stays exactly as the migration wrote it; this record is
	// the live one and is what a read answers with.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_task_book (task_id,stage,owner,sessions,details)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   stage=excluded.stage, owner=excluded.owner,
		   sessions=excluded.sessions, details=excluded.details`,
		a.TaskID, a.Legacy.Stage, a.Legacy.Owner, string(sessions), string(details)); err != nil {
		return nil, err
	}
	// The bookkeeping moved, so the task did something: its updated_at moves
	// with it, and its version deliberately does not.
	if err = touchWorkTask(ctx, tx, a.TaskID, now); err != nil {
		return nil, err
	}
	return readWorkLegacy(ctx, tx, a.TaskID)
}

// taskBook is one task's bookkeeping as the storage holds it, with the two
// facts only a migration could know (kind, legacy_status) when they are there.
type taskBook struct {
	stage, owner, sessions, details string
	kind, legacyStatus              string
}

// readTaskBook answers one task's bookkeeping from whichever record holds it.
// The owned record wins: a parent's write goes there, so reading the preserved
// copy first would answer with the migration's original after every update.
// The preserved copy is the fallback, which is what makes this switch invisible
// to a slot that migrated before the owned record existed and has not been
// written since.
func readTaskBook(ctx context.Context, tx *sql.Tx, taskID string) (taskBook, bool, error) {
	var book taskBook
	err := tx.QueryRowContext(ctx,
		"SELECT stage,owner,sessions,details FROM sp_work_task_book WHERE task_id=?", taskID).
		Scan(&book.stage, &book.owner, &book.sessions, &book.details)
	if err == nil {
		// kind and legacy_status describe what the ledger row was, so they come
		// from the preserved copy when it is still there and are empty once it
		// is not: an invented legacy status is worse than none.
		preserved, found, factErr := readPreservedTask(ctx, tx, taskID)
		if factErr != nil {
			return taskBook{}, false, factErr
		}
		if found {
			book.kind, book.legacyStatus = preserved.kind, preserved.legacyStatus
		}
		return book, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return taskBook{}, false, err
	}
	return readPreservedTask(ctx, tx, taskID)
}

// readPreservedTask reads the verbatim copy a migration made. The table is gone
// on a retired slot, so its absence is "no preserved copy" rather than an error.
func readPreservedTask(ctx context.Context, tx *sql.Tx, taskID string) (taskBook, bool, error) {
	present, err := workTableExists(ctx, tx, "sp_work_task_legacy")
	if err != nil || !present {
		return taskBook{}, false, err
	}
	var book taskBook
	err = tx.QueryRowContext(ctx,
		`SELECT stage,owner,sessions,details,kind,legacy_status
		 FROM sp_work_task_legacy WHERE task_id=?`, taskID).
		Scan(&book.stage, &book.owner, &book.sessions, &book.details, &book.kind, &book.legacyStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return taskBook{}, false, nil
	}
	if err != nil {
		return taskBook{}, false, err
	}
	return book, true, nil
}

// workTableExists reports whether a table is in this database. Retirement drops
// the transition tables, so every reader of one asks first rather than failing
// on a table that was never meant to be there for ever.
func workTableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count)
	return count != 0, err
}

// bookkeepingProvenance marks a preserved row a parent wrote rather than a
// migration. Rows written before the owned record existed carry it, and
// retirement reads it to tell "this task came from the ledger" from "a parent
// recorded an approval scope on a task that never saw one".
const bookkeepingProvenanceKey = "bookkeeping"

// taskCameFromLedger reports whether a migration created this task, which is
// what a reader outside the session is told by `migrated` — and therefore what
// sends it to the ledger's own history route or keeps it away from one that does
// not exist. It is deliberately not "has bookkeeping": a parent writes
// bookkeeping on tasks that were created here.
func taskCameFromLedger(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var origin string
	err := tx.QueryRowContext(ctx,
		"SELECT origin FROM sp_work_task_origin WHERE task_id=?", taskID).Scan(&origin)
	if err == nil {
		return origin == workOriginLedger, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// No origin recorded: this slot migrated before the origin was written down.
	// A preserved copy whose provenance is not a parent's own write is the
	// migration's, and that is the same answer the origin row would give.
	present, err := workTableExists(ctx, tx, "sp_work_task_legacy")
	if err != nil || !present {
		return false, err
	}
	var provenance string
	err = tx.QueryRowContext(ctx,
		"SELECT provenance FROM sp_work_task_legacy WHERE task_id=?", taskID).Scan(&provenance)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !isBookkeepingProvenance(provenance), nil
}

func isBookkeepingProvenance(provenance string) bool {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(provenance), &decoded); err != nil {
		return false
	}
	_, written := decoded[bookkeepingProvenanceKey]
	return written
}

// workOriginLedger is the one origin a task can have besides being written
// here: it came from this slot's ledger.
const workOriginLedger = "ledger"

// turnOrigin is where one turn came from, and the two facts that travel with
// that: the id it was stored under outside this table, and who a person's words
// were from. All three outlive the transition — `origin` is on the read path a
// person sees, `source_id` is what keeps a stored request idempotent across a
// retry, and `author` is what a ledger-shaped reader shows beside the words —
// which is why they are an owned record rather than part of the preserved copy.
type turnOrigin struct {
	origin   string
	sourceID string
	author   string
}

const (
	// workTurnOriginUserRequest is a turn a migration made from a request the
	// ledger had stored, and workTurnOriginAppRequest one a person wrote through
	// the request route after the cutover. Both are on the read path a person
	// sees.
	workTurnOriginUserRequest = "user_request"
	workTurnOriginAppRequest  = "app_request"
	// workTurnOriginExported is a turn this session wrote that a rollback export
	// copied into the ledger. It is bookkeeping for the way back, not where the
	// exchange came from, so it is not shown as an origin.
	workTurnOriginExported = "exported_turn"
)

func turnOriginOf(ctx context.Context, tx *sql.Tx, turnID int64) (turnOrigin, bool, error) {
	var found turnOrigin
	err := tx.QueryRowContext(ctx,
		"SELECT origin,source_id,author FROM sp_work_turn_origin WHERE turn_id=?", turnID).
		Scan(&found.origin, &found.sourceID, &found.author)
	if err == nil {
		return found, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return turnOrigin{}, false, err
	}
	present, err := workTableExists(ctx, tx, "sp_work_turn_legacy")
	if err != nil || !present {
		return turnOrigin{}, false, err
	}
	var admin string
	err = tx.QueryRowContext(ctx,
		"SELECT source,source_id,admin FROM sp_work_turn_legacy WHERE turn_id=?", turnID).
		Scan(&found.origin, &found.sourceID, &admin)
	if errors.Is(err, sql.ErrNoRows) {
		return turnOrigin{}, false, nil
	}
	if err != nil {
		return turnOrigin{}, false, err
	}
	found.author = adminAuthor(admin)
	return found, true, nil
}

func adminAuthor(admin string) string {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(admin), &decoded); err != nil {
		return ""
	}
	author, _ := decoded["author"].(string)
	return author
}

// turnOfOriginSource finds the turn one outside id already became. It asks both
// records because a slot that migrated before the owned one existed has its
// mappings in the preserved copy — and missing one of them is how a retried
// request would become a second turn.
func turnOfOriginSource(ctx context.Context, tx *sql.Tx, origin, sourceID string) (int64, bool, error) {
	var turnID int64
	err := tx.QueryRowContext(ctx,
		"SELECT turn_id FROM sp_work_turn_origin WHERE origin=? AND source_id=?", origin, sourceID).Scan(&turnID)
	if err == nil {
		return turnID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	present, err := workTableExists(ctx, tx, "sp_work_turn_legacy")
	if err != nil || !present {
		return 0, false, err
	}
	err = tx.QueryRowContext(ctx,
		"SELECT turn_id FROM sp_work_turn_legacy WHERE source=? AND source_id=?", origin, sourceID).Scan(&turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return turnID, true, nil
}

// turnSourceTaken reports whether any record already maps this outside id to a
// turn, whatever the origin. A migration asks it before mapping a request: a
// rollback export writes exported turns back as requests, and on the way forward
// those must be recognised rather than imported a second time.
func turnSourceTaken(ctx context.Context, tx *sql.Tx, sourceID string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_turn_origin WHERE source_id=?", sourceID).Scan(&count); err != nil {
		return false, err
	}
	if count != 0 {
		return true, nil
	}
	present, err := workTableExists(ctx, tx, "sp_work_turn_legacy")
	if err != nil || !present {
		return false, err
	}
	err = tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_turn_legacy WHERE source_id=?", sourceID).Scan(&count)
	return count != 0, err
}

func recordTurnOrigin(ctx context.Context, tx *sql.Tx, turnID int64, origin, sourceID, author string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_turn_origin (turn_id,origin,source_id,author) VALUES (?,?,?,?)
		 ON CONFLICT(turn_id) DO NOTHING`,
		turnID, origin, sourceID, author)
	return err
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
