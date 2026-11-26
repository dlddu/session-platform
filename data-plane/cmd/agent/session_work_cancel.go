// Cancelling a task, and the one-off that writes the status onto the tasks
// whose cancellation was recorded before there was a status to write it to.
//
// What cancelling deliberately does not do:
//
// It leaves the task's turns alone, and it leaves the records beside them
// alone. A cancelled task keeps every exchange it had, including a turn that is
// still open — a request nobody answered is what actually happened, and closing
// it here would write an end nobody gave. It also keeps what the turn's
// dispatch record says: an operation the parent already handed to another
// system happened, and calling the task off does not unhappen it. So there is
// no cleanup in this file at all. The status says where the task stopped; it
// does not rewrite how it got there.
//
// The backfill:
//
// Before `cancelled` existed, a parent that called a task off had two places to
// put the fact and neither of them was the status — its own bookkeeping, which
// nothing a person reads shows, and the title, where a management prefix was
// the only way to make it visible. The status filter read neither, so the task
// went on counting as delivered (session_work.go, workStatuses).
//
// task_cancel_backfill writes the status onto those tasks, and its rule for
// which ones is deliberately narrow: only a structured, explicit statement in
// the parent's own bookkeeping counts. Prose is not a statement, a string that
// reads as cancellation is not a statement, and the title prefix is not one
// either — a title is written for a person to read, and reading one as a
// machine-readable flag is how a task called "[취소] 취소 흐름 점검" gets called
// off for being about cancellation. Two fields that disagree, or one that is
// not the shape it has to be, are reported rather than resolved: this runs once
// over work a person already has, so guessing is the one thing it must not do.
//
// The prefix itself is management decoration, so a backfilled task has it taken
// off its title — and the title it had is kept verbatim in a record of its own,
// because the cleanup happens once and nobody can check a one-off against an
// original it threw away.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// taskCancelBackfillTool is the one-off. It is a work tool rather than a
	// script so that it runs where every other write runs: behind the same
	// schema gate, in one transaction, under a request_id that replays its
	// first committed answer. A script with its own handle on the file would be
	// a second writer with none of that.
	taskCancelBackfillTool = "task_cancel_backfill"

	// workCancelTitlePrefix is the decoration a parent put on a title when the
	// status could not carry the fact. It is matched at the start of a title
	// only, which is where a prefix is — a title that merely mentions it
	// somewhere is a person's words about cancellation, not a marker.
	workCancelTitlePrefix = "[취소]"
)

func taskCancelBackfillDefinition() map[string]any {
	description := "One-off: writes the cancelled status onto the tasks whose cancellation this parent " +
		"had already recorded in its own bookkeeping, back when there was no status to write it to. " +
		"A task is only written when the bookkeeping states it in a structured field — details.cancelled " +
		"as a JSON boolean true, or details.cancellation as a JSON object. Prose, a string that reads as " +
		"cancellation and a \"[취소]\" title prefix are never taken as the statement, and two fields that " +
		"disagree are reported instead of resolved. Each task written has the management prefix taken off " +
		"its title, and the title it had is kept verbatim and answered by task_get as original_title. " +
		"Turns are untouched: an open turn stays open and a recorded dispatch stays recorded. " +
		"dry_run reports exactly what it would write and writes nothing. Pages with after_id/limit; " +
		"pass next_cursor unchanged. Running it again writes nothing new — tasks already cancelled are " +
		"counted and left alone."
	return map[string]any{
		"name":        taskCancelBackfillTool,
		"description": description,
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"request_id": map[string]any{"type": "string"},
			"after_id":   map[string]any{"type": "string", "description": "Resume after this task id; pass next_cursor unchanged."},
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": workListLimitMax, "default": workListLimitDefault},
			"dry_run":    map[string]any{"type": "boolean", "description": "Report what would be written and write nothing."},
		}, "required": []string{"request_id"}, "additionalProperties": false},
	}
}

// taskCancelBackfillResult is what one run reports.
type taskCancelBackfillResult struct {
	// DryRun says nothing was written and the rest of this answer is what the
	// run would have done. It is how the one-off is reviewed before it is run.
	DryRun bool `json:"dry_run"`
	// Scanned is how many tasks this page looked at, so "found nothing to do"
	// can be told from "looked at nothing".
	Scanned int `json:"scanned"`
	// Cancelled is every task the status was written onto, with what the task
	// said before and which field said otherwise.
	Cancelled []cancelledTask `json:"cancelled"`
	// AlreadyCancelled is how many tasks on this page already carried the
	// status. A second run is expected to be all of these and nothing else.
	AlreadyCancelled int `json:"already_cancelled"`
	// CountedAsDelivered is how many of Cancelled had been written at the other
	// terminal status. It is the number this one-off exists for: those are the
	// tasks a status filter had been counting as work that was delivered.
	CountedAsDelivered int `json:"counted_as_delivered"`
	// Refused is every task whose bookkeeping this run would not act on, with
	// the reason. It is reported rather than passed over quietly — a one-off
	// that claims to have looked at every task owes an account of the ones it
	// decided not to touch.
	Refused []cancelRefusal `json:"refused"`
	// NextCursor is the id to resume after, empty when the scan reached the
	// end. A page is not a snapshot, so a resumed run re-reads nothing it has
	// already written: a task it wrote is simply already cancelled.
	NextCursor string `json:"next_cursor"`
}

// cancelledTask is one task the backfill wrote the status onto.
type cancelledTask struct {
	ID string `json:"id"`
	// FromStatus is what the task said before. It is kept in the answer because
	// it is the only place that fact survives — the write replaces it.
	FromStatus string `json:"from_status"`
	// Evidence names the structured field the statement came from, so a reader
	// of this answer can go and read the same field.
	Evidence string `json:"evidence"`
	// Title is the task's title after the management prefix was taken off, or
	// unchanged when there was nothing to take off.
	Title string `json:"title"`
	// OriginalTitle is the title as it was, present only when the prefix was
	// taken off. The same value is kept on the slot and answered by task_get.
	OriginalTitle string `json:"original_title,omitempty"`
	// TitleKept says the title is nothing but the prefix, so it was left as it
	// is: a task has to have a title, and an empty one is not an improvement on
	// a decorated one.
	TitleKept bool `json:"title_kept,omitempty"`
}

// cancelRefusal is one task the backfill deliberately did not touch, and why.
type cancelRefusal struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// cancelEvidence is what one task's bookkeeping says about having been called
// off: whether it says so, which field said it, and why the backfill will not
// act on what it found.
type cancelEvidence struct {
	// Field names the structured field the statement came from, and is empty
	// when the bookkeeping makes no statement either way. Silence is the
	// ordinary case: most tasks were never cancelled.
	Field string
	// Cancelled is what that field says. A field that says false is a statement
	// too, and it is deliberately not the same answer as silence.
	Cancelled bool
	// Refusal is why this bookkeeping is not acted on: two fields disagree, or
	// one is not the shape a structured statement has to be. Empty when there
	// is nothing wrong with it.
	Refusal string
}

// readCancelEvidence reads the parent's details JSON for an explicit, structured
// statement that this task was called off.
//
// The two shapes it accepts are the two a parent can write without ambiguity: a
// boolean that says so, and an object recorded beside the task when it happened.
// The object's presence is the whole statement — when it was cancelled, by whom
// and why are the parent's own and are not read here, because a backfill that
// started interpreting the contents would be back to reading prose.
//
// Everything else is refused rather than interpreted. `"cancelled": "true"` is
// not a boolean; a details JSON that does not parse says nothing about
// cancellation; and a details that both says false and carries a cancellation
// object is a disagreement, which is a thing to be told about rather than one
// to pick a side in.
func readCancelEvidence(details map[string]any) cancelEvidence {
	refuse := func(field, why string) cancelEvidence {
		return cancelEvidence{Field: field, Refusal: why}
	}
	flag, flagNamed := details["cancelled"]
	note, noteNamed := details["cancellation"]
	cancelled, flagIsBool := flag.(bool)
	_, noteIsObject := note.(map[string]any)
	switch {
	case flagNamed && !flagIsBool:
		return refuse("details.cancelled", "details.cancelled is not a JSON boolean, so the bookkeeping states nothing either way")
	case noteNamed && !noteIsObject:
		return refuse("details.cancellation", "details.cancellation is not a JSON object, so the bookkeeping states nothing either way")
	case flagNamed && noteNamed && !cancelled:
		return refuse("details.cancelled", "details.cancelled says false and details.cancellation says it was called off")
	case noteNamed:
		return cancelEvidence{Field: "details.cancellation", Cancelled: true}
	case flagNamed:
		return cancelEvidence{Field: "details.cancelled", Cancelled: cancelled}
	}
	return cancelEvidence{}
}

// cleanCancelTitle takes the management prefix off a title and reports whether
// it took anything.
//
// Every leading occurrence goes, because a title that was decorated twice was
// decorated by the same hand for the same reason. A title that is nothing but
// prefixes is returned unchanged: a task has to have a title, and the backfill
// reports that it kept this one rather than leaving an empty string behind.
func cleanCancelTitle(title string) (string, bool) {
	cleaned := strings.TrimSpace(title)
	for strings.HasPrefix(cleaned, workCancelTitlePrefix) {
		cleaned = strings.TrimSpace(strings.TrimPrefix(cleaned, workCancelTitlePrefix))
	}
	if cleaned == "" || cleaned == strings.TrimSpace(title) {
		return title, false
	}
	return cleaned, true
}

// titleCarriesCancelPrefix reports whether a title starts with the management
// prefix. It is asked about a title cleanCancelTitle left alone, which is how
// "there was nothing to clean" is told from "cleaning it would have left
// nothing".
func titleCarriesCancelPrefix(title string) bool {
	return strings.HasPrefix(strings.TrimSpace(title), workCancelTitlePrefix)
}

// cancelCandidate is one task as the scan reads it, before anything is decided
// about it.
type cancelCandidate struct {
	id      string
	title   string
	status  string
	version int64
}

// backfillCancelledTasks is the one-off itself.
//
// It names no task and holds no version, because it is not a parent changing a
// task it read: it finds its own candidates from what the bookkeeping already
// says. Everything it does rides the caller's one transaction, so a run either
// lands whole or lands not at all, and the receipt under request_id makes a
// retried call answer with what the first one committed rather than scanning
// again.
func backfillCancelledTasks(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = workListLimitDefault
	}
	dryRun := a.DryRun != nil && *a.DryRun
	// The page is read out in full before anything is written. A scan that
	// wrote while its own rows were still open would be reading and writing the
	// same table through one connection, which is a deadlock waiting for a
	// slot large enough to find it.
	candidates, more, err := readCancelCandidates(ctx, tx, a.AfterID, limit)
	if err != nil {
		return nil, err
	}
	result := taskCancelBackfillResult{
		DryRun:    dryRun,
		Cancelled: []cancelledTask{},
		Refused:   []cancelRefusal{},
	}
	if more && len(candidates) > 0 {
		result.NextCursor = candidates[len(candidates)-1].id
	}
	for _, task := range candidates {
		result.Scanned++
		if task.status == "cancelled" {
			result.AlreadyCancelled++
			continue
		}
		evidence, bookErr := cancelStatementOf(ctx, tx, task.id)
		if bookErr != nil {
			return nil, bookErr
		}
		if evidence.Refusal != "" {
			result.Refused = append(result.Refused, cancelRefusal{ID: task.id, Reason: evidence.Refusal})
			continue
		}
		if !evidence.Cancelled {
			continue
		}
		title, cleaned := cleanCancelTitle(task.title)
		written := cancelledTask{
			ID: task.id, FromStatus: task.status, Evidence: evidence.Field, Title: title,
		}
		if cleaned {
			written.OriginalTitle = task.title
		} else if titleCarriesCancelPrefix(task.title) {
			written.TitleKept = true
		}
		if isTerminalWorkStatus(task.status) {
			// It was already finished — under the one terminal name that says
			// the completion criteria were met. These are the rows the one-off
			// exists for, so they are counted on their own rather than left for
			// a reader to pick out of the list.
			result.CountedAsDelivered++
		}
		result.Cancelled = append(result.Cancelled, written)
		if dryRun {
			continue
		}
		if err := writeCancelledTask(ctx, tx, task, title, cleaned, now); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// readCancelCandidates reads one id-keyset page of tasks and says whether more
// follow. It asks for one row past the page so that a full page is not reported
// with a cursor that leads to an empty one.
func readCancelCandidates(
	ctx context.Context, tx *sql.Tx, afterID string, limit int,
) ([]cancelCandidate, bool, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT id,title,status,version FROM sp_work_tasks WHERE id>? ORDER BY id LIMIT ?",
		afterID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	candidates := []cancelCandidate{}
	more := false
	for rows.Next() {
		var found cancelCandidate
		if err := rows.Scan(&found.id, &found.title, &found.status, &found.version); err != nil {
			return nil, false, err
		}
		if len(candidates) == limit {
			more = true
			break
		}
		candidates = append(candidates, found)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return candidates, more, nil
}

// cancelStatementOf answers what one task's bookkeeping says. A task with no
// bookkeeping at all says nothing, which is neither a statement nor a refusal —
// it is the ordinary case for a task nobody ever cancelled.
func cancelStatementOf(ctx context.Context, tx *sql.Tx, taskID string) (cancelEvidence, error) {
	book, found, err := readTaskBook(ctx, tx, taskID)
	if err != nil || !found {
		return cancelEvidence{}, err
	}
	details := map[string]any{}
	if strings.TrimSpace(book.details) != "" {
		if err := json.Unmarshal([]byte(book.details), &details); err != nil {
			// Bookkeeping this code cannot read is not a statement about
			// cancellation, and it is not silence either: something is in there
			// and nothing here can tell what. Reported, not guessed at.
			return cancelEvidence{Refusal: "the bookkeeping details are not readable JSON"}, nil
		}
	}
	return readCancelEvidence(details), nil
}

// writeCancelledTask writes the status, and the title the task had before the
// prefix came off it.
func writeCancelledTask(
	ctx context.Context, tx *sql.Tx, task cancelCandidate, title string, cleaned bool, now time.Time,
) error {
	// The title a person had been reading is kept before the cleaned one
	// replaces it, and the first one recorded wins: a second run over a task
	// whose title is already clean must not be able to record the clean title
	// as the original.
	if cleaned {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sp_work_task_title_original (task_id,title,recorded_at) VALUES (?,?,?)
			 ON CONFLICT(task_id) DO NOTHING`,
			task.id, task.title, workNow(now)); err != nil {
			return err
		}
	}
	// The status is a new state of the task, so its version moves — a parent
	// holding the version it read has to read again, exactly as after any other
	// state change. The version is in the WHERE clause so the test and the
	// write are one statement.
	written, err := tx.ExecContext(ctx,
		`UPDATE sp_work_tasks SET title=?,status=?,version=version+1,updated_at=?
		 WHERE id=? AND version=?`,
		title, "cancelled", workNow(now), task.id, task.version)
	if err != nil {
		return err
	}
	// Checked, not assumed, the way every other write here checks: an update
	// that matched nothing is a row that moved, not a task that was backfilled.
	changed, err := written.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return workFail("conflict", "task "+task.id+" changed while the backfill was running; run it again")
	}
	return nil
}

// originalTitleOf answers the title a task had before the backfill took the
// management prefix off it.
//
// It is answered by task_get rather than kept only on the slot, because
// preserved and unreachable is the same as lost (session_work_parent.go). A
// task whose title was never decorated has none, and that absence is not a
// gap: there is nothing it could be compared with.
func originalTitleOf(ctx context.Context, tx *sql.Tx, taskID string) (string, bool, error) {
	var title string
	err := tx.QueryRowContext(ctx,
		"SELECT title FROM sp_work_task_title_original WHERE task_id=?", taskID).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return title, true, nil
}
