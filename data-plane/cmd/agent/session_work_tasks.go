package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The task side of the work records. A task is replaced whole under its
// version, the way the ledger task was: a parent reads, changes what it means
// to change, and writes the version it read — so two parents cannot both
// believe they moved the same task.

const workTaskColumns = `id,session_id,title,description,completion_criteria,status,version,created_at,updated_at`

func scanWorkTask(row interface{ Scan(...any) error }) (workTask, error) {
	var task workTask
	var sessionID, description, criteria, createdAt, updatedAt sql.NullString
	err := row.Scan(&task.ID, &sessionID, &task.Title, &description, &criteria,
		&task.Status, &task.Version, &createdAt, &updatedAt)
	if err != nil {
		return task, err
	}
	task.SessionID = nullableString(sessionID)
	task.Description = nullableString(description)
	task.CompletionCriteria = nullableString(criteria)
	task.CreatedAt = nullableString(createdAt)
	task.UpdatedAt = nullableString(updatedAt)
	return task, nil
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func loadWorkTask(ctx context.Context, tx *sql.Tx, id string) (workTask, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+workTaskColumns+" FROM sp_work_tasks WHERE id=?", id)
	task, err := scanWorkTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return task, workFail("not_found", "task not found")
	}
	return task, err
}

// readWorkTask answers one task with the two facts a reader would otherwise
// need a second call for: how many turns it has, and the last one — which is
// where an unanswered request or an open question sits.
func readWorkTask(ctx context.Context, tx *sql.Tx, id string) (any, error) {
	task, err := loadWorkTask(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	total, latest, err := workTurnTail(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	turnVersion, bookVersion, err := workGuards(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	answer, err := resolveWorkTaskAnswer(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	// The counters travel with the task because a writer needs them before it
	// can write: they are the compare-and-swap values for turns and for the
	// parent's bookkeeping (session_work_guard.go).
	result := map[string]any{
		"task": answer, "turns_total": total,
		"turn_version": turnVersion, "book_version": bookVersion,
	}
	if latest != nil {
		result["latest_turn"] = *latest
	}
	return result, nil
}

// listWorkTasks pages tasks by id. session_id="none" selects the unassigned
// ones: an empty filter cannot express "no session" because an empty string is
// also "no filter".
//
// The status filter selects on the status a reader *sees*, which for a row an
// older image wrote as `blocked` is decided from evidence beside it
// (session_work_review.go). That cannot be one SQL predicate without this file
// and that one drifting apart, so the page is built in two steps: the query
// selects every row that could resolve to the asked-for status, and the
// resolution happens over the rows that come back.
//
// Which means the page is cut *after* the resolution, never before. A post-pass
// that filtered an already-cut page would hand back short pages and, worse,
// quietly drop the rows it skipped — so this keeps reading until the page is
// full or the candidates run out. Nothing is lost: the cursor is still an id
// and still moves forward only, so resuming after it re-reads the skipped rows
// and skips them again rather than stepping over work.
//
// The statuses partition the tasks: every task answers to exactly one of them,
// a legacy row included, so paging one status after another sees every task
// once and no task twice.
func listWorkTasks(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = workListLimitDefault
	}
	// Resolved by validateWork before this runs, so an unknown status never
	// reaches the query; re-resolving here keeps the sidecar view, which calls
	// this directly, on the same rule.
	want, known := workStatusFilter(a.Status)
	if !known {
		return nil, workFail("invalid_argument", "status must be "+strings.Join(workStatuses, ", "))
	}
	predicate, statusArgs := workStatusCandidates(want)
	cursor := a.AfterID
	tasks := []workTaskAnswer{}
	scanned := 0
	for len(tasks) < limit && scanned < workListScanMax {
		batch, batchErr := readWorkTaskBatch(ctx, tx, a, predicate, statusArgs, cursor, workListChunk)
		if batchErr != nil {
			return nil, batchErr
		}
		// A short run is the last one: there is no candidate after it. That is
		// only the end of the *answer* if the page did not fill up first —
		// filling it can leave rows of this very run unread.
		last := len(batch) < workListChunk
		for _, row := range batch {
			cursor = row.ID
			scanned++
			answer, resolveErr := resolveWorkTaskAnswer(ctx, tx, row)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if want != "" && answer.Status != want {
				continue
			}
			tasks = append(tasks, answer)
			if len(tasks) == limit {
				break
			}
		}
		if last && len(tasks) < limit {
			// The candidates ran out before the page filled, so there is
			// nothing after it whatever happened to the ones that were skipped.
			return map[string]any{"tasks": tasks, "next_cursor": ""}, nil
		}
		if last {
			break
		}
	}
	// A full page says nothing about whether more exists, and a page cut short
	// by the scan bound says nothing either — so ask, rather than report an
	// empty cursor that would read as "that was everything".
	more, err := workTasksRemain(ctx, tx, a, predicate, statusArgs, cursor)
	if err != nil {
		return nil, err
	}
	next := ""
	if more {
		next = cursor
	}
	return map[string]any{"tasks": tasks, "next_cursor": next}, nil
}

const (
	// workListChunk is how many candidate rows one read takes. It is the page
	// cap rather than the caller's limit so that a status whose candidates are
	// mostly other statuses costs round trips in proportion to the table, not
	// to the page size.
	workListChunk = workListLimitMax
	// workListScanMax bounds the candidates one call resolves. A page cut short
	// by it comes back with a cursor rather than an empty one, so the caller
	// reads the next page instead of believing it has seen everything — the
	// bound costs a page, never a row.
	workListScanMax = 2000
)

// workStatusCandidates is the SQL for "could resolve to this status". A row
// stored as the legacy word is a candidate for the two statuses it can resolve
// to and for no others, so every bucket a reader can ask for selects a superset
// of itself and the resolution narrows it.
func workStatusCandidates(want string) (string, []any) {
	switch want {
	case "":
		return "1=1", nil
	case workStatusReview, workStatusRunning:
		return "status IN (?,?)", []any{want, workStatusBlockedLegacy}
	default:
		return "status=?", []any{want}
	}
}

// readWorkTaskBatch reads one run of candidate rows after a cursor, in id
// order. The session filter is the caller's and is applied in SQL: it needs no
// resolution, so there is nothing to be gained by carrying those rows out here.
func readWorkTaskBatch(
	ctx context.Context, tx *sql.Tx, a workArgs, predicate string, statusArgs []any, cursor string, limit int,
) ([]workTask, error) {
	query := "SELECT " + workTaskColumns + ` FROM sp_work_tasks
		WHERE id>? AND (` + predicate + `)
		  AND (?='' OR (?='none' AND session_id IS NULL) OR session_id=?)
		ORDER BY id LIMIT ?`
	args := []any{cursor}
	args = append(args, statusArgs...)
	args = append(args, a.SessionID, a.SessionID, a.SessionID, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batch := []workTask{}
	for rows.Next() {
		task, scanErr := scanWorkTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		batch = append(batch, task)
	}
	return batch, rows.Err()
}

// workTasksRemain reports whether any candidate sits after the cursor. It
// answers about candidates rather than about rows that would survive the
// resolution, which is the honest answer to "is there another page": a page
// that turns out to hold nothing is still a page the caller has to read, and
// resolving the whole tail here to avoid it would cost the scan this bound
// exists to limit.
func workTasksRemain(
	ctx context.Context, tx *sql.Tx, a workArgs, predicate string, statusArgs []any, cursor string,
) (bool, error) {
	batch, err := readWorkTaskBatch(ctx, tx, a, predicate, statusArgs, cursor, 1)
	return len(batch) != 0, err
}

// putWorkTask creates or replaces one task. created_at is written once and then
// carried: a replacement is a new state of the same task, not a new task.
func putWorkTask(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	var current workTask
	existing := true
	row := tx.QueryRowContext(ctx, "SELECT "+workTaskColumns+" FROM sp_work_tasks WHERE id=?", a.ID)
	current, err := scanWorkTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		existing = false
	} else if err != nil {
		return nil, err
	}
	stamp := workNow(now)
	// `blocked` is accepted and stored as review: an image older than this one
	// cannot know the new word, and refusing its write would stop work a person
	// left rather than move it. Nothing is stored under the old word, so no
	// write here adds a row the read path has to resolve later.
	status, aliased := workTaskInputStatus(*a.Task.Status)
	if !existing {
		if *a.ExpectedVersion != 0 {
			return nil, workFail("not_found", "task not found")
		}
		var retired int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_retired WHERE id=?", a.ID).Scan(&retired); err != nil {
			return nil, err
		}
		if retired != 0 {
			return nil, workFail("conflict", "deleted task ID cannot be reused")
		}
		task := workTask{
			ID: a.ID, SessionID: a.Task.SessionID, Title: *a.Task.Title,
			Description: a.Task.Description, CompletionCriteria: a.Task.CompletionCriteria,
			Status: status, Version: 1, CreatedAt: &stamp, UpdatedAt: &stamp,
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO sp_work_tasks ("+workTaskColumns+") VALUES (?,?,?,?,?,?,?,?,?)",
			task.ID, task.SessionID, task.Title, task.Description, task.CompletionCriteria,
			task.Status, task.Version, task.CreatedAt, task.UpdatedAt)
		if err != nil {
			return nil, err
		}
		return workTaskPutAnswer(task, aliased), nil
	}
	if *a.ExpectedVersion == 0 || *a.ExpectedVersion != current.Version {
		return nil, workFail("conflict", "expected_version does not match")
	}
	if err := refuseToOrphanAnOpenReview(ctx, tx, a.ID, status); err != nil {
		return nil, err
	}
	task := workTask{
		ID: a.ID, SessionID: a.Task.SessionID, Title: *a.Task.Title,
		Description: a.Task.Description, CompletionCriteria: a.Task.CompletionCriteria,
		Status: status, Version: current.Version + 1,
		CreatedAt: current.CreatedAt, UpdatedAt: &stamp,
	}
	// The expected version is in the WHERE clause, so the test and the write
	// are one statement — and the row count is checked rather than assumed: an
	// UPDATE that matched nothing is a lost race, not a success.
	result, err := tx.ExecContext(ctx,
		`UPDATE sp_work_tasks SET session_id=?,title=?,description=?,completion_criteria=?,status=?,version=?,updated_at=?
		 WHERE id=? AND version=?`,
		task.SessionID, task.Title, task.Description, task.CompletionCriteria, task.Status,
		task.Version, task.UpdatedAt, a.ID, *a.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, workFail("conflict", "expected_version does not match")
	}
	return workTaskPutAnswer(task, aliased), nil
}

// workTaskPutAnswer is what a put answers with. alias_applied names the word
// the caller sent when it was not the word that landed, so a writer is never
// left believing this slot holds something it does not.
func workTaskPutAnswer(task workTask, aliased bool) map[string]any {
	answer := map[string]any{"task": workTaskAnswer{workTask: task}}
	if aliased {
		answer["alias_applied"] = workStatusBlockedLegacy
	}
	return answer
}

// The status a task_put writes is the status it writes. There is deliberately
// **no transition constraint** here, and that is the agreed contract rather
// than an omission.
//
// The owner's own target-state document says so in as many words — the record
// is a full replacement under version CAS and the status value has no
// transition constraint — and it says why: the correction it names depends on
// it. A task written `completed` when the person had in fact decided nothing is
// put back to `review` once they say so and the monitor checks the turns. That
// is not a normal transition, which is why no arrow is drawn for it, and a gate
// here would make it reachable only by first writing a state that never
// happened.
//
// What carries the *evidence* is a call of its own rather than a fence
// (session_work_review.go): task_review_open records the exact target, the
// scope, the official route, the question and the basis in the same
// transaction as the status, and the two closing calls require the named
// confirmation and the completion checks. Those are the **complete** path into
// and out of the one status a person's judgement owns — not a wall around the
// bare write. A `review` written bare is a task whose decision nobody
// recorded, and that is exactly what a reader is then shown: nothing asked.
//
// An earlier revision of this file did fence it, and was wrong twice over. It
// contradicted that contract, and it refused the *new* word while letting the
// legacy alias through — so `status: "blocked"` landed and `status: "review"`
// did not, which is the reverse of what folding the two statuses was for.

// refuseToOrphanAnOpenReview is the one thing a bare status write is stopped
// from doing: walking away from a decision this slot has recorded and nobody
// has settled.
//
// It is **not** the transition fence this file used to have, and the difference
// is the whole point. That one asked "which status is this moving between" and
// was wrong for it — the agreed contract has no transition constraint, and the
// `completed` correction depends on there being none. This asks "is there an
// open record that this write would leave behind", which is a question about
// consistency rather than about permission:
//
//   - A task with no recorded review moves anywhere, as before. planned →
//     running, running → completed, a `completed` put back to review, an older
//     writer's alias — every one of those is untouched, because none of them
//     orphans anything.
//   - A task that **carries an open review** may not be written to running or
//     completed by a bare put. Those two are the statuses that say the work
//     moved on, and a task that reads as moved-on while holding an unsettled
//     question hides that question from every reader: the status filter puts it
//     with the work in flight, and the only record that somebody is waiting
//     sits in bookkeeping nothing on the read path shows.
//
// Why those two and not every status: `planned` is where a stored request
// legitimately puts such a task — received, nobody has picked it up, decision
// still open (session_work_views.go) — and `review` is where it already is.
// `unknown` is "this slot cannot say", and refusing a writer who is admitting
// that would be insisting on a certainty nobody has.
//
// It is also not a dead end, which is what keeps it from being a gate. There
// are two ways past it and both are honest: settle the review with the call
// that carries the evidence (task_review_resume / task_review_complete), or —
// if there is genuinely nothing to settle — replace the bookkeeping with
// task_book_put, which is the parent's own lane and has always been how
// `details` is written. What is refused is only doing neither.
func refuseToOrphanAnOpenReview(ctx context.Context, tx *sql.Tx, taskID, status string) error {
	if status != workStatusRunning && status != workStatusCompleted {
		return nil
	}
	details, err := readReviewDetails(ctx, tx, taskID)
	if errors.Is(err, errUnparseableBook) {
		// Bookkeeping this image cannot read says nothing either way, and it is
		// left exactly as it is elsewhere for the same reason. Refusing an
		// ordinary status write on the strength of bytes nobody can parse would
		// strand the task.
		return nil
	}
	if err != nil {
		return err
	}
	open, hasOpen := openReviewOf(details)
	if !hasOpen {
		return nil
	}
	return workFail("conflict", "this task carries an open review ("+reviewSubjectLine(open)+
		") and task_put would leave it behind at "+status+" — a task that reads as moved on while "+
		"somebody is still waiting hides the question from every reader. Settle it with "+
		taskReviewResumeTool+" (naming what confirmed the decision) or "+taskReviewCompleteTool+
		", or — if there is nothing to settle — replace the bookkeeping with "+taskBookPutTool)
}

// deleteWorkTask removes a task with its turns and retires the id. What a
// migration preserved about the task (sp_work_task_legacy) is left: the parent's
// own bookkeeping outlives the task row, exactly as the ledger's history does.
func deleteWorkTask(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	current, err := loadWorkTask(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}
	if current.Version != *a.ExpectedVersion {
		return nil, workFail("conflict", "expected_version does not match")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sp_work_turns WHERE task_id=?", a.ID); err != nil {
		return nil, err
	}
	deleted, err := tx.ExecContext(ctx, "DELETE FROM sp_work_tasks WHERE id=? AND version=?", a.ID, current.Version)
	if err != nil {
		return nil, err
	}
	// Checked, not assumed: a delete that matched nothing means the version
	// moved under this caller, which is a lost race rather than a deletion.
	gone, err := deleted.RowsAffected()
	if err != nil {
		return nil, err
	}
	if gone != 1 {
		return nil, workFail("conflict", "expected_version does not match")
	}
	if err := dropWorkGuard(ctx, tx, a.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO sp_work_retired VALUES (?)", a.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": a.ID, "version": current.Version, "deleted": true}, nil
}

// touchWorkTask moves a task's updated_at without moving its version. A turn is
// an event on the task rather than a new state of it, so a parent holding the
// version it read can still write the task it was working on.
func touchWorkTask(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE sp_work_tasks SET updated_at=? WHERE id=?", workNow(now), id)
	return err
}
