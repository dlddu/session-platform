package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The turn side of the work records.
//
// A turn's id comes from the database and rises across the whole database, so
// ordering turns by id is ordering them by when they happened — which is why
// there is no sequence column, no link to the previous turn and no per-turn
// status. "Still needs the parent" is not stored either: it is a request with
// no ended_at, so a turn cannot claim to be open while carrying a final answer.
//
// Choices live on the turn that offered them, which is what keeps an earlier
// turn's choices readable as history while making only the latest turn's
// choices current (checkWorkAnsweredTurn).

const workTurnColumns = `id,task_id,session_seq,request,instruction,response,explanation,question,choices,artifacts,requested_at,started_at,ended_at`

func scanWorkTurn(row interface{ Scan(...any) error }) (workTurn, error) {
	var turn workTurn
	var sessionSeq sql.NullInt64
	var request, instruction, response, explanation, question, startedAt, endedAt sql.NullString
	var choices, artifacts string
	err := row.Scan(&turn.ID, &turn.TaskID, &sessionSeq, &request, &instruction, &response,
		&explanation, &question, &choices, &artifacts, &turn.RequestedAt, &startedAt, &endedAt)
	if err != nil {
		return turn, err
	}
	if sessionSeq.Valid {
		seq := sessionSeq.Int64
		turn.SessionSeq = &seq
	}
	turn.Request = nullableString(request)
	turn.Instruction = nullableString(instruction)
	turn.Response = nullableString(response)
	turn.Explanation = nullableString(explanation)
	turn.Question = nullableString(question)
	turn.StartedAt = nullableString(startedAt)
	turn.EndedAt = nullableString(endedAt)
	turn.Choices = []workChoice{}
	if err := json.Unmarshal([]byte(choices), &turn.Choices); err != nil {
		return turn, err
	}
	turn.Artifacts = []string{}
	if err := json.Unmarshal([]byte(artifacts), &turn.Artifacts); err != nil {
		return turn, err
	}
	return turn, nil
}

func loadWorkTurn(ctx context.Context, tx *sql.Tx, taskID string, turnID int64) (workTurn, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+workTurnColumns+" FROM sp_work_turns WHERE id=? AND task_id=?", turnID, taskID)
	turn, err := scanWorkTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Scoped to the task on purpose: a turn id that belongs to another task
		// is "not on this task", not someone else's row to edit.
		return turn, workFail("not_found", "turn not found on this task")
	}
	return turn, err
}

// workTurnTail is how many turns a task has and its last one.
func workTurnTail(ctx context.Context, tx *sql.Tx, taskID string) (int64, *workTurn, error) {
	var total int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_turns WHERE task_id=?", taskID).Scan(&total); err != nil {
		return 0, nil, err
	}
	if total == 0 {
		return 0, nil, nil
	}
	row := tx.QueryRowContext(ctx,
		"SELECT "+workTurnColumns+" FROM sp_work_turns WHERE task_id=? ORDER BY id DESC LIMIT 1", taskID)
	turn, err := scanWorkTurn(row)
	if err != nil {
		return total, nil, err
	}
	return total, &turn, nil
}

func encodeWorkChoices(choices []workChoice) string {
	if choices == nil {
		choices = []workChoice{}
	}
	encoded, err := json.Marshal(choices)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func encodeWorkArtifacts(artifacts []string) string {
	if artifacts == nil {
		artifacts = []string{}
	}
	encoded, err := json.Marshal(artifacts)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// requireWorkTaskVersion is the fence on every turn write: the writer has to
// hold the task's current version, so a parent that has not seen the task move
// cannot write the turn that follows a state it never read.
// requireWorkTaskVersion checks that the writer is acting on the task state it
// read, and then takes the turn lane's compare-and-swap. Both are needed and
// they are different things: expected_version says "the task has not changed
// under me" and does not move (so a turn does not invalidate the choices a
// screen is showing), while expected_turn_version is the actual lock — a single
// conditional UPDATE, so two parents holding the same value cannot both write
// (session_work_guard.go).
func requireWorkTaskVersion(
	ctx context.Context, tx *sql.Tx, taskID string, expected int64, expectedTurn *int64,
) (workTask, int64, error) {
	task, err := loadWorkTask(ctx, tx, taskID)
	if err != nil {
		return task, 0, err
	}
	if task.Version != expected {
		return task, 0, workFail("conflict", "expected_version does not match the task's current version")
	}
	if expectedTurn == nil {
		return task, 0, workFail("invalid_argument", "expected_turn_version is required")
	}
	next, err := swapWorkGuard(ctx, tx, taskID, workGuardTurn, *expectedTurn)
	if err != nil {
		return task, 0, err
	}
	return task, next, nil
}

func appendWorkTurn(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	_, turnVersion, err := requireWorkTaskVersion(ctx, tx, a.TaskID, *a.ExpectedVersion, a.ExpectedTurnVersion)
	if err != nil {
		return nil, err
	}
	turn, err := insertWorkTurn(ctx, tx, a.TaskID, a.Turn, now)
	if err != nil {
		return nil, err
	}
	if err := touchWorkTask(ctx, tx, a.TaskID, now); err != nil {
		return nil, err
	}
	return map[string]any{"turn": turn, "turn_version": turnVersion}, nil
}

func insertWorkTurn(ctx context.Context, tx *sql.Tx, taskID string, input *workTurnInput, now time.Time) (workTurn, error) {
	requestedAt := workNow(now)
	if input.RequestedAt != nil {
		requestedAt = *input.RequestedAt
	}
	choices := []workChoice{}
	if input.rawChoices != nil {
		choices = *input.rawChoices
	}
	artifacts := input.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_turns
		 (task_id,session_seq,request,instruction,response,explanation,question,choices,artifacts,requested_at,started_at,ended_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, input.SessionSeq, input.Request, input.Instruction, input.Response, input.Explanation,
		input.Question, encodeWorkChoices(choices), encodeWorkArtifacts(artifacts),
		requestedAt, input.StartedAt, input.EndedAt)
	if err != nil {
		return workTurn{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return workTurn{}, err
	}
	return loadWorkTurn(ctx, tx, taskID, id)
}

// patchWorkTurn changes only the fields the caller named. An explicit null
// clears one; an absent field is left alone, so filling in a response does not
// erase the question that was asked on the same turn.
func patchWorkTurn(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	_, turnVersion, err := requireWorkTaskVersion(ctx, tx, a.TaskID, *a.ExpectedVersion, a.ExpectedTurnVersion)
	if err != nil {
		return nil, err
	}
	current, err := loadWorkTurn(ctx, tx, a.TaskID, *a.TurnID)
	if err != nil {
		return nil, err
	}
	// Taking a turn is writing its started_at, so writing one over a turn that
	// already has one is a second parent claiming work somebody is already
	// doing. The counter alone cannot catch it: a parent that re-reads *after*
	// the first claim holds a current counter and would be admitted. Clearing
	// it is how a parent hands a turn back, and that stays allowed.
	if a.Turn.named("started_at") && !a.Turn.cleared("started_at") && current.StartedAt != nil {
		return nil, workFail("conflict",
			"this turn was already taken at "+*current.StartedAt+
				"; clear started_at to hand it back before taking it again")
	}
	// Taking the turn mints the claim token that turn_dispatch requires, and
	// handing it back destroys it. The counters cannot do this job: a second
	// parent that re-reads them holds current values, so without a token it
	// could record an outside action for work it does not hold.
	taking := a.Turn.named("started_at") && !a.Turn.cleared("started_at")
	handingBack := a.Turn.named("started_at") && a.Turn.cleared("started_at")
	issued := ""
	if taking {
		issued, err = newClaimToken()
		if err != nil {
			return nil, err
		}
	}
	// A turn that has ended is finished work, and what was said and answered on
	// it is what a person has already been shown. Those four fields are the
	// verbatim record, so they are not rewritten after the fact — reopen the
	// turn by clearing ended_at, or append the next one. Tidying the question,
	// its choices or the bookkeeping fields stays allowed: a stale question
	// that a finished turn is still offering is worth clearing.
	if current.EndedAt != nil && !a.Turn.cleared("ended_at") {
		for _, field := range []string{"request", "instruction", "response", "explanation"} {
			if a.Turn.named(field) {
				return nil, workFail("conflict",
					"this turn ended at "+*current.EndedAt+" and its "+field+
						" is the record a person was shown; clear ended_at to reopen it, or append the next turn")
			}
		}
	}
	assignments := []string{}
	values := []any{}
	set := func(column string, value any) {
		assignments = append(assignments, column+"=?")
		values = append(values, value)
	}
	input := a.Turn
	text := func(column string, value *string) {
		if !input.named(column) {
			return
		}
		if input.cleared(column) {
			set(column, nil)
			return
		}
		set(column, value)
	}
	text("request", input.Request)
	text("instruction", input.Instruction)
	text("response", input.Response)
	text("explanation", input.Explanation)
	text("question", input.Question)
	text("started_at", input.StartedAt)
	text("ended_at", input.EndedAt)
	if input.named("requested_at") && !input.cleared("requested_at") {
		set("requested_at", input.RequestedAt)
	}
	if input.named("session_seq") {
		if input.cleared("session_seq") {
			set("session_seq", nil)
		} else {
			set("session_seq", input.SessionSeq)
		}
	}
	if input.named("choices") {
		if input.cleared("choices") {
			set("choices", "[]")
		} else {
			set("choices", encodeWorkChoices(*input.rawChoices))
		}
	}
	if input.named("artifacts") {
		if input.cleared("artifacts") {
			set("artifacts", "[]")
		} else {
			set("artifacts", encodeWorkArtifacts(input.Artifacts))
		}
	}
	if len(assignments) == 0 {
		return nil, workFail("invalid_argument", "turn must name at least one field to change")
	}
	values = append(values, *a.TurnID, a.TaskID)
	if _, err := tx.ExecContext(ctx,
		"UPDATE sp_work_turns SET "+strings.Join(assignments, ",")+" WHERE id=? AND task_id=?", values...); err != nil {
		return nil, err
	}
	if err := touchWorkTask(ctx, tx, a.TaskID, now); err != nil {
		return nil, err
	}
	if taking {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO sp_work_turn_claim (turn_id,claim_token,claimed_at) VALUES (?,?,?)
			 ON CONFLICT(turn_id) DO UPDATE SET claim_token=excluded.claim_token, claimed_at=excluded.claimed_at`,
			*a.TurnID, issued, workNow(now)); err != nil {
			return nil, err
		}
	}
	if handingBack {
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM sp_work_turn_claim WHERE turn_id=?", *a.TurnID); err != nil {
			return nil, err
		}
	}
	turn, err := loadWorkTurn(ctx, tx, a.TaskID, *a.TurnID)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"turn": turn, "turn_version": turnVersion}
	if issued != "" {
		// Returned once, to the parent that took the turn. It is not in any
		// read view: nothing a person reads carries it.
		result["claim_token"] = issued
	}
	return result, nil
}

// listWorkTurns pages a task's turns oldest first. open=true is the parent's
// work queue: a turn carrying a request that has not ended yet.
func listWorkTurns(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = workListLimitDefault
	}
	after := int64(0)
	if a.AfterTurnID != nil {
		after = *a.AfterTurnID
	}
	openOnly := 0
	if a.Open != nil && *a.Open {
		openOnly = 1
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT "+workTurnColumns+` FROM sp_work_turns
		 WHERE task_id=? AND id>? AND (?=0 OR (request IS NOT NULL AND ended_at IS NULL))
		 ORDER BY id LIMIT ?`, a.TaskID, after, openOnly, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []workTurn{}
	var next int64
	for rows.Next() {
		turn, scanErr := scanWorkTurn(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if len(turns) == limit {
			next = turns[len(turns)-1].ID
			break
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The turn lane's counter travels with the page: a parent that has just
	// read a task's turns needs it to write the next one, and reading it in a
	// second call would be reading it after somebody else moved it.
	turnVersion, _, err := workGuards(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"turns": turns, "turn_version": turnVersion}
	if next != 0 {
		result["next_cursor"] = next
	}
	return result, nil
}

// dispatchWorkTurn records an outside action's key and intent beside the turn,
// before the action. It is deliberately not a turn field: what the parent has
// handed to another system is its own bookkeeping, and the agreed turn carries
// only what a person said, was told, answered and was explained.
//
// A second dispatch under a different key is refused: something may already
// have landed outside this database under the first one, and resending under a
// new key would do it twice. observed=not_accepted is the parent stating it
// established the first never landed.
func dispatchWorkTurn(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	_, turnVersion, err := requireWorkTaskVersion(ctx, tx, a.TaskID, *a.ExpectedVersion, a.ExpectedTurnVersion)
	if err != nil {
		return nil, err
	}
	turn, err := loadWorkTurn(ctx, tx, a.TaskID, *a.TurnID)
	if err != nil {
		return nil, err
	}
	// Recording an intent to act for a turn that has already ended would
	// authorise an action for work that is finished.
	if turn.EndedAt != nil {
		return nil, workFail("conflict",
			"this turn ended at "+*turn.EndedAt+"; it cannot dispatch anything further")
	}
	// Only the parent holding the turn may act for it. A turn nobody has taken
	// has nobody to act for it.
	if turn.StartedAt == nil {
		return nil, workFail("conflict",
			"this turn has not been taken: write started_at with turn_patch before dispatching for it")
	}
	// Holding it means holding its claim token. started_at alone is a
	// timestamp, not an identity — measured, a second caller that re-read the
	// counters could record an outside action for a turn the first parent was
	// working. The token is what the counters cannot provide.
	var heldToken string
	err = tx.QueryRowContext(ctx,
		"SELECT claim_token FROM sp_work_turn_claim WHERE turn_id=?", *a.TurnID).Scan(&heldToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workFail("conflict",
			"this turn has no live claim: take it again with turn_patch(started_at) and use the claim_token it returns")
	}
	if err != nil {
		return nil, err
	}
	if a.ClaimToken == "" || a.ClaimToken != heldToken {
		return nil, workFail("conflict",
			"claim_token does not hold this turn; the parent that took it holds it. "+
				"Do not act outside on its behalf — read the turn again")
	}
	var recordedKey, recordedTarget, recordedState string
	err = tx.QueryRowContext(ctx,
		"SELECT operation_key,operation_target,dispatch_state FROM sp_work_turn_admin WHERE turn_id=?", *a.TurnID).
		Scan(&recordedKey, &recordedTarget, &recordedState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if recordedKey != "" && recordedKey != a.OperationKey && a.Observed != "not_accepted" {
		return nil, workFail("conflict",
			"this turn already recorded operation_key "+recordedKey+"; establish what it did before dispatching another")
	}
	if recordedKey != "" && recordedKey == a.OperationKey {
		// The key is what the outside system deduplicates on, so it has to keep
		// naming the same action. Letting the target move under it would make
		// the record say one thing was done while another was.
		if recordedTarget != a.OperationTarget {
			return nil, workFail("conflict",
				"operation_key "+recordedKey+" already names target "+recordedTarget+
					"; a key cannot be reused for a different target")
		}
		// dispatched means it was handed over. Rewinding to intended would
		// erase that, and a later reader would believe nothing had left.
		if recordedState == dispatchDispatched && a.DispatchState == dispatchIntended {
			return nil, workFail("conflict",
				"this turn already recorded dispatched; it cannot go back to intended")
		}
	}
	stamp := workNow(now)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_turn_admin (turn_id,operation_key,operation_target,dispatch_state,dispatched_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(turn_id) DO UPDATE SET
		   operation_key=excluded.operation_key,operation_target=excluded.operation_target,
		   dispatch_state=excluded.dispatch_state,dispatched_at=excluded.dispatched_at`,
		*a.TurnID, a.OperationKey, a.OperationTarget, a.DispatchState, stamp); err != nil {
		return nil, err
	}
	return map[string]any{
		"turn_id": *a.TurnID, "operation_key": a.OperationKey, "operation_target": a.OperationTarget,
		"dispatch_state": a.DispatchState, "dispatched_at": stamp, "turn_version": turnVersion,
	}, nil
}

// checkWorkAnsweredTurn refuses an answer aimed at a turn the task has moved
// past. A screen opened before the parent asked something else still holds the
// older question and its choices; hiding them on the read side is not enough,
// because that screen can still submit one. The rule is the same one the
// ledger's stale choice used, expressed with the turn instead of a version: the
// turn being answered has to be this task's latest.
func checkWorkAnsweredTurn(ctx context.Context, tx *sql.Tx, taskID string, answered int64) error {
	var latest sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT max(id) FROM sp_work_turns WHERE task_id=?", taskID).Scan(&latest); err != nil {
		return err
	}
	if !latest.Valid || latest.Int64 != answered {
		return workFail("stale_choice", staleChoiceMessage)
	}
	return nil
}
