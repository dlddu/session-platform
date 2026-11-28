// The work records as a reader outside the session gets them, and the one write
// that reader has: a person's request becomes the next turn of a task.
//
// Why a view of its own rather than the records as a tool returns them: the
// three hops from the app to this slot (control plane -> workload agent ->
// sidecar) carry no SQL and no tool call, so what they can show is exactly what
// this file projects. It is the agreed task and turn plus the two facts a reader
// would otherwise need a call per task for — how many turns a task has and what
// its latest turn is — and nothing of the parent's own bookkeeping: a claim
// token, an operation key and a dispatch state are what the parent handed to
// another system, and were never part of what a person reads.
package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	// workRequestReviewStatus is the status a stored request moves a task out
	// of, and workRequestReceiptStatus the one it moves it into. Both are
	// members of workStatuses (session_work.go) — this path invents no status
	// and renames none; it only moves a task between two that already exist.
	workRequestReviewStatus  = "review"
	workRequestReceiptStatus = "planned"
)

// workOpenTurns is what a reader showing "somebody is waiting" asks for:
// waiting is a request nobody has picked up, running is one a parent took and
// has not finished. A task with no turns is the ordinary case, so the counts
// come back zero rather than absent.
type workOpenTurns struct {
	Waiting int `json:"waiting"`
	Running int `json:"running"`
}

func workOpenTurnsOf(ctx context.Context, tx *sql.Tx, taskID string) (workOpenTurns, error) {
	var open workOpenTurns
	// coalesce, because sum() over zero rows is NULL and a NULL into an int
	// fails the scan: without it the page 500s for exactly the tasks a reader is
	// most likely to be looking at.
	err := tx.QueryRowContext(ctx,
		`SELECT
		   coalesce(sum(CASE WHEN request IS NOT NULL AND ended_at IS NULL AND started_at IS NULL THEN 1 ELSE 0 END), 0),
		   coalesce(sum(CASE WHEN request IS NOT NULL AND ended_at IS NULL AND started_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		 FROM sp_work_turns WHERE task_id=?`, taskID).Scan(&open.Waiting, &open.Running)
	return open, err
}

// storeRequestAsTurn is the write side of the read path: a request a person
// left outside the session becomes the next turn of that task. No turn is
// started and nothing is woken, and the parent finds the request the next time
// it lists the task's open turns.
//
// One thing beside the turn does change, and only for a task that was waiting
// on the person: a task **shown as** `review` leaves it for `planned` in this
// same transaction. Shown, not stored — the two differ for a row an older image
// wrote (requestReceivesTheTask), and the eligibility is judged before this
// store adds a turn of its own.
//
// Idempotency is the caller's own id: the same id with the same words answers
// with the turn already appended, and the same id with different words is a
// conflict rather than a second turn. The replay answers from the turn that is
// already there and never reaches the status move below — a retry of an answer
// the app never saw cannot pull a task the parent has since taken back into the
// queue.
//
// choiceTurn, when given, is the turn whose choices the person picked from. The
// rule is the turn records' own — only the latest turn's choices are current —
// so a screen left open while the task moved on is refused rather than acted
// on, and the words the person typed are theirs to keep.
func storeRequestAsTurn(
	ctx context.Context, tx *sql.Tx, id, taskID, text, author string, choiceTurn *int64, now time.Time,
) (workTurnView, bool, error) {
	turnID, mapped, err := turnOfOriginSource(ctx, tx, workTurnOriginAppRequest, id)
	if err != nil {
		return workTurnView{}, false, err
	}
	if mapped {
		turn, loadErr := loadWorkTurn(ctx, tx, taskID, turnID)
		if loadErr != nil {
			return workTurnView{}, false, workFail("conflict", "request id already used on another task")
		}
		if turn.Request == nil || *turn.Request != text {
			return workTurnView{}, false, workFail("conflict", "request id already used with different content")
		}
		view, viewErr := workTurnViewOf(ctx, tx, turn)
		return view, true, viewErr
	}
	task, err := loadWorkTask(ctx, tx, taskID)
	if err != nil {
		var known workError
		if errors.As(err, &known) && known.Code == "not_found" {
			var retired int
			if countErr := tx.QueryRowContext(ctx,
				"SELECT count(*) FROM sp_work_retired WHERE id=?", taskID).Scan(&retired); countErr != nil {
				return workTurnView{}, false, countErr
			}
			if retired != 0 {
				return workTurnView{}, false, workFail("conflict", "that task was deleted")
			}
		}
		return workTurnView{}, false, err
	}
	if choiceTurn != nil {
		if err = checkRequestChoiceTurn(ctx, tx, task, *choiceTurn); err != nil {
			return workTurnView{}, false, err
		}
	}
	// Judged here, before a turn of this store's own exists. Whether this
	// request is the receipt of an answer somebody was asked for is a fact about
	// the task the person was looking at, and the next statement changes that
	// task's latest turn — so asking afterwards would be asking about a state
	// this transaction had already moved.
	receives, err := requestReceivesTheTask(ctx, tx, task)
	if err != nil {
		return workTurnView{}, false, err
	}
	request := text
	turn, err := insertWorkTurn(ctx, tx, taskID, &workTurnInput{Request: &request}, now)
	if err != nil {
		return workTurnView{}, false, err
	}
	// Where the turn came from is its own record: it is what keeps a retried
	// store from becoming a second turn, and what tells a reader this exchange
	// was written through the app rather than by the session. The sentence a
	// person picked travels as the turn's own request — there is no choice id on
	// a turn, because which turn a pick answers is what decides whether it is
	// still current.
	if err = recordTurnOrigin(ctx, tx, turn.ID, workTurnOriginAppRequest, id, author); err != nil {
		return workTurnView{}, false, err
	}
	if err = touchWorkTask(ctx, tx, taskID, now); err != nil {
		return workTurnView{}, false, err
	}
	// The turn set changed, so the turn lane's counter moves even though the
	// writer is a person's request rather than a parent. A parent holding the
	// value it read before this arrived has to read again — otherwise it would
	// write the next turn without having seen this one.
	if _, _, err = workGuards(ctx, tx, taskID); err != nil {
		return workTurnView{}, false, err
	}
	if _, err = tx.ExecContext(ctx,
		"UPDATE sp_work_task_guard SET turn_version=turn_version+1 WHERE task_id=?", taskID); err != nil {
		return workTurnView{}, false, err
	}
	// Written last, so the status moves only once the request is really stored:
	// every check and every insert above has already succeeded, and a failure in
	// any of them rolls back the move with the turn. What is written was decided
	// before the insert; only the write waits.
	if receives {
		if err = receiveRequestOnReviewTask(ctx, tx, task, now); err != nil {
			return workTurnView{}, false, err
		}
	}
	view, err := workTurnViewOf(ctx, tx, turn)
	return view, false, err
}

// receiveRequestOnReviewTask writes the receipt its caller already decided on:
// a task that was waiting on a person leaves review for `planned` — "received,
// nobody has picked it up yet". Whether it applies is requestReceivesTheTask's
// question; this is only the write. The contract is docs/session-ledger.md,
// "The one write, and the receipt it records"; what is here is why it is in
// this transaction and what it leaves alone.
//
// The compare-and-swap is on the **stored** word and the version this
// transaction read, even when what it was shown as came from evidence: on disk
// is the only place another writer can have moved it.
//
// Why here rather than in the app: the request and the status are one fact. A
// second call from the app could store the request and then fail to move the
// task, and an app moving its own copy optimistically would be guessing about a
// row only this slot owns — a late answer to that guess could paint `review`
// back over a task the monitor had already taken. In one transaction neither is
// possible.
//
// It touches nothing else: not the open question and the choices offered beside
// it, not the task's description or completion criteria, not the parent's own
// bookkeeping, and no other status. Somebody sending a sentence settles no
// decision, so the account of what was asked has to survive into the monitor's
// own handling of the answer, and `running` stays the monitor's to write on a
// real hand-off.
//
// It also wakes nothing. Delivering "a request arrived" to the monitor's
// existing lane is a separate concern that consumes this commit, and it has to
// act on the stored turn rather than re-test for `review`: by then the status is
// `planned`, and a second test would drop exactly the request it was told of.
//
// The version moves with the status because a status is a new state of the task
// rather than an event on it — which is why touchWorkTask above leaves the
// version alone for the turn itself. A monitor holding what it read before the
// request landed is told to read again instead of writing over the receipt.
func receiveRequestOnReviewTask(ctx context.Context, tx *sql.Tx, task workTask, now time.Time) error {
	// The status this transaction read is in the WHERE clause beside the
	// version, and the row count is checked rather than assumed — the same
	// single-winner swap a task write is, so a move that matched nothing is
	// reported as a lost race instead of passing for a move.
	result, err := tx.ExecContext(ctx,
		"UPDATE sp_work_tasks SET status=?,version=?,updated_at=? WHERE id=? AND version=? AND status=?",
		workRequestReceiptStatus, task.Version+1, workNow(now), task.ID, task.Version, task.Status)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return workFail("conflict", "this task moved while the request was being stored — read it again and send the request once")
	}
	return nil
}

// requestReceivesTheTask answers whether this store is the receipt of an answer
// somebody was asked for. The question is about the status the person was
// **shown**, not the word on disk: a slot can hold a status this image does not
// write, and deciding on the byte would skip exactly the task somebody is
// looking at as waiting on them.
func requestReceivesTheTask(ctx context.Context, tx *sql.Tx, task workTask) (bool, error) {
	shown, err := workStatusAsShown(ctx, tx, task)
	if err != nil {
		return false, err
	}
	return shown == workRequestReviewStatus, nil
}

// workStatusAsShown is the only seam between the receipt and the reading of a
// status word, and it holds no reading of its own. On this tree every status in
// workStatuses is written by this image and answered as it stands, so the word
// on disk is what is shown. Where an older word is folded, that reading belongs
// to the status domain rather than here; this is where the receipt asks for it,
// so there is one definition and no copy of its rules.
func workStatusAsShown(_ context.Context, _ *sql.Tx, task workTask) (string, error) {
	return task.Status, nil
}

// checkRequestChoiceTurn refuses a pick from a screen the task has moved past.
func checkRequestChoiceTurn(ctx context.Context, tx *sql.Tx, task workTask, choiceTurn int64) error {
	if err := checkWorkAnsweredTurn(ctx, tx, task.ID, choiceTurn); err != nil {
		return err
	}
	turn, err := loadWorkTurn(ctx, tx, task.ID, choiceTurn)
	if err != nil || len(turn.Choices) == 0 {
		return workFail("stale_choice", staleChoiceMessage)
	}
	return nil
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
	// Migrated says a migration created this task, which is what tells a reader
	// the ledger also holds history about it. It is not "this task has
	// bookkeeping": a parent records an approval scope on tasks created here,
	// and answering those as migrated sent a reader to a ledger history that
	// does not exist.
	migrated, err := taskCameFromLedger(ctx, tx, task.ID)
	if err != nil {
		return workTaskView{}, err
	}
	view.Migrated = migrated
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
	origin, found, err := turnOriginOf(ctx, tx, turn.ID)
	if err != nil {
		return workTurnView{}, err
	}
	if !found {
		return view, nil
	}
	// A turn exported for a rollback and then imported again is a turn this
	// session wrote, which is what the empty origin says. Naming the export in
	// the read view would tell a person their exchange came from somewhere else.
	if origin.origin == workTurnOriginExported {
		return view, nil
	}
	view.Origin = origin.origin
	return view, nil
}
