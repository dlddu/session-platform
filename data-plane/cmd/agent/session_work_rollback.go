// Rolling back after the cutover (AC-N9).
//
// "An image that never heard of these tables still opens the database and still
// serves the ledger" is true and is not enough. The ledger it serves is the one
// the migration left behind, so everything written *after* the cutover — a task
// created in the work records, a turn, a person's request that became a turn —
// is in tables that image does not read. Rolling back would leave a person
// looking at work that silently stopped at the cutover, and a parent acting on
// a ledger that has forgotten days of exchanges. Invisible is the same as lost
// to the person reading the screen.
//
// So a rollback is a step, not an accident. work_rollback_export writes the
// work records back into the ledger shape the older image reads, and leaves the
// work tables untouched beside it:
//
//   - every work task becomes (or updates) its sp_ledger_tasks row, carrying
//     the preserved stage, owner, relations and details back into the document
//     it came from;
//   - every turn that holds a person's words becomes an sp_ledger_user_requests
//     row, keyed so a turn that came *from* such a row is not written twice;
//   - the state becomes rolled_back, which refuses work writes (the old image
//     is about to own this database) and lets ledger writes through again.
//
// Going forward again is work_migrate, unchanged: every insert is keyed, so the
// rows already mapped are not duplicated and whatever the old image wrote in
// the meantime is imported. That is the divergence contract — the two records
// are never both writable, and the direction is named in sp_work_meta.state
// rather than guessed.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	workRollbackTool = "work_rollback_export"

	// workStateRolledBack is the fence pointing the other way: the ledger is
	// authoritative again and the work records are the copy.
	workStateRolledBack = "rolled_back"

	workMigrationStepRollback = "rollback_export"
)

// workRollbackCounts is what the export wrote, in rows. Present is what a
// second run found already there, which is how a repeated export shows it
// added nothing twice.
type workRollbackCounts struct {
	Tasks           int `json:"tasks_written"`
	TasksPresent    int `json:"tasks_present"`
	Requests        int `json:"requests_written"`
	RequestsPresent int `json:"requests_present"`
	// TurnsUnexportable is the turns the ledger shape has nowhere to put: a
	// turn with no request is an exchange the ledger never modelled. The count
	// is exact and the reason is named, because a silent drop here is the thing
	// this whole file exists to prevent.
	TurnsUnexportable int `json:"turns_unexportable"`
}

type workRollbackPlan struct {
	Counts       workRollbackCounts `json:"counts"`
	Unexportable []workUnmapped     `json:"unexportable"`
}

func workRollbackToolDefinition() map[string]any {
	return map[string]any{
		"name":        workRollbackTool,
		"description": "Prepares a rollback to an image that reads only the ledger: writes every work task and every turn holding a person's words back into the ledger tables that image reads, so work done after the cutover is visible there instead of silently stopping at the cutover. dry_run=true reports exactly what it would write and changes nothing. A real run snapshots the database first, then commits in one transaction and leaves the state rolled_back — work writes are then refused and ledger writes are allowed, because the older image is about to own this database. Going forward again is work_migrate, which is keyed and so imports whatever the old image wrote without duplicating what is already mapped. The work tables are not deleted: they stay as the copy.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"request_id": map[string]any{"type": "string"},
			"dry_run":    map[string]any{"type": "boolean"},
		}, "required": []string{"request_id"}, "additionalProperties": false},
	}
}

// runWorkRollback manages its own transactions for the migration's reasons: the
// snapshot cannot be taken inside one.
func (s *sessionDB) runWorkRollback(ctx context.Context, a workArgs, now time.Time) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.workMigrationState(ctx, db)
	if err != nil {
		return nil, err
	}
	switch state {
	case workStateMigrating:
		return nil, ledgerFail("conflict", "the work migration holds this database; retry when it has finished")
	case workStateReady:
		return nil, ledgerFail("conflict",
			"this slot has not migrated, so there is nothing to export — the ledger is already what an older image reads")
	}
	dryRun := a.DryRun != nil && *a.DryRun
	if dryRun {
		var plan workRollbackPlan
		if err = s.workTx(ctx, db, func(tx *sql.Tx) error {
			var planErr error
			plan, planErr = exportWorkIntoLedger(ctx, tx, nil, now)
			return planErr
		}); err != nil {
			return nil, err
		}
		return map[string]any{"dry_run": true, "state": state, "plan": plan}, nil
	}
	snapshot, err := s.snapshotSessionDB(ctx, db, "pre-rollback", now)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		replay, fingerprint, receiptErr := workReceipt(ctx, tx, workRollbackTool, a)
		if receiptErr != nil {
			return receiptErr
		}
		if replay != nil {
			var decoded map[string]any
			if decodeErr := json.Unmarshal(replay, &decoded); decodeErr != nil {
				return decodeErr
			}
			result = decoded
			return nil
		}
		plan, exportErr := exportWorkIntoLedger(ctx, tx, tx, now)
		if exportErr != nil {
			return exportErr
		}
		if recordErr := recordWorkMigrationStep(
			ctx, tx, workMigrationStepRollback, workStateRolledBack, plan, now); recordErr != nil {
			return recordErr
		}
		if stateErr := setWorkState(ctx, tx, workStateRolledBack); stateErr != nil {
			return stateErr
		}
		result = map[string]any{
			"state": workStateRolledBack, "snapshot": snapshot, "plan": plan,
			"next": "roll the image back now; the ledger is authoritative while the state is rolled_back. " +
				"Coming forward again is work_migrate, which imports whatever the older image wrote without " +
				"duplicating what is already mapped.",
		}
		return storeWorkReceipt(ctx, tx, a.RequestID, fingerprint, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// exportWorkIntoLedger writes the work records into the ledger shape. write is
// nil for a rehearsal, which counts exactly what a real run would write and
// touches nothing.
func exportWorkIntoLedger(ctx context.Context, tx *sql.Tx, write *sql.Tx, now time.Time) (workRollbackPlan, error) {
	plan := workRollbackPlan{Unexportable: []workUnmapped{}}
	if err := migrateLedger(ctx, tx); err != nil {
		return plan, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+workTaskColumns+" FROM sp_work_tasks ORDER BY id")
	if err != nil {
		return plan, err
	}
	tasks := []workTask{}
	for rows.Next() {
		task, scanErr := scanWorkTask(rows)
		if scanErr != nil {
			rows.Close()
			return plan, scanErr
		}
		tasks = append(tasks, task)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return plan, err
	}
	// A ledger user request's seq is unique across the whole table, and the
	// migrated rows already hold the low numbers. An exported turn therefore
	// gets the next free seq rather than its turn id, which would collide with
	// the request it was migrated from. Turns are walked in id order, so the
	// arrival order a seq is supposed to express survives.
	var nextSeq int64
	if err = tx.QueryRowContext(ctx,
		"SELECT coalesce(max(seq),0)+1 FROM sp_ledger_user_requests").Scan(&nextSeq); err != nil {
		return plan, err
	}
	for _, task := range tasks {
		written, taskErr := exportOneTask(ctx, tx, write, task)
		if taskErr != nil {
			return plan, taskErr
		}
		if written {
			plan.Counts.Tasks++
		} else {
			plan.Counts.TasksPresent++
		}
		if err = exportTaskTurns(ctx, tx, write, task, &plan, &nextSeq, now); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// exportOneTask puts one work task back in the ledger, carrying its preserved
// bookkeeping into the document. The ledger's version is the work task's, so an
// older image's own CAS keeps working from where this left off.
func exportOneTask(ctx context.Context, tx *sql.Tx, write *sql.Tx, task workTask) (bool, error) {
	record := ledgerRecord{ID: task.ID, Version: task.Version, Task: ledgerTask{
		Title: task.Title, Status: task.Status,
		Sessions: []ledgerSession{}, Details: map[string]any{},
	}}
	if err := fillLegacyFacets(ctx, tx, &record); err != nil {
		return false, err
	}
	// The agreed fields the ledger has no column for travel in the document's
	// details rather than being dropped: an older image does not read them, but
	// coming forward again must not be the moment a description disappears.
	if task.Description != nil || task.CompletionCriteria != nil || task.SessionID != nil {
		carried := map[string]any{}
		if task.Description != nil {
			carried["description"] = *task.Description
		}
		if task.CompletionCriteria != nil {
			carried["completion_criteria"] = *task.CompletionCriteria
		}
		if task.SessionID != nil {
			carried["session_id"] = *task.SessionID
		}
		record.Task.Details["work_records"] = carried
	}
	document, err := json.Marshal(record.Task)
	if err != nil {
		return false, err
	}
	var existing int
	if err = tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_ledger_tasks WHERE id=?", task.ID).Scan(&existing); err != nil {
		return false, err
	}
	if write == nil {
		return existing == 0, nil
	}
	if _, err = write.ExecContext(ctx,
		`INSERT INTO sp_ledger_tasks (id,version,status,stage,owner,document) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   version=excluded.version, status=excluded.status, stage=excluded.stage,
		   owner=excluded.owner, document=excluded.document`,
		task.ID, task.Version, record.Task.Status, record.Task.Stage, record.Task.Owner,
		string(document)); err != nil {
		return false, err
	}
	return existing == 0, nil
}

// exportTaskTurns writes the turns that hold a person's words back as ledger
// user requests. A turn that came from such a row already has one and is not
// written twice; a turn with no request has nothing the ledger shape can hold,
// and is counted and named rather than dropped quietly.
func exportTaskTurns(
	ctx context.Context, tx *sql.Tx, write *sql.Tx, task workTask, plan *workRollbackPlan,
	nextSeq *int64, now time.Time,
) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+workTurnColumns+" FROM sp_work_turns WHERE task_id=? ORDER BY id", task.ID)
	if err != nil {
		return err
	}
	turns := []workTurn{}
	for rows.Next() {
		turn, scanErr := scanWorkTurn(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		turns = append(turns, turn)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, turn := range turns {
		source, sourceID, lookupErr := turnLegacySource(ctx, tx, turn.ID)
		if lookupErr != nil {
			return lookupErr
		}
		if source == "user_request" {
			// It came from the ledger and its row is still there.
			plan.Counts.RequestsPresent++
			continue
		}
		if turn.Request == nil {
			plan.Counts.TurnsUnexportable++
			if len(plan.Unexportable) < workUnmappedReported {
				plan.Unexportable = append(plan.Unexportable, workUnmapped{
					RequestID: fmt.Sprintf("turn-%d", turn.ID), TaskID: task.ID,
					Reason: "the turn holds no request, and a ledger user request is a person's words — " +
						"its instruction, answer and explanation stay in the work records",
				})
			}
			continue
		}
		// A turn a person wrote through the request route keeps the id it was
		// stored under, so an older image sees the same request it would have.
		requestID := sourceID
		if requestID == "" {
			requestID = fmt.Sprintf("turn-%d", turn.ID)
		}
		var present int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_ledger_user_requests WHERE id=?", requestID).Scan(&present); err != nil {
			return err
		}
		if present != 0 {
			plan.Counts.RequestsPresent++
			continue
		}
		plan.Counts.Requests++
		seq := *nextSeq
		*nextSeq++
		if write == nil {
			continue
		}
		status := requestPending
		settledAt := ""
		result := ""
		if turn.EndedAt != nil {
			status = requestProcessed
			settledAt = *turn.EndedAt
			if turn.Explanation != nil {
				// The parent's sentence for the person is what a ledger result
				// was; the session's own answer is not, and stays behind.
				result = *turn.Explanation
			}
		}
		author := turnLegacyAuthor(ctx, tx, turn.ID)
		if _, err = write.ExecContext(ctx,
			`INSERT INTO sp_ledger_user_requests
			   (id,task_id,seq,version,text,author,created_at,status,
			    claim_owner,claim_token,claim_expires_at,
			    operation_key,operation_target,dispatch_state,dispatched_at,
			    related_seq,result,settled_at)
			 VALUES (?,?,?,1,?,?,?,?,'','','','','','','',NULL,?,?)`,
			requestID, task.ID, seq, *turn.Request, author, turn.RequestedAt, status,
			result, settledAt); err != nil {
			return err
		}
		if source == "" {
			// A turn the session wrote itself had no legacy row. Record the id
			// it was exported under, so coming forward again recognises the
			// ledger row as this turn rather than importing it as a new one.
			provenance, marshalErr := json.Marshal(map[string]any{
				"turn": "exported into the ledger for a rollback; the ledger row is this turn, not a new one",
			})
			if marshalErr != nil {
				return marshalErr
			}
			if _, err = write.ExecContext(ctx,
				`INSERT INTO sp_work_turn_legacy (turn_id,source,source_id,source_seq,claim_state,admin,provenance)
				 VALUES (?,'exported_turn',?,?,'','{}',?)
				 ON CONFLICT(turn_id) DO NOTHING`,
				turn.ID, requestID, seq, string(provenance)); err != nil {
				return err
			}
		}
	}
	_ = now
	return nil
}

func turnLegacySource(ctx context.Context, tx *sql.Tx, turnID int64) (string, string, error) {
	var source, sourceID string
	err := tx.QueryRowContext(ctx,
		"SELECT source,source_id FROM sp_work_turn_legacy WHERE turn_id=?", turnID).Scan(&source, &sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return source, sourceID, nil
}

// turnLegacyAuthor recovers who a stored request said it was from. The author
// is bookkeeping the ledger carried, so losing it on the way back would change
// what an older image shows beside a person's words.
func turnLegacyAuthor(ctx context.Context, tx *sql.Tx, turnID int64) string {
	var admin string
	if err := tx.QueryRowContext(ctx,
		"SELECT admin FROM sp_work_turn_legacy WHERE turn_id=?", turnID).Scan(&admin); err != nil {
		return ""
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(admin), &decoded); err != nil {
		return ""
	}
	if author, ok := decoded["author"].(string); ok {
		return author
	}
	return ""
}
