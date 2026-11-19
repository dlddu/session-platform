// Moving what the ledger holds into the work records, once.
//
// The rules the migration keeps, because the data a person already saw is the
// thing most easily lost here:
//
//   - Nothing is deleted or flattened. The ledger tables stay exactly as they
//     are, and every part of a row that has no column in the agreed task/turn
//     shape — stage, owner, the session relations with their cursors and seqs,
//     the details JSON with approval and preservation state, a request's claim
//     and dispatch — is preserved verbatim in sp_work_task_legacy /
//     sp_work_turn_legacy. Reading the work records never requires them; the
//     parent still acts on them.
//   - A monitor row is the parent's own bookkeeping, not a person's work, so it
//     is preserved and does not become a task. Presenting one as a task would
//     invent work the person never asked for.
//   - Only what the ledger really recorded is mapped. The ledger never kept the
//     instruction that went to a session or that session's final answer
//     verbatim, so instruction and response stay null. A request's `result` is
//     the sentence the parent wrote for the person, so it becomes the turn's
//     explanation — never its response, which would dress a parent's summary up
//     as the session's own words. A time or a seq the ledger does not have
//     stays null rather than being guessed.
//   - Past management events are not turns. Only a stored user request — a
//     person's own words, with the time they arrived — becomes one.
//   - Status, approval and completion do not move because the data did.
//
// Restarting is safe. A run takes a consistent snapshot (VACUUM INTO) and
// records its checksum, then raises a fence that refuses ledger and work
// writes, then copies in one transaction. An interrupted copy rolls back and
// leaves the fence up, so the next run resumes; every insert is keyed, so a
// second run adds nothing. Rolling the image back leaves these tables unread
// but intact, and the snapshot is what a full restore starts from.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	workMigrationStepSnapshot = "snapshot"
	workMigrationStepFence    = "fence"
	workMigrationStepCopy     = "copy"

	// workUnmappedReported bounds how many unmapped rows the report names one by
	// one. The count is always exact; the list is a sample a person can act on.
	workUnmappedReported = 20
)

// workMigrationCounts is what the run did, in rows. tasks_present and
// turns_present are what a second run found already there, which is how a
// resumed run shows it added nothing twice.
type workMigrationCounts struct {
	LegacyTasks      int `json:"legacy_tasks"`
	MonitorPreserved int `json:"monitor_preserved"`
	TasksCreated     int `json:"tasks_created"`
	TasksPresent     int `json:"tasks_present"`
	// TasksReconciled is what a return from a rollback refreshed from the
	// ledger: rows that were already here and had moved while the older image
	// owned the database.
	TasksReconciled   int `json:"tasks_reconciled"`
	LegacyRequests    int `json:"legacy_requests"`
	TurnsCreated      int `json:"turns_created"`
	TurnsPresent      int `json:"turns_present"`
	RequestsUnmapped  int `json:"requests_unmapped"`
	ChoicesAttached   int `json:"choices_attached"`
	ChoicesUnattached int `json:"choices_unattached"`
}

type workUnmapped struct {
	RequestID string `json:"request_id"`
	TaskID    string `json:"task_id"`
	Reason    string `json:"reason"`
}

type workMigrationPlan struct {
	Counts   workMigrationCounts `json:"counts"`
	Unmapped []workUnmapped      `json:"unmapped"`
	Checks   map[string]any      `json:"checks"`
}

// runWorkMigration is the only work tool that manages its own transactions: the
// snapshot cannot run inside one, and the fence has to be committed before the
// copy so that an interrupted copy is still fenced when the next run arrives.
func (s *sessionDB) runWorkMigration(ctx context.Context, a workArgs, now time.Time) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.workMigrationState(ctx, db)
	if err != nil {
		return nil, err
	}
	if state == workStateMigrated {
		report, reportErr := s.workReportOf(ctx, db)
		if reportErr != nil {
			return nil, reportErr
		}
		report["already_migrated"] = true
		return report, nil
	}
	dryRun := a.DryRun != nil && *a.DryRun
	if dryRun {
		plan, planErr := s.workMigrationPlan(ctx, db, state == workStateRolledBack)
		if planErr != nil {
			return nil, planErr
		}
		return map[string]any{"dry_run": true, "state": state, "plan": plan}, nil
	}
	snapshot := map[string]any{}
	// rolled_back is the other direction's starting point: the ledger has been
	// authoritative and may have moved, so this run needs its own snapshot
	// rather than the one the first migration took.
	if state == workStateReady || state == workStateRolledBack {
		snapshot, err = s.snapshotSessionDB(ctx, db, "pre-work", now)
		if err != nil {
			return nil, err
		}
		if err = s.workTx(ctx, db, func(tx *sql.Tx) error {
			if recordErr := recordWorkMigrationStep(ctx, tx, workMigrationStepSnapshot, "done", snapshot, now); recordErr != nil {
				return recordErr
			}
			if recordErr := recordWorkMigrationStep(ctx, tx, workMigrationStepFence, workStateMigrating, map[string]any{}, now); recordErr != nil {
				return recordErr
			}
			return setWorkState(ctx, tx, workStateMigrating)
		}); err != nil {
			return nil, err
		}
	} else {
		// A fence already up means an earlier run was interrupted after its
		// snapshot. Reuse that snapshot's record rather than taking a second one:
		// the first is the state the database was in before any copy.
		snapshot, err = s.workRecordedSnapshot(ctx, db)
		if err != nil {
			return nil, err
		}
	}
	reconcile := state == workStateRolledBack
	var result map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		replay, fingerprint, receiptErr := workReceipt(ctx, tx, "work_migrate", a)
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
		plan, copyErr := copyLedgerIntoWork(ctx, tx, reconcile, now)
		if copyErr != nil {
			return copyErr
		}
		if recordErr := recordWorkMigrationStep(ctx, tx, workMigrationStepCopy, "done", plan, now); recordErr != nil {
			return recordErr
		}
		if stateErr := setWorkState(ctx, tx, workStateMigrated); stateErr != nil {
			return stateErr
		}
		result = map[string]any{
			"state": workStateMigrated, "snapshot": snapshot, "plan": plan,
			"restore_hint": "restoring the snapshot is an offline operation: no sidecar may have the database open, " +
				"copy the snapshot over it and re-check integrity and row counts",
		}
		return storeWorkReceipt(ctx, tx, a.RequestID, fingerprint, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *sessionDB) workTx(ctx context.Context, db *sql.DB, body func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrateWork(ctx, tx); err != nil {
		return err
	}
	if err := body(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sessionDB) workMigrationState(ctx context.Context, db *sql.DB) (string, error) {
	state := ""
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var readErr error
		state, readErr = workState(ctx, tx)
		return readErr
	})
	return state, err
}

func (s *sessionDB) workMigrationPlan(ctx context.Context, db *sql.DB, reconcile bool) (workMigrationPlan, error) {
	var plan workMigrationPlan
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var planErr error
		plan, planErr = planLedgerIntoWork(ctx, tx, reconcile)
		return planErr
	})
	return plan, err
}

func (s *sessionDB) workRecordedSnapshot(ctx context.Context, db *sql.DB) (map[string]any, error) {
	snapshot := map[string]any{}
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var detail string
		readErr := tx.QueryRowContext(ctx,
			"SELECT detail FROM sp_work_migration WHERE step=? ORDER BY seq DESC LIMIT 1",
			workMigrationStepSnapshot).Scan(&detail)
		if errors.Is(readErr, sql.ErrNoRows) {
			// The fence is up with no snapshot recorded, which this code cannot
			// produce. Refusing is the only safe answer: a copy without a
			// recorded starting point has nothing to roll back to.
			return ledgerFail("conflict", "the work fence is up but no snapshot is recorded; restore from a backup before migrating")
		}
		if readErr != nil {
			return readErr
		}
		return json.Unmarshal([]byte(detail), &snapshot)
	})
	return snapshot, err
}

// snapshotSessionDB writes a consistent copy of the whole database beside it and
// returns its checksum. VACUUM INTO is the consistent-snapshot path: it reads
// one transaction's view, so the copy is never half a write, and it cannot run
// inside a transaction.
func (s *sessionDB) snapshotSessionDB(
	ctx context.Context, db *sql.DB, label string, now time.Time,
) (map[string]any, error) {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, err
	}
	if integrity != "ok" {
		return nil, ledgerFail("storage_error", "the session database failed its integrity check; migrating it would copy the damage")
	}
	// The label keeps the two directions' snapshots apart: a migration and a
	// rollback export in the same second are different starting points, and one
	// overwriting the other would leave a restore with nothing to go back to.
	// Never overwrite an existing snapshot — it may be the only copy of the
	// state something still has to go back to — so a name already taken takes
	// the next free suffix instead of failing. Two runs in the same second are
	// ordinary when a rollback is rehearsed and then taken.
	stamp := now.UTC().Format("20060102T150405Z")
	path := fmt.Sprintf("%s.%s-%s.bak", s.path, label, stamp)
	for attempt := 1; ; attempt++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return nil, err
		}
		if attempt > workSnapshotNameTries {
			return nil, ledgerFail("conflict",
				"too many snapshots of this second already exist; retry in a moment")
		}
		path = fmt.Sprintf("%s.%s-%s-%d.bak", s.path, label, stamp, attempt)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(path, "'", "''")+"'"); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path": path, "sha256": hex.EncodeToString(digest.Sum(nil)), "bytes": size,
		"integrity_check": integrity,
	}, nil
}

// legacyTaskRow is one ledger row as the migration reads it.
type legacyTaskRow struct {
	id       string
	version  int64
	status   string
	document string
	task     ledgerTask
}

func readLegacyTasks(ctx context.Context, tx *sql.Tx) ([]legacyTaskRow, error) {
	var present int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sp_ledger_tasks'").Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 {
		// A slot that never ran the ledger has nothing to migrate; that is an
		// empty migration, not a failure.
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,version,status,document FROM sp_ledger_tasks ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []legacyTaskRow{}
	for rows.Next() {
		var row legacyTaskRow
		if err := rows.Scan(&row.id, &row.version, &row.status, &row.document); err != nil {
			return nil, err
		}
		if err := decodeLedgerTask(row.document, &row.task); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// legacyRequestRow is one stored user request as the migration reads it: the
// person's words and the times and outcome the ledger really recorded.
type legacyRequestRow struct {
	id, taskID, text, author, createdAt, status string
	seq                                         int64
	version                                     int64
	claimOwner, claimExpires                    string
	operationKey, operationTarget               string
	dispatchState, dispatchedAt                 string
	relatedSeq                                  sql.NullInt64
	result, settledAt, choiceID                 string
}

func readLegacyRequests(ctx context.Context, tx *sql.Tx) ([]legacyRequestRow, error) {
	var present int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sp_ledger_user_requests'").Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id,task_id,seq,version,text,author,created_at,status,claim_owner,claim_expires_at,
		        operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at,
		        coalesce(choice_id,'')
		 FROM sp_ledger_user_requests ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []legacyRequestRow{}
	for rows.Next() {
		var row legacyRequestRow
		if err := rows.Scan(&row.id, &row.taskID, &row.seq, &row.version, &row.text, &row.author,
			&row.createdAt, &row.status, &row.claimOwner, &row.claimExpires, &row.operationKey,
			&row.operationTarget, &row.dispatchState, &row.dispatchedAt, &row.relatedSeq,
			&row.result, &row.settledAt, &row.choiceID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// currentSessionOf picks the one session a migrated task is assigned to now.
// A ledger task collects relations as it goes, so the assignment is the latest
// relation the parent still owns and has not finished with; everything else is
// history and stays in the preserved relation list. No relation that fits means
// unassigned, which is null rather than a guess.
func currentSessionOf(task ledgerTask) (*string, string) {
	for i := len(task.Sessions) - 1; i >= 0; i-- {
		relation := task.Sessions[i]
		if relation.Ownership != "owned" {
			continue
		}
		if relation.Cleanup == "deleted" || relation.Cleanup == "deleting" {
			continue
		}
		id := relation.ID
		return &id, "latest owned relation that is not being cleaned up"
	}
	return nil, "no owned, live relation recorded"
}

// legacyTaskTimes are the only task times the ledger ever recorded: the history
// rows written with the change they describe. A task older than that history
// has none, and the migration leaves both null — a created_at invented from the
// migration's own clock would say the person's task started today.
func legacyTaskTimes(ctx context.Context, tx *sql.Tx, taskID string) (*string, *string, error) {
	var present int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sp_ledger_task_events'").Scan(&present); err != nil {
		return nil, nil, err
	}
	if present == 0 {
		return nil, nil, nil
	}
	var created, updated sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT (SELECT at FROM sp_ledger_task_events WHERE task_id=? AND kind=? ORDER BY seq LIMIT 1),
		        (SELECT at FROM sp_ledger_task_events WHERE task_id=? ORDER BY seq DESC LIMIT 1)`,
		taskID, eventTaskCreated, taskID).Scan(&created, &updated)
	if err != nil {
		return nil, nil, err
	}
	return nullableString(created), nullableString(updated), nil
}

// freshLegacyChoices are the choices the session still offers on a task: the
// ones written against the version the task is at now. Anything older was
// already not shown, so it stays where it is.
func freshLegacyChoices(ctx context.Context, tx *sql.Tx, taskID string, version int64) ([]workChoice, error) {
	var present int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sp_ledger_task_choices'").Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT label,request_text FROM sp_ledger_task_choices
		 WHERE task_id=? AND basis_version=? ORDER BY position`, taskID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workChoice{}
	for rows.Next() {
		var choice workChoice
		if err := rows.Scan(&choice.Label, &choice.RequestText); err != nil {
			return nil, err
		}
		out = append(out, choice)
	}
	return out, rows.Err()
}

func planLedgerIntoWork(ctx context.Context, tx *sql.Tx, reconcile bool) (workMigrationPlan, error) {
	return migrateOrPlan(ctx, tx, nil, reconcile, time.Time{})
}

func copyLedgerIntoWork(ctx context.Context, tx *sql.Tx, reconcile bool, now time.Time) (workMigrationPlan, error) {
	return migrateOrPlan(ctx, tx, tx, reconcile, now)
}

// migrateOrPlan walks the ledger once. With write == nil it only counts, which
// is what dry_run reports; the two paths share one walk so the plan cannot
// describe a different mapping than the run performs.
// reconcile says the ledger has been authoritative since this slot last came
// forward — a rollback was exported and an older image owned the database. A
// task that is already here then has to be refreshed from the ledger rather
// than skipped, because skipping it is exactly how an edit made during the
// rollback would be lost. On a first or resumed migration reconcile is false
// and an existing task is left alone, which is what makes a resumed run add
// nothing.
func migrateOrPlan(
	ctx context.Context, tx *sql.Tx, write *sql.Tx, reconcile bool, now time.Time,
) (workMigrationPlan, error) {
	plan := workMigrationPlan{Unmapped: []workUnmapped{}, Checks: map[string]any{}}
	tasks, err := readLegacyTasks(ctx, tx)
	if err != nil {
		return plan, err
	}
	migrated := map[string]int64{}
	for _, row := range tasks {
		plan.Counts.LegacyTasks++
		kind := detailsKindOf(row.task.Details)
		relations, _ := json.Marshal(row.task.Sessions)
		details, _ := json.Marshal(row.task.Details)
		sessionID, sessionReason := currentSessionOf(row.task)
		if kind == monitorLedgerKind {
			plan.Counts.MonitorPreserved++
			if write != nil {
				if err = preserveLegacyTask(ctx, write, row, kind, string(relations), string(details), map[string]any{
					"task": "preserved only — a monitor row is the parent's own bookkeeping, not a person's work",
				}); err != nil {
					return plan, err
				}
			}
			continue
		}
		var present int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_tasks WHERE id=?", row.id).Scan(&present); err != nil {
			return plan, err
		}
		if present != 0 {
			plan.Counts.TasksPresent++
			migrated[row.id] = row.version
			if !reconcile {
				continue
			}
			plan.Counts.TasksReconciled++
			if write == nil {
				continue
			}
			if err = reconcileWorkTask(ctx, write, row, sessionID, string(relations), string(details)); err != nil {
				return plan, err
			}
			continue
		}
		var retired int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_retired WHERE id=?", row.id).Scan(&retired); err != nil {
			return plan, err
		}
		if retired != 0 {
			// The id was deleted on the work side after an earlier run. Re-creating
			// it would revive a task a person had removed.
			plan.Counts.TasksPresent++
			continue
		}
		createdAt, updatedAt, timeErr := legacyTaskTimes(ctx, tx, row.id)
		if timeErr != nil {
			return plan, timeErr
		}
		plan.Counts.TasksCreated++
		migrated[row.id] = row.version
		if write == nil {
			continue
		}
		if _, err = write.ExecContext(ctx,
			"INSERT INTO sp_work_tasks ("+workTaskColumns+") VALUES (?,?,?,?,?,?,?,?,?)",
			row.id, sessionID, row.task.Title, nil, nil, row.status, row.version, createdAt, updatedAt); err != nil {
			return plan, err
		}
		if err = preserveLegacyTask(ctx, write, row, kind, string(relations), string(details), map[string]any{
			"session_id":          sessionReason,
			"description":         "not recorded by the ledger",
			"completion_criteria": "not recorded by the ledger",
			"created_at":          "first task_created history row, null when the task predates that history",
			"updated_at":          "latest history row, null when the task predates that history",
			"version":             "carried over, so a parent holding the version it read keeps it",
		}); err != nil {
			return plan, err
		}
	}
	if err = migrateLegacyRequests(ctx, tx, write, migrated, &plan, now); err != nil {
		return plan, err
	}
	if err = attachLegacyChoices(ctx, tx, write, migrated, &plan); err != nil {
		return plan, err
	}
	return plan, verifyWorkMigration(ctx, tx, &plan)
}

// monitorLedgerKind is the details.kind that marks a parent monitoring row. It
// is the contract the app's panel already honours (docs/session-ledger.md), and
// the migration honours it for the same reason: such a row is not work a person
// asked for.
const monitorLedgerKind = "monitor"

func detailsKindOf(details map[string]any) string {
	if details == nil {
		return ""
	}
	kind, _ := details["kind"].(string)
	return strings.ToLower(strings.TrimSpace(kind))
}

func preserveLegacyTask(ctx context.Context, tx *sql.Tx, row legacyTaskRow, kind, relations, details string, provenance map[string]any) error {
	encoded, err := json.Marshal(provenance)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_task_legacy
		 (task_id,legacy_version,legacy_status,kind,stage,owner,sessions,details,provenance)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   legacy_version=excluded.legacy_version,legacy_status=excluded.legacy_status,kind=excluded.kind,
		   stage=excluded.stage,owner=excluded.owner,sessions=excluded.sessions,details=excluded.details,
		   provenance=excluded.provenance`,
		row.id, row.version, row.status, kind, row.task.Stage, row.task.Owner, relations, details, string(encoded))
	return err
}

func migrateLegacyRequests(ctx context.Context, tx *sql.Tx, write *sql.Tx, migrated map[string]int64, plan *workMigrationPlan, now time.Time) error {
	requests, err := readLegacyRequests(ctx, tx)
	if err != nil {
		return err
	}
	for _, request := range requests {
		plan.Counts.LegacyRequests++
		if _, ok := migrated[request.taskID]; !ok {
			plan.Counts.RequestsUnmapped++
			if len(plan.Unmapped) < workUnmappedReported {
				plan.Unmapped = append(plan.Unmapped, workUnmapped{
					RequestID: request.id, TaskID: request.taskID,
					Reason: "its task is not a work task (a monitor row, or a task the ledger no longer has); the request stays readable in the ledger",
				})
			}
			continue
		}
		var present int
		if err = tx.QueryRowContext(ctx,
			// Any source, not just user_request: a rollback export writes the
			// turns it exported back into the ledger as user requests, and on
			// the way forward those must be recognised as already mapped or the
			// round trip duplicates every one of them.
			"SELECT count(*) FROM sp_work_turn_legacy WHERE source_id=?", request.id).
			Scan(&present); err != nil {
			return err
		}
		if present != 0 {
			plan.Counts.TurnsPresent++
			continue
		}
		plan.Counts.TurnsCreated++
		if write == nil {
			continue
		}
		if err = insertMigratedTurn(ctx, write, request, now); err != nil {
			return err
		}
	}
	return nil
}

// insertMigratedTurn writes one turn from one stored request, and beside it
// everything about that request the turn has no column for.
func insertMigratedTurn(ctx context.Context, tx *sql.Tx, request legacyRequestRow, now time.Time) error {
	text := request.text
	explanation := (*string)(nil)
	explanationSource := "not recorded by the ledger"
	if strings.TrimSpace(request.result) != "" {
		// The ledger's `result` is the sentence the parent wrote for the person,
		// which is this turn's explanation. It is explicitly not the response:
		// the session's own final words were never stored, so claiming these are
		// them would put the parent's summary in the session's mouth.
		stored := request.result
		explanation = &stored
		explanationSource = "the request's own result — the parent's sentence for the person"
	}
	var sessionSeq *int64
	sessionSeqSource := "not recorded by the ledger"
	if request.relatedSeq.Valid {
		seq := request.relatedSeq.Int64
		sessionSeq = &seq
		sessionSeqSource = "the request's related_seq — the turn the parent recorded as carrying it"
	}
	var startedAt, endedAt *string
	if strings.TrimSpace(request.dispatchedAt) != "" {
		stamp := request.dispatchedAt
		startedAt = &stamp
	}
	if strings.TrimSpace(request.settledAt) != "" {
		stamp := request.settledAt
		endedAt = &stamp
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_turns
		 (task_id,session_seq,request,instruction,response,explanation,question,choices,artifacts,requested_at,started_at,ended_at)
		 VALUES (?,?,?,NULL,NULL,?,NULL,'[]','[]',?,?,?)`,
		request.taskID, sessionSeq, text, explanation, request.createdAt, startedAt, endedAt)
	if err != nil {
		return err
	}
	turnID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	admin, err := json.Marshal(map[string]any{
		"status": request.status, "version": request.version, "author": request.author,
		"claim_owner": request.claimOwner, "claim_expires_at": request.claimExpires,
		"operation_key": request.operationKey, "operation_target": request.operationTarget,
		"dispatch_state": request.dispatchState, "dispatched_at": request.dispatchedAt,
		"result": request.result, "settled_at": request.settledAt, "choice_id": request.choiceID,
	})
	if err != nil {
		return err
	}
	provenance, err := json.Marshal(map[string]any{
		"request":     "the person's own words, verbatim",
		"instruction": "not recorded by the ledger",
		"response":    "not recorded by the ledger — the session's final words were never stored",
		"explanation": explanationSource,
		"session_seq": sessionSeqSource,
		"started_at":  "the recorded dispatch time, null when none was recorded",
		"ended_at":    "the recorded settle time, null when none was recorded",
		"choices":     "left empty — the ledger kept choices on the task, not on a turn",
	})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_turn_legacy (turn_id,source,source_id,source_seq,claim_state,admin,provenance)
		 VALUES (?,'user_request',?,?,?,?,?)`,
		turnID, request.id, request.seq, request.status, string(admin), string(provenance)); err != nil {
		return err
	}
	if strings.TrimSpace(request.operationKey) == "" {
		return nil
	}
	// An outside action was recorded against this request. Carrying it to the
	// turn keeps the parent's reconciliation working on the new surface: without
	// it, a resend after a crash would look like a first send.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_turn_admin (turn_id,operation_key,operation_target,dispatch_state,dispatched_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(turn_id) DO NOTHING`,
		turnID, request.operationKey, request.operationTarget, request.dispatchState, request.dispatchedAt)
	return err
}

// attachLegacyChoices puts a task's still-offered choices on its latest turn,
// which is the turn they are about. A task with no turn has nowhere to hold
// them: creating one would invent an exchange that never happened, so they stay
// in the ledger and the report counts them as unattached.
func attachLegacyChoices(ctx context.Context, tx *sql.Tx, write *sql.Tx, migrated map[string]int64, plan *workMigrationPlan) error {
	for taskID, version := range migrated {
		choices, err := freshLegacyChoices(ctx, tx, taskID, version)
		if err != nil {
			return err
		}
		if len(choices) == 0 {
			continue
		}
		var latest sql.NullInt64
		if err = tx.QueryRowContext(ctx, "SELECT max(id) FROM sp_work_turns WHERE task_id=?", taskID).Scan(&latest); err != nil {
			return err
		}
		if !latest.Valid {
			plan.Counts.ChoicesUnattached++
			continue
		}
		plan.Counts.ChoicesAttached++
		if write == nil {
			continue
		}
		if _, err = write.ExecContext(ctx,
			"UPDATE sp_work_turns SET choices=? WHERE id=? AND choices='[]'",
			encodeWorkChoices(choices), latest.Int64); err != nil {
			return err
		}
	}
	return nil
}

// verifyWorkMigration re-counts what is there now. A turn whose task is missing
// would be a broken record rather than a migration, so the copy fails on it
// instead of committing.
func verifyWorkMigration(ctx context.Context, tx *sql.Tx, plan *workMigrationPlan) error {
	var orphans, tasks, turns int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sp_work_turns t LEFT JOIN sp_work_tasks k ON t.task_id=k.id WHERE k.id IS NULL`).
		Scan(&orphans); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_tasks").Scan(&tasks); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_turns").Scan(&turns); err != nil {
		return err
	}
	plan.Checks["orphan_turns"] = orphans
	plan.Checks["work_tasks"] = tasks
	plan.Checks["work_turns"] = turns
	plan.Checks["legacy_tasks_accounted"] =
		plan.Counts.TasksCreated+plan.Counts.TasksPresent+plan.Counts.MonitorPreserved == plan.Counts.LegacyTasks
	plan.Checks["legacy_requests_accounted"] =
		plan.Counts.TurnsCreated+plan.Counts.TurnsPresent+plan.Counts.RequestsUnmapped == plan.Counts.LegacyRequests
	if orphans != 0 {
		return ledgerFail("storage_error", "the migration would leave turns with no task; nothing was committed")
	}
	if plan.Checks["legacy_tasks_accounted"] != true || plan.Checks["legacy_requests_accounted"] != true {
		return ledgerFail("storage_error", "the migration could not account for every ledger row; nothing was committed")
	}
	return nil
}

func (s *sessionDB) workReportOf(ctx context.Context, db *sql.DB) (map[string]any, error) {
	var report map[string]any
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		result, readErr := workMigrationReport(ctx, tx)
		if readErr != nil {
			return readErr
		}
		report = result.(map[string]any)
		return nil
	})
	return report, err
}

// workMigrationReport is where this database stands and what was recorded on
// the way: every step with its own detail, and the counts as they are now, so a
// reader re-checks rather than trusting the run's own word.
func workMigrationReport(ctx context.Context, tx *sql.Tx) (any, error) {
	state, err := workState(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT seq,at,step,state,detail FROM sp_work_migration ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := []map[string]any{}
	for rows.Next() {
		var seq int64
		var at, step, stepState, detail string
		if err := rows.Scan(&seq, &at, &step, &stepState, &detail); err != nil {
			return nil, err
		}
		entry := map[string]any{"seq": seq, "at": at, "step": step, "state": stepState}
		var decoded any
		if json.Unmarshal([]byte(detail), &decoded) == nil {
			entry["detail"] = decoded
		}
		steps = append(steps, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for name, query := range map[string]string{
		"work_tasks":        "SELECT count(*) FROM sp_work_tasks",
		"work_turns":        "SELECT count(*) FROM sp_work_turns",
		"preserved_tasks":   "SELECT count(*) FROM sp_work_task_legacy",
		"preserved_turns":   "SELECT count(*) FROM sp_work_turn_legacy",
		"monitor_preserved": "SELECT count(*) FROM sp_work_task_legacy WHERE kind='monitor'",
	} {
		var count int
		if err := tx.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	return map[string]any{
		"state": state, "schema_version": workSchemaVersion, "steps": steps, "counts": counts,
	}, nil
}

// reconcileWorkTask refreshes a task the ledger has moved while it was
// authoritative. The agreed columns the ledger has no place for travel in the
// document's details under work_records (session_work_rollback.go wrote them
// there), so a description survives the round trip instead of being dropped the
// moment the ledger owns the row. The version is the ledger's, so a parent
// holding what it read keeps holding it.
func reconcileWorkTask(
	ctx context.Context, write *sql.Tx, row legacyTaskRow, sessionID *string, relations, details string,
) error {
	var carried map[string]any
	if decoded := map[string]any{}; json.Unmarshal([]byte(details), &decoded) == nil {
		if nested, ok := decoded["work_records"].(map[string]any); ok {
			carried = nested
		}
	}
	text := func(key string) any {
		if carried == nil {
			return nil
		}
		if value, ok := carried[key].(string); ok {
			return value
		}
		return nil
	}
	if carried != nil {
		if named, ok := carried["session_id"].(string); ok && sessionID == nil {
			sessionID = &named
		}
	}
	if _, err := write.ExecContext(ctx,
		`UPDATE sp_work_tasks
		   SET session_id=?, title=?, status=?, version=?,
		       description=coalesce(?, description),
		       completion_criteria=coalesce(?, completion_criteria)
		 WHERE id=?`,
		sessionID, row.task.Title, row.status, row.version,
		text("description"), text("completion_criteria"), row.id); err != nil {
		return err
	}
	_, err := write.ExecContext(ctx,
		`UPDATE sp_work_task_legacy SET stage=?, owner=?, sessions=?, details=?, legacy_version=?, legacy_status=?
		 WHERE task_id=?`,
		row.task.Stage, row.task.Owner, relations, details, row.version, row.status, row.id)
	return err
}
