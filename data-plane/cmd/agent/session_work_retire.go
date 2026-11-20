// Removing the ledger this slot's records came from, once they are in the work
// records (AC-N10).
//
// The move that came before this one kept everything. The ledger tables stood,
// every part of a row the agreed shape had no column for was copied verbatim
// beside the task, the snapshots were "retained", and the tools that performed
// the move stayed in tools/list. That is right while a slot can still go back,
// and it is wrong for ever — three reasons this file acts on:
//
//   - **Two copies of one truth.** A monitoring row stayed the parent's to
//     write through the ledger while a copy of it sat beside the work records,
//     diverging from the day it was made. A reader that found the copy would
//     read a stale state as a current one.
//   - **Records that are needed, in tables named after residue.** The approval
//     scope, the session cursors, the preservation and cancellation state, the
//     requests nobody has answered: all live, all read every run, none of it
//     residue. They belong in records this slot owns, under names that say what
//     they are.
//   - **A finished move that still offers to move.** The migration, the
//     rollback export and the ledger tools would stay callable for the lifetime
//     of the slot, describing records that no longer exist.
//
// So this is one call that finishes it: adopt what is still needed
// (session_work_adopt.go), verify that every row about to go is either carried
// or has nothing left that needs it, then drop the ledger, the preserved copies
// and the step log, and stop offering the tools that moved them. What is *not*
// carried is named row by row and key by key in the report — "nothing is lost"
// is a claim a reader is entitled to check, and a cleanup that cannot say what
// it deleted is one nobody can audit.
//
// It is a one-way door, and the version gate is what makes that honest rather
// than hopeful: a retired database carries sp_work_meta.version =
// workSchemaVersionRetired, which an image that only knows version 1 refuses
// outright (session_work.go, migrateWork). That image would otherwise recreate
// the ledger tables it expects and serve an empty ledger as though the records
// had been lost.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	workRetireTool = "work_retire"
	// workStateTool is the read-only state oracle. It is the one call in this
	// area that outlives the move: a parent has to know which records this slot
	// keeps before it touches any of them, and afterwards the answer is still a
	// fact rather than an assumption.
	workStateTool = "work_state"

	// workStateRetiring is the fence the move holds, and workStateRetired the
	// terminal state. One transaction adopts and drops, so a write admitted in
	// the middle would be adopted twice or not at all.
	workStateRetiring = "retiring"
	workStateRetired  = "retired"

	workRetirementStepSnapshot = "snapshot"
	workRetirementStepFence    = "fence"
	workRetirementStepRetire   = "retire"

	// workRetireSnapshotLabel names the snapshot the move takes. It is the
	// archive of everything dropped, which is what makes dropping safe — and it
	// has an end: it stays required only while this database has not been
	// verified finished with it, and is then deleted like any other finished
	// backup, with no waiting period (session_work_backups.go).
	workRetireSnapshotLabel = "pre-retire"
)

// workRetirementCounts is what the move did, in rows.
type workRetirementCounts struct {
	// Adoption is what came out of the ledger and what was left behind, with
	// the reasons (session_work_adopt.go).
	Adoption workAdoptionCounts `json:"adoption"`
	// BookCarried is the bookkeeping records moved out of a preserved copy an
	// older image's migration wrote; BookPresent the ones already owned,
	// because the adoption or a parent had written them.
	BookCarried int `json:"preserved_bookkeeping_carried"`
	BookPresent int `json:"preserved_bookkeeping_already_owned"`
	// OriginCarried and TurnOriginCarried are the "came from the ledger" facts
	// rescued from a preserved copy before it went.
	OriginCarried     int `json:"task_origin_carried"`
	TurnOriginCarried int `json:"turn_origin_carried"`
	// MonitorRescued is a preserved copy of a monitoring row that had no ledger
	// row behind it any more: the copy was the only record of that loop, so it
	// became a monitoring record rather than being dropped.
	MonitorRescued int `json:"monitors_rescued_from_copies"`
	// DispatchVerified is the turns whose recorded outside action was checked to
	// be in the owned dispatch record before the copy of it went.
	DispatchVerified int `json:"turn_dispatch_verified"`
	// Dropped is the row count of each table the move removes, by table.
	Dropped map[string]int `json:"dropped_rows"`
}

// workRetirementPlan is the whole answer to "what would this do" — and, after a
// real run, to "what did it do".
type workRetirementPlan struct {
	Counts      workRetirementCounts `json:"counts"`
	DropTables  []string             `json:"drop_tables"`
	DropRows    []workRetireDropRow  `json:"drop_rows"`
	DropFields  []string             `json:"drop_fields"`
	Carried     []workAdoptNote      `json:"carried"`
	Dropped     []workAdoptNote      `json:"dropped"`
	Checks      map[string]any       `json:"checks"`
	Refusals    []string             `json:"refusals"`
	KeptRecords []string             `json:"kept_records"`
	// MigrationSteps is the step log of an older image's migration, carried into
	// this record because the table it lived in is one of the ones being
	// dropped. How this slot got here stays readable through work_state.
	MigrationSteps []map[string]any `json:"migration_steps,omitempty"`
}

// workRetireDropRow names one row the move removes and why removing it loses
// nothing. The list is bounded the way the adoption's notes are: the counts are
// exact, the naming is a sample a person can act on.
type workRetireDropRow struct {
	Table string `json:"table"`
	Key   string `json:"key"`
	Why   string `json:"why"`
}

// runWorkRetire manages its own transactions: a consistent snapshot cannot be
// taken inside one, and the fence has to be committed before the adoption so
// that an interrupted run is still fenced when the next one arrives.
func (s *sessionDB) runWorkRetire(ctx context.Context, a workArgs, now time.Time) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.workRecordsState(ctx, db)
	if err != nil {
		return nil, err
	}
	switch state {
	case workStateRetired:
		// Already done. The report says so rather than refusing, so a parent
		// that lost its answer can ask again.
		report, reportErr := s.workStateReportOf(ctx, db)
		if reportErr != nil {
			return nil, reportErr
		}
		report["already_retired"] = true
		return report, nil
	case workStateMigrating:
		return nil, workFail("conflict", "an older image's migration holds this database; retry when it has finished")
	}
	dryRun := a.DryRun != nil && *a.DryRun
	if dryRun {
		var plan workRetirementPlan
		if err = s.workTx(ctx, db, func(tx *sql.Tx) error {
			var planErr error
			plan, planErr = retireTransition(ctx, tx, nil, now)
			return planErr
		}); err != nil {
			return nil, err
		}
		return map[string]any{
			"dry_run": true, "state": state, "plan": plan,
			"snapshot_label": workRetireSnapshotLabel,
			"next": "a real run snapshots this database first, holds a fence that refuses writes while it adopts, " +
				"and commits in one transaction; nothing above is written until then",
		}, nil
	}
	// A snapshot of the state before anything is dropped, checksummed and
	// recorded, and the fence committed before the adoption so an interrupted
	// run is still fenced.
	snapshot := map[string]any{}
	if state != workStateRetiring {
		if snapshot, err = s.snapshotSessionDB(ctx, db, workRetireSnapshotLabel, now); err != nil {
			return nil, err
		}
		if err = s.workTx(ctx, db, func(tx *sql.Tx) error {
			if recordErr := recordWorkRetirementStep(ctx, tx, workRetirementStepSnapshot, snapshot, now); recordErr != nil {
				return recordErr
			}
			if recordErr := recordWorkRetirementStep(ctx, tx, workRetirementStepFence,
				map[string]any{"state": workStateRetiring, "from": state}, now); recordErr != nil {
				return recordErr
			}
			return setWorkState(ctx, tx, workStateRetiring)
		}); err != nil {
			return nil, err
		}
	} else {
		// The fence is up from an earlier run that was interrupted after its
		// snapshot. Reuse that snapshot's record rather than taking a second
		// one: the first is the state the database was in before any adoption.
		if snapshot, err = s.workRecordedRetireSnapshot(ctx, db); err != nil {
			return nil, err
		}
	}
	var result map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		replay, fingerprint, receiptErr := workReceipt(ctx, tx, workRetireTool, a)
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
		plan, retireErr := retireTransition(ctx, tx, tx, now)
		if retireErr != nil {
			return retireErr
		}
		if recordErr := recordWorkRetirementStep(ctx, tx, workRetirementStepRetire, plan, now); recordErr != nil {
			return recordErr
		}
		if stateErr := setWorkState(ctx, tx, workStateRetired); stateErr != nil {
			return stateErr
		}
		// The version moves with the state. An image that only knows version 1
		// reads sp_work_meta before anything else and refuses a version it was
		// not written for, so it cannot serve a retired database half-way.
		if _, stateErr := tx.ExecContext(ctx,
			"UPDATE sp_work_meta SET version=?", workSchemaVersionRetired); stateErr != nil {
			return stateErr
		}
		result = map[string]any{
			"state": workStateRetired, "schema_version": workSchemaVersionRetired,
			"snapshot": snapshot, "plan": plan,
			"removed_tools": workRetiredToolNames(),
			"next": "this slot's records are the work records only: " + workStateTool + " reports where it stands, " +
				taskBookGetTool + "/" + taskBookPutTool + " carry the parent's bookkeeping, " + monitorListTool +
				" the monitoring loops, and a task's history is its turns. Rolling the image back is not a step — a " +
				"version-1 image refuses this database rather than serving a ledger that is gone. " +
				workBackupsTool + " says which snapshots are still required and which may now be deleted, and " +
				workBackupsPruneTool + " deletes them.",
		}
		return storeWorkReceipt(ctx, tx, a.RequestID, fingerprint, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// workRetiredToolNames is what a retired slot no longer offers. The names this
// image removed outright are not here — they are gone for every slot, retired
// or not, and a call to one is answered with its successor (session_work.go).
func workRetiredToolNames() []string {
	names := []string{}
	for _, name := range workTools {
		if isWorkTransitionTool(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *sessionDB) workRecordedRetireSnapshot(ctx context.Context, db *sql.DB) (map[string]any, error) {
	snapshot := map[string]any{}
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var detail string
		readErr := tx.QueryRowContext(ctx,
			"SELECT detail FROM sp_work_retirement WHERE step=? ORDER BY seq DESC LIMIT 1",
			workRetirementStepSnapshot).Scan(&detail)
		if errors.Is(readErr, sql.ErrNoRows) {
			// The fence is up with no snapshot recorded, which this code cannot
			// produce. Refusing is the only safe answer: dropping rows with no
			// recorded archive of them has nothing to go back to.
			return workFail("conflict",
				"the fence is up but no snapshot is recorded; restore from a backup before retiring")
		}
		if readErr != nil {
			return readErr
		}
		return json.Unmarshal([]byte(detail), &snapshot)
	})
	return snapshot, err
}

// retireTransition is the whole adopt-verify-drop, walked once. With write ==
// nil it only counts and checks, which is what dry_run reports; the two paths
// share one walk so the plan cannot describe a different cleanup than the run
// performs.
func retireTransition(ctx context.Context, tx *sql.Tx, write *sql.Tx, now time.Time) (workRetirementPlan, error) {
	plan := workRetirementPlan{
		Counts:     workRetirementCounts{Dropped: map[string]int{}},
		DropTables: []string{},
		DropRows:   []workRetireDropRow{},
		DropFields: []string{},
		Carried:    []workAdoptNote{},
		Dropped:    []workAdoptNote{},
		Checks:     map[string]any{},
		Refusals:   []string{},
		KeptRecords: []string{
			"sp_work_tasks, sp_work_turns — the agreed records",
			"sp_work_task_book — the parent's live bookkeeping (approval scope, the session relations it can still " +
				"resume from, preservation, cancellation, notification, direct-execution responsibility)",
			"sp_work_monitor — the parent's monitoring loops, which were never a person's work",
			"sp_work_task_origin, sp_work_turn_origin — where a record came from, the id it was stored under, the " +
				"author of a person's words",
			"sp_work_turn_admin, sp_work_turn_claim — the recorded outside action and the turn's owner",
			"sp_work_task_guard, sp_work_receipts, sp_work_retired — the write lanes, the replay receipts, the " +
				"retired ids",
			"sp_work_retirement — what this move adopted and dropped, and why",
		},
	}
	steps, err := readMigrationSteps(ctx, tx)
	if err != nil {
		return plan, err
	}
	plan.MigrationSteps = steps
	// The ledger first: it is the live source, so what it holds decides which
	// tasks the work records own by the time the preserved copies are judged.
	adoption := workAdoptionPlan{Checks: map[string]any{}}
	if err = adoptLedgerRecords(ctx, tx, write, &adoption, now); err != nil {
		return plan, err
	}
	if err = verifyAdoption(ctx, tx, &adoption); err != nil {
		return plan, err
	}
	plan.Counts.Adoption = adoption.Counts
	plan.Carried = append(plan.Carried, adoption.Carried...)
	plan.Dropped = append(plan.Dropped, adoption.Dropped...)
	plan.Refusals = append(plan.Refusals, adoption.Refusals...)
	for name, value := range adoption.Checks {
		plan.Checks["adoption_"+name] = value
	}
	if err = carryPreservedBookkeeping(ctx, tx, write, &plan, now); err != nil {
		return plan, err
	}
	if err = carryTurnOrigins(ctx, tx, write, &plan); err != nil {
		return plan, err
	}
	if err = countRetiredTables(ctx, tx, &plan); err != nil {
		return plan, err
	}
	if err = verifyRetirement(ctx, tx, write, &plan); err != nil {
		return plan, err
	}
	if write == nil {
		return plan, nil
	}
	// The table names are this file's own constants, never caller input.
	// Dropping the table is what deletes the rows the walk decided nothing
	// needs — the duplicates of records the work records now hold, the settled
	// requests, the past history — and it commits in the same transaction as
	// the retirement record that says what went.
	for _, table := range plan.DropTables {
		if _, err := write.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

// carryPreservedBookkeeping deals with the verbatim copies an older image's
// migration wrote beside each moved row. The adoption has already taken what
// the ledger itself holds, so by the time this runs a copy is one of three
// things: the bookkeeping of a task that is now owned (carried if nothing owns
// it yet), the last record of a monitoring loop whose ledger row is gone
// (rescued), or the copy of a task that was deleted on purpose (dropped).
func carryPreservedBookkeeping(
	ctx context.Context, tx *sql.Tx, write *sql.Tx, plan *workRetirementPlan, now time.Time,
) error {
	present, err := workTableExists(ctx, tx, "sp_work_task_legacy")
	if err != nil || !present {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT task_id,stage,owner,sessions,details,kind,provenance,legacy_status
		 FROM sp_work_task_legacy ORDER BY task_id`)
	if err != nil {
		return err
	}
	type preserved struct {
		taskID, stage, owner, sessions, details, kind, provenance, status string
	}
	all := []preserved{}
	for rows.Next() {
		var row preserved
		if scanErr := rows.Scan(&row.taskID, &row.stage, &row.owner, &row.sessions,
			&row.details, &row.kind, &row.provenance, &row.status); scanErr != nil {
			rows.Close()
			return scanErr
		}
		all = append(all, row)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, row := range all {
		_, owned, ownErr := workTaskVersionOf(ctx, tx, row.taskID)
		if ownErr != nil {
			return ownErr
		}
		if !owned {
			if row.kind != adoptMonitorKind {
				plan.DropRows = append(plan.DropRows, workRetireDropRow{
					Table: "sp_work_task_legacy", Key: row.taskID,
					Why: "no work task owns this id — it was deleted after the records moved, so the copy of its " +
						"bookkeeping is a record of something nothing will read again",
				})
				continue
			}
			if err = rescuePreservedMonitor(ctx, tx, write, row.taskID, row.stage, row.details, row.status, plan, now); err != nil {
				return err
			}
			continue
		}
		var already int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_work_task_book WHERE task_id=?", row.taskID).Scan(&already); err != nil {
			return err
		}
		if already != 0 || plan.Counts.Adoption.BookCarried != 0 && write == nil {
			// Something owns this task's bookkeeping already — the adoption just
			// wrote it from the ledger row, or a parent wrote it after the
			// cutover. Either way the owned record is the live one and this copy
			// is the older of the two.
			plan.Counts.BookPresent++
		} else {
			plan.Counts.BookCarried++
			if write != nil {
				relations, _ := liveRelations(decodePreservedRelations(row.sessions))
				details, _, _ := liveDetails(decodePreservedDetails(row.details))
				encodedRelations, marshalErr := json.Marshal(relations)
				if marshalErr != nil {
					return marshalErr
				}
				encodedDetails, marshalErr := json.Marshal(details)
				if marshalErr != nil {
					return marshalErr
				}
				if _, err = write.ExecContext(ctx,
					`INSERT INTO sp_work_task_book (task_id,stage,owner,sessions,details) VALUES (?,?,?,?,?)
					 ON CONFLICT(task_id) DO NOTHING`,
					row.taskID, row.stage, row.owner, string(encodedRelations), string(encodedDetails)); err != nil {
					return err
				}
			}
		}
		plan.DropRows = append(plan.DropRows, workRetireDropRow{
			Table: "sp_work_task_legacy", Key: row.taskID,
			Why: "the live bookkeeping is in sp_work_task_book; what is left describes a ledger row that is going " +
				"with this cleanup",
		})
		// "Came from the ledger" is a fact about the task, and a slot migrated by
		// an older image has it only in the provenance of this copy. Writing it
		// down is what keeps `migrated` answerable after the copy is gone.
		if isBookkeepingProvenance(row.provenance) {
			continue
		}
		var origin int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_work_task_origin WHERE task_id=?", row.taskID).Scan(&origin); err != nil {
			return err
		}
		if origin != 0 {
			continue
		}
		plan.Counts.OriginCarried++
		if write != nil {
			if _, err = write.ExecContext(ctx,
				"INSERT INTO sp_work_task_origin (task_id,origin) VALUES (?,?) ON CONFLICT(task_id) DO NOTHING",
				row.taskID, workOriginLedger); err != nil {
				return err
			}
		}
	}
	return nil
}

// rescuePreservedMonitor turns the last copy of a monitoring loop into a
// monitoring record. The ledger row it was copied from is gone — deleted while
// the copy sat there — so this copy is the only record of the loop, and
// dropping it would be the one case where this cleanup loses something.
func rescuePreservedMonitor(
	ctx context.Context, tx *sql.Tx, write *sql.Tx, id, title, details, status string,
	plan *workRetirementPlan, now time.Time,
) error {
	var owned int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_monitor WHERE id=?", id).Scan(&owned); err != nil {
		return err
	}
	if owned != 0 {
		plan.DropRows = append(plan.DropRows, workRetireDropRow{
			Table: "sp_work_task_legacy", Key: id,
			Why: "this monitoring loop is in sp_work_monitor; the copy has been standing still since it was made",
		})
		return nil
	}
	if status == "completed" {
		plan.DropRows = append(plan.DropRows, workRetireDropRow{
			Table: "sp_work_task_legacy", Key: id,
			Why: "a monitoring loop that finished; nothing reads it again",
		})
		return nil
	}
	plan.Counts.MonitorRescued++
	plan.DropRows = append(plan.DropRows, workRetireDropRow{
		Table: "sp_work_task_legacy", Key: id,
		Why: "the only copy left of a monitoring loop; carried into sp_work_monitor before this table goes",
	})
	if write == nil {
		return nil
	}
	note, err := json.Marshal(monitorNoteOf(decodePreservedDetails(details)))
	if err != nil {
		return err
	}
	text := string(note)
	if text == "{}" {
		text = ""
	}
	state := monitorStateRunning
	if status != "running" {
		state = monitorStateStopped
	}
	name := title
	if name == "" {
		name = id
	}
	return adoptMonitorRow(ctx, write, workMonitor{
		ID: id, Title: name, State: state, Note: text, UpdatedAt: workNow(now),
	})
}

func decodePreservedRelations(raw string) []workBookSession {
	relations := []workBookSession{}
	if raw == "" {
		return relations
	}
	if err := json.Unmarshal([]byte(raw), &relations); err != nil || relations == nil {
		return []workBookSession{}
	}
	return relations
}

func decodePreservedDetails(raw string) map[string]any {
	details := map[string]any{}
	if raw == "" {
		return details
	}
	if err := json.Unmarshal([]byte(raw), &details); err != nil || details == nil {
		return map[string]any{}
	}
	return details
}

// carryTurnOrigins moves the three facts a turn's preserved copy holds that the
// read path and a retried store still need — where it came from, the id it was
// stored under, who the words were from — and checks that a recorded outside
// action is in the owned dispatch record before the copy of it goes.
func carryTurnOrigins(ctx context.Context, tx *sql.Tx, write *sql.Tx, plan *workRetirementPlan) error {
	present, err := workTableExists(ctx, tx, "sp_work_turn_legacy")
	if err != nil || !present {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT turn_id,source,source_id,admin FROM sp_work_turn_legacy ORDER BY turn_id")
	if err != nil {
		return err
	}
	type preserved struct {
		turnID                  int64
		source, sourceID, admin string
	}
	all := []preserved{}
	for rows.Next() {
		var row preserved
		if scanErr := rows.Scan(&row.turnID, &row.source, &row.sourceID, &row.admin); scanErr != nil {
			rows.Close()
			return scanErr
		}
		all = append(all, row)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, row := range all {
		var already int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_work_turn_origin WHERE turn_id=?", row.turnID).Scan(&already); err != nil {
			return err
		}
		if already == 0 {
			plan.Counts.TurnOriginCarried++
			if write != nil {
				if err = recordTurnOrigin(ctx, write, row.turnID, row.source, row.sourceID,
					adminAuthor(row.admin)); err != nil {
					return err
				}
			}
		}
		// An outside action recorded against the request this turn came from was
		// copied into the owned dispatch record by the migration. The copy in
		// `admin` is the duplicate; dropping it is only safe while the owned one
		// is there, so this checks rather than assumes.
		if key := adminOperationKey(row.admin); key != "" {
			var dispatch int
			if err = tx.QueryRowContext(ctx,
				"SELECT count(*) FROM sp_work_turn_admin WHERE turn_id=? AND operation_key=?",
				row.turnID, key).Scan(&dispatch); err != nil {
				return err
			}
			if dispatch == 0 {
				plan.Refusals = append(plan.Refusals,
					"sp_work_turn_legacy turn "+strconv.FormatInt(row.turnID, 10)+" records operation key "+key+
						" that is not in sp_work_turn_admin, so dropping the copy would lose what the parent "+
						"handed to another system")
				continue
			}
			plan.Counts.DispatchVerified++
		}
		plan.DropRows = append(plan.DropRows, workRetireDropRow{
			Table: "sp_work_turn_legacy", Key: strconv.FormatInt(row.turnID, 10),
			Why: "origin, source id and author carried into sp_work_turn_origin; the recorded dispatch is in " +
				"sp_work_turn_admin; the rest was a copy of a ledger request going with this cleanup",
		})
	}
	return nil
}

func adminOperationKey(admin string) string {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(admin), &decoded); err != nil {
		return ""
	}
	key, _ := decoded["operation_key"].(string)
	return key
}

// countRetiredTables counts what each dropped table holds now and names the
// fields that go with it. A field that is dropped rather than carried is named
// one by one, for the same reason the adoption names every details key it drops.
func countRetiredTables(ctx context.Context, tx *sql.Tx, plan *workRetirementPlan) error {
	for _, table := range workRetiredTables {
		present, err := workTableExists(ctx, tx, table)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			return err
		}
		plan.Counts.Dropped[table] = count
		plan.DropTables = append(plan.DropTables, table)
	}
	plan.DropFields = []string{
		"sp_ledger_tasks.document.details — the keys that record something that already happened (a result " +
			"summary, a PR or head link, import provenance, an observed_at, an operation intent). The turn that did " +
			"the work holds its own result and artifacts, and turn_dispatch holds the intent",
		"sp_ledger_tasks.document.sessions[] with cleanup=deleted — a cursor into a session that is gone",
		"sp_ledger_task_events.* — the task history before the cutover. A past management event was never a turn, " +
			"and a task's history from here on is its turns; the created_at and updated_at this history recorded are " +
			"read into the task before it goes",
		"sp_ledger_user_requests.* that are settled — the person was told the outcome at the time",
		"sp_ledger_user_requests.claim_owner, .claim_expires_at, .version, .seq — the ledger's own claim lane, " +
			"replaced by the turn's claim token and the turn lane's counter",
		"sp_ledger_task_choices.* — a choice is a turn's now, and the live ones are carried onto the turn they " +
			"belong to; a stale one was already not shown to anybody",
		"sp_ledger_requests.* — replay receipts for tools that no longer exist",
		"sp_work_task_legacy.*, sp_work_turn_legacy.* — the verbatim copies an older image's migration kept. " +
			"What is live is in the owned records above; what is not is named in this record",
		"sp_work_migration.* — the migration's step log; its counts and snapshot checksums are folded into " +
			"sp_work_retirement so what happened stays readable through " + workStateTool,
	}
	return nil
}

// verifyRetirement is the check before the drop. It fails the whole run rather
// than committing a cleanup that leaves a record unreachable — the rows are
// about to go, so every one of these has to hold *now*.
//
// It has to know whether the carry has happened yet, which is what `write`
// says. A real run has already inserted everything it carried into this same
// transaction, so the counts it reads include them; a rehearsal has inserted
// nothing, so the same counts are short by exactly what it would carry. Adding
// the carried counts unconditionally was wrong in both directions: it
// double-counted a real run — which made a correctly carried origin look
// uncarried and refused a sound cleanup — and it is the difference between a
// check and a number that happens to come out right in a rehearsal.
//
// A rehearsal reports and does not refuse: the refusals are what a person
// approves or acts on, and a dry run that returns an error instead of the plan
// naming them tells nobody which row was the problem.
func verifyRetirement(ctx context.Context, tx *sql.Tx, write *sql.Tx, plan *workRetirementPlan) error {
	var tasks, turns, orphans, book, taskOrigin, turnOrigin, monitors, openTurns int
	for query, into := range map[string]*int{
		"SELECT count(*) FROM sp_work_tasks":                                                &tasks,
		"SELECT count(*) FROM sp_work_turns":                                                &turns,
		"SELECT count(*) FROM sp_work_task_book":                                            &book,
		"SELECT count(*) FROM sp_work_task_origin":                                          &taskOrigin,
		"SELECT count(*) FROM sp_work_turn_origin":                                          &turnOrigin,
		"SELECT count(*) FROM sp_work_monitor":                                              &monitors,
		"SELECT count(*) FROM sp_work_turns WHERE request IS NOT NULL AND ended_at IS NULL": &openTurns,
		`SELECT count(*) FROM sp_work_turns t LEFT JOIN sp_work_tasks k ON t.task_id=k.id
		 WHERE k.id IS NULL`: &orphans,
	} {
		if err := tx.QueryRowContext(ctx, query).Scan(into); err != nil {
			return err
		}
	}
	// Every turn a reader is told the origin of has to keep its origin after the
	// carry.
	var originGap int
	legacyTurns, err := workTableExists(ctx, tx, "sp_work_turn_legacy")
	if err != nil {
		return err
	}
	if legacyTurns {
		if err = tx.QueryRowContext(ctx,
			`SELECT count(*) FROM sp_work_turn_legacy l
			 LEFT JOIN sp_work_turn_origin o ON o.turn_id=l.turn_id
			 WHERE o.turn_id IS NULL`).Scan(&originGap); err != nil {
			return err
		}
	}
	// Every request a person is still waiting on has to be answerable
	// afterwards: an open ledger request either became a turn in this run, was
	// already one, or stopped the run (adoption refusals).
	openLedger := 0
	requests, err := workTableExists(ctx, tx, "sp_ledger_user_requests")
	if err != nil {
		return err
	}
	if requests {
		if err = tx.QueryRowContext(ctx,
			`SELECT count(*) FROM sp_ledger_user_requests
			 WHERE status IN ('pending','claimed','reconciling')`).Scan(&openLedger); err != nil {
			return err
		}
	}
	// pending is the carry this walk has decided on but not yet written. In a
	// real run it is zero, because the inserts are already in this transaction.
	adoption := plan.Counts.Adoption
	pending := func(carried int) int {
		if write == nil {
			return carried
		}
		return 0
	}
	plan.Checks["work_tasks"] = tasks
	plan.Checks["work_turns"] = turns
	plan.Checks["open_turns"] = openTurns
	plan.Checks["orphan_turns"] = orphans
	plan.Checks["monitors"] = monitors + pending(adoption.MonitorsCarried+plan.Counts.MonitorRescued)
	plan.Checks["bookkeeping_records"] = book + pending(adoption.BookCarried+plan.Counts.BookCarried)
	plan.Checks["task_origins"] = taskOrigin + pending(plan.Counts.OriginCarried)
	plan.Checks["turn_origins"] = turnOrigin + pending(plan.Counts.TurnOriginCarried)
	plan.Checks["turn_origins_uncarried"] = originGap - pending(plan.Counts.TurnOriginCarried)
	plan.Checks["open_requests_in_ledger"] = openLedger
	plan.Checks["open_requests_carried_or_present"] = adoption.TurnsCreated + adoption.TurnsPresent
	plan.Checks["refusals"] = len(plan.Refusals)
	if orphans != 0 {
		// Damage rather than a decision: it is reported the same way to a
		// rehearsal, because there is nothing here to approve.
		return workFail("storage_error", "this database has turns with no task; nothing was changed")
	}
	if write == nil {
		return nil
	}
	if len(plan.Refusals) != 0 {
		// The refusals travel in the error, not only in the plan. A parent that
		// called this without dry_run gets no plan back, and "something would be
		// lost" is not something anybody can act on without knowing what.
		return workFail("conflict",
			"this cleanup would drop something nothing else holds; nothing was changed — "+
				strings.Join(plan.Refusals, "; ")+". "+workRetireTool+
				" with dry_run reports the same without writing")
	}
	if plan.Checks["turn_origins_uncarried"] != 0 {
		return workFail("storage_error", "not every preserved turn would keep its origin; nothing was changed")
	}
	return nil
}

func recordWorkRetirementStep(ctx context.Context, tx *sql.Tx, step string, detail any, now time.Time) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO sp_work_retirement (at,step,detail) VALUES (?,?,?)", workNow(now), step, string(encoded))
	return err
}

func workRetireToolDefinition() map[string]any {
	return map[string]any{
		"name": workRetireTool,
		"description": "Finishes this slot's move off the ledger, in one direction. It adopts what is still needed — " +
			"a task a person has, the parent's live bookkeeping beside it (approval scope, the session relations it " +
			"can still resume from, preservation and cancellation), a request nobody has dealt with (which becomes a " +
			"turn), the still-offered choices, a monitoring loop that is still running, and the retired ids — then " +
			"drops the ledger, the verbatim copies an older migration kept and its step log. What it does not carry " +
			"it names: every details key, session relation, settled request and history row, with what holds that " +
			"information now. A request nobody has dealt with whose task is not a work task stops the whole run " +
			"rather than being deleted. dry_run=true reports all of it and changes nothing. A real run snapshots the " +
			"database first (that snapshot is the archive of everything dropped), holds a fence that refuses writes " +
			"while it adopts, and commits in one transaction, so an interrupted run leaves the database as it was " +
			"and can simply be run again. It is one-way: a retired database carries a schema version an image " +
			"without these records refuses.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
			"request_id": map[string]any{"type": "string"},
			"dry_run":    map[string]any{"type": "boolean"},
		}, "required": []string{"request_id"}, "additionalProperties": false},
	}
}

func workStateToolDefinition() map[string]any {
	return map[string]any{
		"name": workStateTool,
		"description": "Where this slot's records stand: state (ready = a ledger is still behind this slot and " +
			"nothing has been adopted · migrating · migrated = an older image moved the records and left the ledger " +
			"standing · rolled_back · retiring · retired = the work records are the only records, which is also " +
			"where a new slot starts), the schema version, every recorded step with its row counts and snapshot " +
			"checksums, and the current counts so a reader can re-check them. Read this before deciding which " +
			"records to use; do not infer the state from which tools exist. Reading changes nothing.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{},
			"required": []string{}, "additionalProperties": false},
	}
}
