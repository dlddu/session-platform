// What keeps an app that has not been updated working after the cutover.
//
// The three hops from the app to this slot (control plane -> workload agent ->
// sidecar) speak the ledger's shapes: a page of ledger tasks to read, and a
// user request to store. Once a slot's records have moved, those shapes have no
// rows behind them any more — so this file answers them from the work records
// instead: the read view projects work tasks into the ledger's page shape, and
// a stored request becomes a turn.
//
// Deploy order follows from that: this image can be rolled out before the app
// changes, and an app that reads the new views can be rolled out before or
// after. The one order that does not work is an app expecting the work views
// against an older image, which answers 404 — the app reads that as "no route",
// not as "no work".
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// tasksView answers the read-only task view from whichever records this slot
// keeps. Before the cutover that is the ledger; after it, the work tasks
// projected into the same shape — so the caller cannot tell from the answer
// whether this slot has migrated, which is what lets the two roll out
// separately.
func (s *sessionDB) tasksView(ctx context.Context, limit int, afterID string) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	state := ""
	if err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		if ledgerErr := migrateLedger(ctx, tx); ledgerErr != nil {
			return ledgerErr
		}
		var readErr error
		state, readErr = workState(ctx, tx)
		return readErr
	}); err != nil {
		return nil, err
	}
	if state != workStateMigrated {
		return s.ledger(ctx, "ledger_list", ledgerArgs{Limit: limit, AfterID: afterID})
	}
	var page map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		var projectErr error
		page, projectErr = workTasksAsLedgerPage(ctx, tx, limit, afterID)
		return projectErr
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// compatChoiceID names one of a turn's choices for a caller that can only send
// a choice id back. It is derived rather than stored, so a turn that is no
// longer the latest cannot have its choices picked: the id itself says which
// turn it came from.
func compatChoiceID(turnID int64, position int) string {
	return fmt.Sprintf("t%dp%d", turnID, position)
}

func parseCompatChoiceID(raw string) (int64, int, error) {
	if !strings.HasPrefix(raw, "t") {
		return 0, 0, errors.New("not a projected choice id")
	}
	parts := strings.SplitN(strings.TrimPrefix(raw, "t"), "p", 2)
	if len(parts) != 2 {
		return 0, 0, errors.New("not a projected choice id")
	}
	turnID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	position, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return turnID, position, nil
}

// workTasksAsLedgerPage projects one keyset page of work tasks into the shape
// the ledger's read-only view serves. The parent's own bookkeeping that a
// migration preserved is put back where a reader of that shape expects it —
// stage, owner and the session relations with their notes — so a panel that
// shows why a task is stuck keeps showing it.
func workTasksAsLedgerPage(ctx context.Context, tx *sql.Tx, limit int, afterID string) (map[string]any, error) {
	if limit == 0 {
		limit = workListLimitDefault
	}
	listed, err := listWorkTasks(ctx, tx, workArgs{Limit: limit, AfterID: afterID})
	if err != nil {
		return nil, err
	}
	page := listed.(map[string]any)
	tasks := page["tasks"].([]workTask)
	records := []ledgerRecord{}
	for _, task := range tasks {
		record := ledgerRecord{ID: task.ID, Version: task.Version, Task: ledgerTask{
			Title:    task.Title,
			Status:   task.Status,
			Sessions: []ledgerSession{},
			Details:  map[string]any{},
		}}
		if err := fillLegacyFacets(ctx, tx, &record); err != nil {
			return nil, err
		}
		summary, choices, err := workTaskFacets(ctx, tx, task)
		if err != nil {
			return nil, err
		}
		if summary != nil {
			record.Requests = summary
		}
		if len(choices) > 0 {
			record.Choices = choices
		}
		records = append(records, record)
	}
	return map[string]any{"tasks": records, "next_cursor": page["next_cursor"]}, nil
}

// fillLegacyFacets puts a migrated task's preserved stage, owner, relations and
// details back into the projected record. A task created after the cutover has
// none of them, and the record simply carries none.
func fillLegacyFacets(ctx context.Context, tx *sql.Tx, record *ledgerRecord) error {
	var stage, owner, sessions, details string
	err := tx.QueryRowContext(ctx,
		"SELECT stage,owner,sessions,details FROM sp_work_task_legacy WHERE task_id=?", record.ID).
		Scan(&stage, &owner, &sessions, &details)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	record.Task.Stage = stage
	record.Task.Owner = owner
	if sessions != "" {
		var relations []ledgerSession
		if err := json.Unmarshal([]byte(sessions), &relations); err == nil && relations != nil {
			record.Task.Sessions = relations
		}
	}
	if details != "" {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(details), &decoded); err == nil && decoded != nil {
			record.Task.Details = decoded
		}
	}
	return nil
}

// workTaskFacets is what a reader of the projected page learns about waiting
// work: how many turns still need the parent, and the choices the latest turn
// offers. A turn that has a request and has not started is waiting to be picked
// up; one that started and has not ended is being worked on.
func workTaskFacets(ctx context.Context, tx *sql.Tx, task workTask) (*ledgerRequestSummary, []ledgerChoice, error) {
	// coalesce, because a task with no turns is the ordinary case — one created
	// after the cutover has none yet, and a migrated one has none unless a
	// person had already written a request on it. sum() over zero rows is NULL,
	// and a NULL into an int fails the scan: without this the projected page
	// 500s for exactly the tasks a reader is most likely to be looking at.
	var waiting, running int
	var lastSeq sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT
		   coalesce(sum(CASE WHEN request IS NOT NULL AND ended_at IS NULL AND started_at IS NULL THEN 1 ELSE 0 END), 0),
		   coalesce(sum(CASE WHEN request IS NOT NULL AND ended_at IS NULL AND started_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		   max(id)
		 FROM sp_work_turns WHERE task_id=?`, task.ID).Scan(&waiting, &running, &lastSeq)
	if err != nil {
		return nil, nil, err
	}
	_, latest, err := workTurnTail(ctx, tx, task.ID)
	if err != nil {
		return nil, nil, err
	}
	choices := []ledgerChoice{}
	if latest != nil {
		for position, choice := range latest.Choices {
			choices = append(choices, ledgerChoice{
				ID: compatChoiceID(latest.ID, position), Label: choice.Label,
				RequestText: choice.RequestText, BasisVersion: task.Version,
				CreatedAt: latest.RequestedAt,
			})
		}
	}
	if waiting == 0 && running == 0 {
		return nil, choices, nil
	}
	return &ledgerRequestSummary{Pending: waiting, Claimed: running, LastSeq: lastSeq.Int64}, choices, nil
}

// storeRequestAsTurn is the cutover's write side: a request stored by an app
// that still speaks the ledger becomes the next turn of that task. Nothing else
// changes — no turn is started, nothing is woken, and the parent finds it the
// next time it lists the task's open turns.
//
// Idempotency is the caller's own id, as it was before: the same id with the
// same words answers with the turn already appended, and the same id with
// different words is a conflict rather than a second turn.
func storeRequestAsTurn(ctx context.Context, tx *sql.Tx, id, taskID, text, author, choiceID string, choiceBasis int64, now time.Time) (ledgerRequest, bool, error) {
	var turnID int64
	var recorded string
	err := tx.QueryRowContext(ctx,
		"SELECT turn_id,admin FROM sp_work_turn_legacy WHERE source='app_request' AND source_id=?", id).
		Scan(&turnID, &recorded)
	if err == nil {
		turn, loadErr := loadWorkTurn(ctx, tx, taskID, turnID)
		if loadErr != nil {
			return ledgerRequest{}, false, ledgerFail("conflict", "request id already used on another task")
		}
		if turn.Request == nil || *turn.Request != text {
			return ledgerRequest{}, false, ledgerFail("conflict", "request id already used with different content")
		}
		return requestOfTurn(id, author, choiceID, turn), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ledgerRequest{}, false, err
	}
	task, err := loadWorkTask(ctx, tx, taskID)
	if err != nil {
		var known ledgerError
		if errors.As(err, &known) && known.Code == "not_found" {
			var retired int
			if countErr := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_retired WHERE id=?", taskID).Scan(&retired); countErr != nil {
				return ledgerRequest{}, false, countErr
			}
			if retired != 0 {
				return ledgerRequest{}, false, ledgerFail("conflict", "that task was deleted")
			}
		}
		return ledgerRequest{}, false, err
	}
	if choiceID != "" {
		if err = checkCompatChoice(ctx, tx, task, choiceID, choiceBasis); err != nil {
			return ledgerRequest{}, false, err
		}
	}
	request := text
	turn, err := insertWorkTurn(ctx, tx, taskID, &workTurnInput{Request: &request}, now)
	if err != nil {
		return ledgerRequest{}, false, err
	}
	admin, err := json.Marshal(map[string]any{"author": author, "choice_id": choiceID})
	if err != nil {
		return ledgerRequest{}, false, err
	}
	provenance, err := json.Marshal(map[string]any{
		"request": "the person's own words, verbatim, stored through the ledger request route",
	})
	if err != nil {
		return ledgerRequest{}, false, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_turn_legacy (turn_id,source,source_id,source_seq,claim_state,admin,provenance)
		 VALUES (?,'app_request',?,NULL,'',?,?)`,
		turn.ID, id, string(admin), string(provenance)); err != nil {
		return ledgerRequest{}, false, err
	}
	if err = touchWorkTask(ctx, tx, taskID, now); err != nil {
		return ledgerRequest{}, false, err
	}
	return requestOfTurn(id, author, choiceID, turn), false, nil
}

// checkCompatChoice refuses a pick from a screen the task has moved past. The
// projected id carries the turn it came from, so the rule is the one the turn
// records keep: only the latest turn's choices are current, and the position
// has to exist on it.
func checkCompatChoice(ctx context.Context, tx *sql.Tx, task workTask, choiceID string, choiceBasis int64) error {
	turnID, position, err := parseCompatChoiceID(choiceID)
	if err != nil {
		return ledgerFail("stale_choice", staleChoiceMessage)
	}
	if choiceBasis != 0 && choiceBasis != task.Version {
		return ledgerFail("stale_choice", staleChoiceMessage)
	}
	if err = checkWorkAnsweredTurn(ctx, tx, task.ID, turnID); err != nil {
		return err
	}
	turn, err := loadWorkTurn(ctx, tx, task.ID, turnID)
	if err != nil {
		return ledgerFail("stale_choice", staleChoiceMessage)
	}
	if position < 0 || position >= len(turn.Choices) {
		return ledgerFail("stale_choice", staleChoiceMessage)
	}
	return nil
}

// requestOfTurn answers a ledger-shaped caller about the turn its request
// became. seq is the turn's id: both are "where this sits in the order things
// happened", and the caller only uses it to tell one request from another.
func requestOfTurn(id, author, choiceID string, turn workTurn) ledgerRequest {
	text := ""
	if turn.Request != nil {
		text = *turn.Request
	}
	status := requestPending
	if turn.EndedAt != nil {
		status = requestProcessed
	} else if turn.StartedAt != nil {
		status = requestClaimed
	}
	return ledgerRequest{
		ID: id, TaskID: turn.TaskID, Seq: turn.ID, Version: 1, Text: text, Author: author,
		CreatedAt: turn.RequestedAt, Status: status, ChoiceID: choiceID,
	}
}

// workTaskView is one work task as a reader outside the session gets it: the
// agreed columns, and the two facts a reader would otherwise need a call per
// task for. Migrated says this task came from the ledger, which is how a reader
// knows the ledger still holds history and parent bookkeeping about it — a task
// created after the cutover has neither and is not shown as if it did.
type workTaskView struct {
	Task       workTask      `json:"task"`
	TurnsTotal int64         `json:"turns_total"`
	LatestTurn *workTurnView `json:"latest_turn,omitempty"`
	Migrated   bool          `json:"migrated,omitempty"`
}

// workTurnView is one turn with where it came from. Origin is empty for a turn
// the session wrote itself, "user_request" for one migrated from a ledger
// request, and "app_request" for one a person wrote through the request route
// after the cutover. It carries no claim token, operation key or dispatch
// state: what the parent handed to another system is its own bookkeeping and
// has never been part of what a person reads.
type workTurnView struct {
	workTurn
	Origin string `json:"origin,omitempty"`
}

// workTasksView answers one page of this slot's work tasks. It refuses rather
// than answers while the migration holds the database: a page read mid-copy
// could show a task whose turns have not arrived yet.
func (s *sessionDB) workTasksView(ctx context.Context, a workArgs) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		if fenceErr := requireWorkReadable(ctx, tx); fenceErr != nil {
			return fenceErr
		}
		listed, listErr := listWorkTasks(ctx, tx, a)
		if listErr != nil {
			return listErr
		}
		decoded := listed.(map[string]any)
		tasks := decoded["tasks"].([]workTask)
		views := make([]workTaskView, 0, len(tasks))
		for _, task := range tasks {
			view, viewErr := workTaskViewOf(ctx, tx, task)
			if viewErr != nil {
				return viewErr
			}
			views = append(views, view)
		}
		page = map[string]any{"tasks": views, "next_cursor": decoded["next_cursor"]}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func workTaskViewOf(ctx context.Context, tx *sql.Tx, task workTask) (workTaskView, error) {
	total, latest, err := workTurnTail(ctx, tx, task.ID)
	if err != nil {
		return workTaskView{}, err
	}
	view := workTaskView{Task: task, TurnsTotal: total}
	if latest != nil {
		turnView, turnErr := workTurnViewOf(ctx, tx, *latest)
		if turnErr != nil {
			return workTaskView{}, turnErr
		}
		view.LatestTurn = &turnView
	}
	var migrated int
	if err = tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_task_legacy WHERE task_id=?", task.ID).Scan(&migrated); err != nil {
		return workTaskView{}, err
	}
	view.Migrated = migrated != 0
	return view, nil
}

// workTurnsView answers one page of one task's turns. The task has to exist:
// an empty page for a task this slot does not have reads as "no turns yet",
// which is a different thing and would hide a caller's wrong id.
func (s *sessionDB) workTurnsView(ctx context.Context, a workArgs) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		if fenceErr := requireWorkReadable(ctx, tx); fenceErr != nil {
			return fenceErr
		}
		task, taskErr := loadWorkTask(ctx, tx, a.TaskID)
		if taskErr != nil {
			return taskErr
		}
		listed, listErr := listWorkTurns(ctx, tx, a)
		if listErr != nil {
			return listErr
		}
		decoded := listed.(map[string]any)
		turns := decoded["turns"].([]workTurn)
		views := make([]workTurnView, 0, len(turns))
		for _, turn := range turns {
			view, viewErr := workTurnViewOf(ctx, tx, turn)
			if viewErr != nil {
				return viewErr
			}
			views = append(views, view)
		}
		// The task travels with its turns: a reader showing one task needs its
		// description and completion criteria beside the exchanges, and a second
		// call for them could land on a different version than these turns were
		// read against.
		page = map[string]any{
			"task_id": task.ID, "task_version": task.Version, "task": task, "turns": views,
		}
		if next, ok := decoded["next_cursor"]; ok {
			page["next_cursor"] = next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func workTurnViewOf(ctx context.Context, tx *sql.Tx, turn workTurn) (workTurnView, error) {
	view := workTurnView{workTurn: turn}
	var source string
	err := tx.QueryRowContext(ctx,
		"SELECT source FROM sp_work_turn_legacy WHERE turn_id=?", turn.ID).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return workTurnView{}, err
	}
	view.Origin = source
	return view, nil
}

// requireWorkReadable refuses a read while the migration holds the database.
// The copy is one transaction, so a reader would either block on it or — on a
// resumed run that has not reached this task yet — see a task without the turns
// that belong to it.
func requireWorkReadable(ctx context.Context, tx *sql.Tx) error {
	state, err := workState(ctx, tx)
	if err != nil {
		return err
	}
	if state == workStateMigrating {
		return ledgerFail("conflict", "the work migration holds this database; retry when it has finished")
	}
	return nil
}
