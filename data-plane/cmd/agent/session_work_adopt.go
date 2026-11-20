// Taking what this slot still needs out of the ledger, once, on the way to
// removing it (AC-N10).
//
// This replaces the migration that came before it, and the difference is the
// point. The migration kept everything: the ledger tables stood, and every part
// of a row the agreed shape had no column for was copied verbatim beside the
// task. That is right while a slot can still go back, and wrong for ever —
// "preserved" was doing the work of "needed", and nothing ever established that
// any of it was.
//
// So this walk selects, and it selects by consumer:
//
//   - A **task** a person has becomes a work task. One the work records already
//     have is left alone: they are authoritative, and refreshing a task from a
//     ledger row nothing has written since would undo the parent's own work.
//   - The parent's **live bookkeeping** beside that task — the stage it is at,
//     the owner, the session relations that still have a cursor to resume from,
//     and the details keys that are current state (approval scope,
//     preservation, cancellation, notification, direct-execution
//     responsibility) — goes into the owned record. The details keys that are a
//     record of something that already happened (a result summary, a PR link,
//     import provenance, an observed_at, an operation intent) do **not**: the
//     turn that did the work carries its own result, and the turn's dispatch
//     record carries the intent. Every dropped key is named in the report.
//   - A **request nobody has dealt with** becomes a turn, because that is a
//     person waiting for an answer. A request that was settled does not: its
//     outcome was told to the person at the time, and keeping the row would be
//     keeping a second copy of a finished exchange.
//   - A **monitoring loop** the parent still runs becomes a monitoring record
//     (session_work_monitor.go). One whose ledger row says it finished does
//     not.
//   - A **retired id** carries over, because it is what keeps a deleted task
//     from being recreated.
//   - The **task history** does not carry. A past management event is not a
//     turn — dressing one up as a turn puts words in a person's mouth — and the
//     history of a task from here on is its turns. The times the history
//     recorded are read before it goes, so a task keeps the created_at it
//     really had.
//
// Nothing here guesses. A time or a seq the ledger does not have stays null, a
// field it never recorded (the instruction that went to a session, that
// session's own final answer) stays null, and a status does not move because
// the data did.
//
// What cannot be carried is **refused**, not dropped: an open request whose
// task is not a work task has nobody to answer it and no turn to become, so the
// whole run stops and names it. A parent settles it or creates the task, and
// runs again.
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
	"sort"
	"strings"
	"time"
)

// workAdoptReported bounds how many rows the report names one by one. The
// counts are always exact; the list is a sample a person can act on.
const workAdoptReported = 20

// The request statuses the ledger wrote. The first three are a person still
// waiting — nobody has answered, a parent took it, or a parent is reconciling
// an outside action it may have started — and the last two are done with.
const (
	adoptRequestPending     = "pending"
	adoptRequestClaimed     = "claimed"
	adoptRequestReconciling = "reconciling"
)

func adoptRequestIsOpen(status string) bool {
	switch status {
	case adoptRequestPending, adoptRequestClaimed, adoptRequestReconciling:
		return true
	}
	return false
}

// adoptMonitorKind is the details.kind that marked a parent monitoring row. The
// ledger had no column for it, so it was a convention in a JSON — which is one
// of the reasons these rows get a record of their own now.
const adoptMonitorKind = "monitor"

// workLiveDetailKeys are the details keys that are current state the parent
// acts on, each with why it is still needed. Everything here is carried.
//
// The list is not a guess about what a parent might want: every entry is a key
// the managing skillset actually reads (plugin-marketplace skillset/
// task-session-lifecycle and task-monitoring-lifecycle). That is what makes
// "only what is needed" checkable — a key is live because a consumer reads it,
// not because it looked important.
var workLiveDetailKeys = map[string]string{
	"kind":             "what this row is",
	"approval":         "the approval scope in force",
	"approvals":        "the approval scope in force",
	"approval_scope":   "the approval scope in force",
	"restrictions":     "the limits that come with that approval",
	"preservation":     "what is being preserved and why",
	"preserve":         "what is being preserved and why",
	"cancellation":     "a cancellation the parent has to honour",
	"canceled":         "a cancellation the parent has to honour",
	"cancelled":        "a cancellation the parent has to honour",
	"notification":     "who was notified and whether it has been sent",
	"direct_execution": "the responsibility the parent took for running this itself",
	"responsibility":   "the responsibility the parent took for running this itself",

	// The keys the lifecycle skillset reads every run. They were reaching the
	// owned record through the unclassified catch-all, which carried them but
	// reported each one as a key nothing here recognised — so every ordinary
	// row asked a person about state the parent depends on.
	"lifecycle":     "the stage, wait reason and next action the parent judges state from",
	"stage":         "the display summary of lifecycle.stage, kept in the same record",
	"resumption":    "an in-flight restore: its operation key, state and evidence",
	"observation":   "the platform and execution state last observed, with its source",
	"monitor_ref":   "which monitoring loop this task is connected to",
	"monitoring":    "the parent's own scheduling record for this task",
	"queue":         "the single-queue form of that scheduling record",
	"blocker":       "what is blocking this task, which the handover reads",
	"next_action":   "what comes next and whose it is, which the handover reads",
	"report":        "the handover display, including the remaining steps",
	"priority":      "the order the parent works tasks in",
	"goal":          "what this task is for, as the parent recorded it",
	"cleanup_check": "the cleanup the parent still owes",
	"reuse_plans":   "the sessions this task plans to reuse",
	"archive":       "the archive state the parent tracks",
}

// workPastDetailKeys are the details keys that record something that already
// happened, each with what holds it now. Everything here is dropped.
var workPastDetailKeys = map[string]string{
	"result":             "the turn's explanation is the account of a result",
	"results":            "the turn's explanation is the account of a result",
	"summary":            "the turn's explanation is the account of a result",
	"pr":                 "a link to what was produced is a turn artifact",
	"pr_url":             "a link to what was produced is a turn artifact",
	"head":               "a link to what was produced is a turn artifact",
	"links":              "a link to what was produced is a turn artifact",
	"validation":         "a check that already ran is not current state",
	"provenance":         "how a historical row was imported is not current state",
	"observed_at":        "when a historical row was observed is not current state",
	"history":            "a task's history is its turns",
	"events":             "a task's history is its turns",
	"log":                "a task's history is its turns",
	"operation":          "an outside action is recorded on the turn it belongs to (turn_dispatch)",
	"external_operation": "an outside action is recorded on the turn it belongs to (turn_dispatch)",
	"intent":             "an outside action is recorded on the turn it belongs to (turn_dispatch)",
}

// adoptedTask is the ledger's task document, read for the fields something
// still needs. It is a decode shape rather than a record: nothing in this image
// writes one.
type adoptedTask struct {
	Title    string            `json:"title"`
	Status   string            `json:"status"`
	Stage    string            `json:"stage"`
	Owner    string            `json:"owner"`
	Sessions []workBookSession `json:"sessions"`
	Details  map[string]any    `json:"details"`
}

type adoptedTaskRow struct {
	id      string
	version int64
	status  string
	task    adoptedTask
}

// adoptedRequestRow is one stored request, read for the person's words and the
// times the ledger really recorded.
type adoptedRequestRow struct {
	id, taskID, text, author, createdAt, status string
	operationKey, operationTarget               string
	dispatchState, dispatchedAt                 string
	relatedSeq                                  sql.NullInt64
	result, settledAt                           string
}

// workAdoptionCounts is what the walk did, in rows. Every row the ledger holds
// is in exactly one of these, which is what the accounting check verifies
// before anything is dropped.
type workAdoptionCounts struct {
	LedgerTasks         int `json:"ledger_tasks"`
	TasksCreated        int `json:"tasks_created"`
	TasksPresent        int `json:"tasks_present"`
	TasksRetiredSkipped int `json:"tasks_retired_skipped"`
	BookCarried         int `json:"bookkeeping_carried"`
	BookPresent         int `json:"bookkeeping_already_owned"`
	MonitorsCarried     int `json:"monitors_carried"`
	MonitorsDropped     int `json:"monitors_dropped"`
	LedgerRequests      int `json:"ledger_requests"`
	TurnsCreated        int `json:"turns_created"`
	TurnsPresent        int `json:"turns_present"`
	RequestsDropped     int `json:"requests_dropped"`
	ChoicesAttached     int `json:"choices_attached"`
	ChoicesDropped      int `json:"choices_dropped"`
	RetiredIDsCarried   int `json:"retired_ids_carried"`
	EventsDropped       int `json:"history_rows_dropped"`
	ReceiptsDropped     int `json:"ledger_receipts_dropped"`
	DetailKeysDropped   int `json:"detail_keys_dropped"`
	RelationsDropped    int `json:"session_relations_dropped"`
}

// workAdoptNote names one thing the walk moved or removed, and why. "Nothing is
// lost" is a claim a reader is entitled to check, so the list is specific:
// which record, which part of it, and what holds it now.
type workAdoptNote struct {
	Record string `json:"record"`
	Part   string `json:"part,omitempty"`
	Why    string `json:"why"`
}

type workAdoptionPlan struct {
	Counts   workAdoptionCounts `json:"counts"`
	Carried  []workAdoptNote    `json:"carried"`
	Dropped  []workAdoptNote    `json:"dropped"`
	Refusals []string           `json:"refusals"`
	Checks   map[string]any     `json:"checks"`
}

func (p *workAdoptionPlan) carried(record, part, why string) {
	if len(p.Carried) < workAdoptReported {
		p.Carried = append(p.Carried, workAdoptNote{Record: record, Part: part, Why: why})
	}
}

func (p *workAdoptionPlan) dropped(record, part, why string) {
	if len(p.Dropped) < workAdoptReported {
		p.Dropped = append(p.Dropped, workAdoptNote{Record: record, Part: part, Why: why})
	}
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

func (s *sessionDB) workRecordsState(ctx context.Context, db *sql.DB) (string, error) {
	state := ""
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var readErr error
		state, readErr = workState(ctx, tx)
		return readErr
	})
	return state, err
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
		return nil, workFail("storage_error",
			"the session database failed its integrity check; moving its records would copy the damage")
	}
	// Never overwrite an existing snapshot — it may be the only copy of a state
	// something still has to go back to — so a name already taken takes the next
	// free suffix instead of failing.
	stamp := now.UTC().Format("20060102T150405Z")
	path := fmt.Sprintf("%s.%s-%s.bak", s.path, label, stamp)
	for attempt := 1; ; attempt++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return nil, err
		}
		if attempt > workSnapshotNameTries {
			return nil, workFail("conflict",
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

// adoptLedgerRecords walks the ledger once. With write == nil it only counts and
// checks, which is what a rehearsal reports; the two paths share one walk so a
// rehearsal cannot describe a different selection than the run performs.
func adoptLedgerRecords(ctx context.Context, tx *sql.Tx, write *sql.Tx, plan *workAdoptionPlan, now time.Time) error {
	tasks, err := readLedgerTasks(ctx, tx)
	if err != nil {
		return err
	}
	adopted := map[string]int64{}
	for _, row := range tasks {
		plan.Counts.LedgerTasks++
		if detailsKindOf(row.task.Details) == adoptMonitorKind {
			if err = adoptMonitor(ctx, write, row, plan, now); err != nil {
				return err
			}
			continue
		}
		owned, err := adoptTaskRow(ctx, tx, write, row, adopted, plan)
		if err != nil {
			return err
		}
		if !owned {
			continue
		}
		if err = adoptTaskBook(ctx, tx, write, row, plan); err != nil {
			return err
		}
	}
	if err = adoptRequests(ctx, tx, write, adopted, plan, now); err != nil {
		return err
	}
	if err = adoptChoices(ctx, tx, write, adopted, plan); err != nil {
		return err
	}
	if err = adoptRetiredIDs(ctx, tx, write, plan); err != nil {
		return err
	}
	return countLedgerLeftovers(ctx, tx, plan)
}

// adoptTaskRow puts one ledger task into the work records, or establishes that
// it is already there. It reports whether the work records own the id
// afterwards, which is what decides whether its bookkeeping and its requests
// have anywhere to go.
func adoptTaskRow(
	ctx context.Context, tx *sql.Tx, write *sql.Tx, row adoptedTaskRow, adopted map[string]int64, plan *workAdoptionPlan,
) (bool, error) {
	var present int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_tasks WHERE id=?", row.id).Scan(&present); err != nil {
		return false, err
	}
	if present != 0 {
		// The work records have it, so they are the current state of it. The
		// ledger row is a copy nothing has written since the records moved.
		plan.Counts.TasksPresent++
		adopted[row.id] = row.version
		plan.dropped("sp_ledger_tasks:"+row.id, "the row",
			"the work records already hold this task and are what everything reads")
		return true, nil
	}
	var retired int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_retired WHERE id=?", row.id).Scan(&retired); err != nil {
		return false, err
	}
	if retired != 0 {
		// Deleted on the work side on purpose. Recreating it would revive a task
		// a person removed.
		plan.Counts.TasksRetiredSkipped++
		plan.dropped("sp_ledger_tasks:"+row.id, "the row", "this task was deleted after the records moved")
		return false, nil
	}
	createdAt, updatedAt, err := ledgerTaskTimes(ctx, tx, row.id)
	if err != nil {
		return false, err
	}
	plan.Counts.TasksCreated++
	adopted[row.id] = row.version
	plan.carried("sp_ledger_tasks:"+row.id, "the task", "a person's work, and the work records are where work lives now")
	if write == nil {
		return true, nil
	}
	sessionID, _ := currentSessionOf(row.task)
	if _, err = write.ExecContext(ctx,
		"INSERT INTO sp_work_tasks ("+workTaskColumns+") VALUES (?,?,?,?,?,?,?,?,?)",
		row.id, sessionID, row.task.Title, nil, nil, row.status, row.version, createdAt, updatedAt); err != nil {
		return false, err
	}
	// Where the task came from is its own fact, recorded once by the only thing
	// that can know it.
	_, err = write.ExecContext(ctx,
		"INSERT INTO sp_work_task_origin (task_id,origin) VALUES (?,?) ON CONFLICT(task_id) DO NOTHING",
		row.id, workOriginLedger)
	return true, err
}

// adoptTaskBook carries the parent's live bookkeeping into the owned record. A
// record the parent has already written here wins outright: it wrote it after
// the records moved, so the ledger's copy is the stale one.
func adoptTaskBook(ctx context.Context, tx *sql.Tx, write *sql.Tx, row adoptedTaskRow, plan *workAdoptionPlan) error {
	var present int
	if err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_task_book WHERE task_id=?", row.id).Scan(&present); err != nil {
		return err
	}
	if present != 0 {
		plan.Counts.BookPresent++
		plan.dropped("sp_ledger_tasks:"+row.id, "stage, owner, sessions, details",
			"the parent has written this task's bookkeeping in the owned record since")
		return nil
	}
	relations, droppedRelations := liveRelations(row.task.Sessions)
	details, droppedKeys, unclassified := liveDetails(row.task.Details)
	for _, name := range droppedRelations {
		plan.Counts.RelationsDropped++
		plan.dropped("sp_work_task_book:"+row.id, "session "+name,
			"that session is gone, so its cursor cannot be resumed from")
	}
	for _, key := range droppedKeys {
		plan.Counts.DetailKeysDropped++
		plan.dropped("sp_work_task_book:"+row.id, "details."+key, workPastDetailKeys[key])
	}
	for _, key := range unclassified {
		plan.carried("sp_work_task_book:"+row.id, "details."+key,
			"not a key this image knows; carried rather than guessed away")
	}
	plan.Counts.BookCarried++
	plan.carried("sp_work_task_book:"+row.id, "stage, owner, live sessions, current details",
		"the parent reads and writes this every run")
	if write == nil {
		return nil
	}
	encodedRelations, err := json.Marshal(relations)
	if err != nil {
		return err
	}
	encodedDetails, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = write.ExecContext(ctx,
		`INSERT INTO sp_work_task_book (task_id,stage,owner,sessions,details) VALUES (?,?,?,?,?)
		 ON CONFLICT(task_id) DO NOTHING`,
		row.id, row.task.Stage, row.task.Owner, string(encodedRelations), string(encodedDetails))
	return err
}

// liveRelations keeps the session relations a parent can still act on. A
// relation whose session has been deleted has a cursor into something that is
// not there any more; keeping it is keeping a pointer to nothing.
func liveRelations(all []workBookSession) ([]workBookSession, []string) {
	live := []workBookSession{}
	dropped := []string{}
	for _, relation := range all {
		if strings.ToLower(strings.TrimSpace(relation.Cleanup)) == "deleted" {
			dropped = append(dropped, relation.ID)
			continue
		}
		live = append(live, relation)
	}
	return live, dropped
}

// liveDetails splits the details JSON by what still has a consumer.
//
// The third return is the keys this image has no judgement about. They are
// carried, and that is deliberately not a loophole in "only what is needed":
// dropping a key nothing here recognises is the one mistake that cannot be
// noticed from the outside — the record would simply be missing state nobody
// knew to look for. So an unrecognised key is carried *and named*, which is
// what the rehearsal is for, and the answer to a key that keeps showing up
// there is to classify it from its consumer rather than to widen the default.
//
// That is what happened to the lifecycle keys: they were reaching the record
// through this path, so nothing was lost, but every ordinary row reported them
// as unrecognised. Reading the managing skillset settled each one, and the
// catch-all went back to being the narrow safety net it is meant to be.
func liveDetails(details map[string]any) (map[string]any, []string, []string) {
	live := map[string]any{}
	dropped := []string{}
	unclassified := []string{}
	for key, value := range details {
		lower := strings.ToLower(strings.TrimSpace(key))
		if _, past := workPastDetailKeys[lower]; past {
			dropped = append(dropped, lower)
			continue
		}
		live[key] = value
		if _, known := workLiveDetailKeys[lower]; !known {
			unclassified = append(unclassified, lower)
		}
	}
	sort.Strings(dropped)
	sort.Strings(unclassified)
	return live, dropped, unclassified
}

// adoptMonitor carries one monitoring loop into its own record, or establishes
// that it is finished and goes. A loop the ledger recorded as completed will not
// run again; a record of it would be exactly the copy nothing reads that this
// cleanup exists to remove.
func adoptMonitor(ctx context.Context, write *sql.Tx, row adoptedTaskRow, plan *workAdoptionPlan, now time.Time) error {
	if row.status == "completed" {
		plan.Counts.MonitorsDropped++
		plan.dropped("sp_ledger_tasks:"+row.id, "the monitoring row", "this loop finished; nothing reads it again")
		return nil
	}
	plan.Counts.MonitorsCarried++
	plan.carried("sp_work_monitor:"+row.id, "the monitoring row", "the parent's loop is still running")
	// The ledger row carried an owner and session relations. The monitoring
	// record has neither on purpose — a loop is this parent's own, and the
	// relations were a convention in a table that had no column for a loop at
	// all. They are named here rather than vanishing, because "nothing is lost"
	// is a claim a reader checks key by key.
	if strings.TrimSpace(row.task.Owner) != "" {
		plan.dropped("sp_work_monitor:"+row.id, "owner",
			"a monitoring loop is the parent's own; there is no second owner to record")
	}
	if len(row.task.Sessions) != 0 {
		plan.dropped("sp_work_monitor:"+row.id, fmt.Sprintf("%d session relation(s)", len(row.task.Sessions)),
			"a loop is not assigned to a session the way work is; the sessions it watches are recomputed each run")
	}
	state := monitorStateRunning
	if row.status != "running" {
		state = monitorStateStopped
	}
	note, err := json.Marshal(monitorNoteOf(row.task.Details))
	if err != nil {
		return err
	}
	text := string(note)
	if text == "{}" {
		text = ""
	}
	carried, changed := adoptableMonitor(workMonitor{
		ID: row.id, Title: row.task.Title, State: state, Note: text, UpdatedAt: workNow(now),
	})
	for _, why := range changed {
		plan.dropped("sp_work_monitor:"+row.id, "the record as it stood", why)
	}
	if write == nil {
		return nil
	}
	return adoptMonitorRow(ctx, write, carried)
}

// monitorNoteOf is what a monitoring row's details still say, minus the
// convention that marked it one. The kind column exists now, so carrying the
// marker would carry the workaround with it.
func monitorNoteOf(details map[string]any) map[string]any {
	note := map[string]any{}
	for key, value := range details {
		if strings.ToLower(strings.TrimSpace(key)) == "kind" {
			continue
		}
		note[key] = value
	}
	return note
}

// adoptRequests turns every request a person is still waiting on into a turn,
// and accounts for the ones that are done. A settled request is not carried:
// what the parent told the person is what the person already read, and a second
// copy of a finished exchange is the duplication this cleanup removes.
func adoptRequests(
	ctx context.Context, tx *sql.Tx, write *sql.Tx, adopted map[string]int64, plan *workAdoptionPlan, now time.Time,
) error {
	requests, err := readLedgerRequests(ctx, tx)
	if err != nil {
		return err
	}
	stranded := []string{}
	for _, request := range requests {
		plan.Counts.LedgerRequests++
		open := adoptRequestIsOpen(request.status)
		_, owned := adopted[request.taskID]
		if !owned {
			if open {
				// Nobody can answer it and there is no turn for it to become.
				// Stopping is the honest answer: dropping it would delete a
				// question a person is still waiting on.
				stranded = append(stranded, request.id+" (task "+request.taskID+", "+request.status+")")
				continue
			}
			plan.Counts.RequestsDropped++
			plan.dropped("sp_ledger_user_requests:"+request.id, "the row",
				"settled, and its task is not a work task")
			continue
		}
		mapped, err := turnSourceTaken(ctx, tx, request.id)
		if err != nil {
			return err
		}
		if mapped {
			plan.Counts.TurnsPresent++
			continue
		}
		if !open {
			plan.Counts.RequestsDropped++
			plan.dropped("sp_ledger_user_requests:"+request.id, "the row",
				"settled: the person was told the outcome at the time, and the task's turns are its history")
			continue
		}
		plan.Counts.TurnsCreated++
		plan.carried("sp_work_turns:"+request.taskID, "request "+request.id,
			"a person is still waiting on this: "+request.status)
		if write == nil {
			continue
		}
		if err = insertAdoptedTurn(ctx, write, request); err != nil {
			return err
		}
	}
	if len(stranded) == 0 {
		return nil
	}
	sort.Strings(stranded)
	named := stranded
	if len(named) > workAdoptReported {
		named = named[:workAdoptReported]
	}
	plan.Refusals = append(plan.Refusals, fmt.Sprintf(
		"%d request(s) nobody has dealt with belong to a task the work records do not have, so there is no turn for "+
			"them to become and no parent to answer them: %s. Settle them (or create the task with task_put) and run "+
			"again — they are not deleted.", len(stranded), strings.Join(named, ", ")))
	return nil
}

// insertAdoptedTurn writes one turn from one open request, and beside it the
// facts a turn has no column for but something still needs: where it came from,
// the id it was stored under, who the words were from, and the outside action
// the parent had already recorded against it.
func insertAdoptedTurn(ctx context.Context, tx *sql.Tx, request adoptedRequestRow) error {
	// The ledger's `result` is the sentence the parent wrote for the person,
	// which is this turn's explanation. It is explicitly not the response: the
	// session's own final words were never stored, so claiming these are them
	// would put the parent's summary in the session's mouth. An open request
	// rarely has one, and when it does (a reconciling request) it is what the
	// person was last told.
	var explanation *string
	if strings.TrimSpace(request.result) != "" {
		stored := request.result
		explanation = &stored
	}
	var sessionSeq *int64
	if request.relatedSeq.Valid {
		seq := request.relatedSeq.Int64
		sessionSeq = &seq
	}
	var startedAt *string
	if strings.TrimSpace(request.dispatchedAt) != "" {
		stamp := request.dispatchedAt
		startedAt = &stamp
	}
	// ended_at stays null however the row looked: this request is open, and a
	// turn with an ended_at is one nobody is waiting on.
	result, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_turns
		 (task_id,session_seq,request,instruction,response,explanation,question,choices,artifacts,requested_at,started_at,ended_at)
		 VALUES (?,?,?,NULL,NULL,?,NULL,'[]','[]',?,?,NULL)`,
		request.taskID, sessionSeq, request.text, explanation, request.createdAt, startedAt)
	if err != nil {
		return err
	}
	turnID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if err = recordTurnOrigin(ctx, tx, turnID, workTurnOriginUserRequest, request.id, request.author); err != nil {
		return err
	}
	if strings.TrimSpace(request.operationKey) == "" {
		return nil
	}
	// An outside action was recorded against this request. Carrying it keeps the
	// parent's reconciliation working: without it, a resend after a crash would
	// look like a first send.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_turn_admin (turn_id,operation_key,operation_target,dispatch_state,dispatched_at)
		 VALUES (?,?,?,?,?) ON CONFLICT(turn_id) DO NOTHING`,
		turnID, request.operationKey, request.operationTarget, request.dispatchState, request.dispatchedAt)
	return err
}

// adoptChoices puts a task's still-offered choices on its latest turn, which is
// the turn they are about. A task with no turn has nowhere to hold them:
// creating one would invent an exchange that never happened, so they go and the
// report says so — the session writes its choices again on the next turn.
func adoptChoices(ctx context.Context, tx *sql.Tx, write *sql.Tx, adopted map[string]int64, plan *workAdoptionPlan) error {
	ids := make([]string, 0, len(adopted))
	for taskID := range adopted {
		ids = append(ids, taskID)
	}
	sort.Strings(ids)
	for _, taskID := range ids {
		choices, err := liveLedgerChoices(ctx, tx, taskID, adopted[taskID])
		if err != nil {
			return err
		}
		if len(choices) == 0 {
			continue
		}
		var latest sql.NullInt64
		if err = tx.QueryRowContext(ctx,
			"SELECT max(id) FROM sp_work_turns WHERE task_id=?", taskID).Scan(&latest); err != nil {
			return err
		}
		if !latest.Valid {
			plan.Counts.ChoicesDropped++
			plan.dropped("sp_ledger_task_choices:"+taskID, "the offered choices",
				"this task has no turn to offer them on; the session writes them again on its next turn")
			continue
		}
		plan.Counts.ChoicesAttached++
		plan.carried("sp_work_turns:"+taskID, "the offered choices", "the session is still offering these")
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

// adoptRetiredIDs carries the ids the ledger retired. A retired id is not
// residue: it is what makes a later create under the same id a conflict rather
// than a revival of something a person deleted.
func adoptRetiredIDs(ctx context.Context, tx *sql.Tx, write *sql.Tx, plan *workAdoptionPlan) error {
	present, err := workTableExists(ctx, tx, "sp_ledger_retired")
	if err != nil || !present {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM sp_ledger_retired WHERE id NOT IN (SELECT id FROM sp_work_retired) ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		plan.Counts.RetiredIDsCarried++
		plan.carried("sp_work_retired:"+id, "the retired id", "it keeps a deleted task from being created again")
		if write == nil {
			continue
		}
		if _, err := write.ExecContext(ctx,
			"INSERT INTO sp_work_retired (id) VALUES (?) ON CONFLICT(id) DO NOTHING", id); err != nil {
			return err
		}
	}
	return nil
}

// countLedgerLeftovers counts what goes with the tables and names what holds it
// now. The history is the big one: it is the only thing here that was ever a
// person's to read, and what replaces it is the task's turns.
func countLedgerLeftovers(ctx context.Context, tx *sql.Tx, plan *workAdoptionPlan) error {
	for table, counter := range map[string]*int{
		"sp_ledger_task_events": &plan.Counts.EventsDropped,
		"sp_ledger_requests":    &plan.Counts.ReceiptsDropped,
	} {
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
		*counter = count
	}
	if plan.Counts.EventsDropped != 0 {
		plan.dropped("sp_ledger_task_events", fmt.Sprintf("%d row(s)", plan.Counts.EventsDropped),
			"past management events are not turns, and a task's history from here is its turns")
	}
	if plan.Counts.ReceiptsDropped != 0 {
		plan.dropped("sp_ledger_requests", fmt.Sprintf("%d row(s)", plan.Counts.ReceiptsDropped),
			"replay receipts for tools that are gone")
	}
	return nil
}

// verifyAdoption is the accounting check. Every ledger task is a task, a
// monitoring row, a skip or an already-present row; every request became a
// turn, was already one, or was dropped as settled. A row that is in none of
// those would be one this walk never looked at, and dropping the table then
// would lose it.
func verifyAdoption(ctx context.Context, tx *sql.Tx, plan *workAdoptionPlan) error {
	var orphans int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sp_work_turns t LEFT JOIN sp_work_tasks k ON t.task_id=k.id WHERE k.id IS NULL`).
		Scan(&orphans); err != nil {
		return err
	}
	counts := plan.Counts
	plan.Checks["orphan_turns"] = orphans
	plan.Checks["ledger_tasks_accounted"] = counts.TasksCreated+counts.TasksPresent+counts.TasksRetiredSkipped+
		counts.MonitorsCarried+counts.MonitorsDropped == counts.LedgerTasks
	plan.Checks["ledger_requests_accounted"] = counts.TurnsCreated+counts.TurnsPresent+counts.RequestsDropped+
		len(plan.Refusals) == counts.LedgerRequests || len(plan.Refusals) != 0
	if orphans != 0 {
		return workFail("storage_error", "this would leave turns with no task; nothing was changed")
	}
	if plan.Checks["ledger_tasks_accounted"] != true {
		return workFail("storage_error", "every ledger task could not be accounted for; nothing was changed")
	}
	return nil
}

func readLedgerTasks(ctx context.Context, tx *sql.Tx) ([]adoptedTaskRow, error) {
	present, err := workTableExists(ctx, tx, "sp_ledger_tasks")
	if err != nil || !present {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,version,status,document FROM sp_ledger_tasks ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []adoptedTaskRow{}
	for rows.Next() {
		var row adoptedTaskRow
		var document string
		if err := rows.Scan(&row.id, &row.version, &row.status, &document); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(document), &row.task); err != nil {
			return nil, workFail("storage_error", "a ledger task's document could not be read; nothing was changed")
		}
		if row.task.Details == nil {
			row.task.Details = map[string]any{}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func readLedgerRequests(ctx context.Context, tx *sql.Tx) ([]adoptedRequestRow, error) {
	present, err := workTableExists(ctx, tx, "sp_ledger_user_requests")
	if err != nil || !present {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id,task_id,text,author,created_at,status,
		        operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at
		 FROM sp_ledger_user_requests ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []adoptedRequestRow{}
	for rows.Next() {
		var row adoptedRequestRow
		if err := rows.Scan(&row.id, &row.taskID, &row.text, &row.author, &row.createdAt, &row.status,
			&row.operationKey, &row.operationTarget, &row.dispatchState, &row.dispatchedAt,
			&row.relatedSeq, &row.result, &row.settledAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ledgerTaskTimes are the only task times the ledger ever recorded: the history
// rows written with the change they describe. They are read here, before the
// history goes, so a task keeps the created_at it really had — a time invented
// from this run's own clock would say a person's task started today.
func ledgerTaskTimes(ctx context.Context, tx *sql.Tx, taskID string) (*string, *string, error) {
	present, err := workTableExists(ctx, tx, "sp_ledger_task_events")
	if err != nil || !present {
		return nil, nil, err
	}
	var created, updated sql.NullString
	if err = tx.QueryRowContext(ctx,
		`SELECT (SELECT at FROM sp_ledger_task_events WHERE task_id=? AND kind='task_created' ORDER BY seq LIMIT 1),
		        (SELECT at FROM sp_ledger_task_events WHERE task_id=? ORDER BY seq DESC LIMIT 1)`,
		taskID, taskID).Scan(&created, &updated); err != nil {
		return nil, nil, err
	}
	return nullableString(created), nullableString(updated), nil
}

// liveLedgerChoices are the choices the session still offers on a task: the ones
// written against the version the task is at now. Anything older was already
// not shown to anybody.
func liveLedgerChoices(ctx context.Context, tx *sql.Tx, taskID string, version int64) ([]workChoice, error) {
	present, err := workTableExists(ctx, tx, "sp_ledger_task_choices")
	if err != nil || !present {
		return nil, err
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

// currentSessionOf picks the one session an adopted task is assigned to now. A
// ledger task collected relations as it went, so the assignment is the latest
// relation the parent still owns and has not finished with. No relation that
// fits means unassigned, which is null rather than a guess.
func currentSessionOf(task adoptedTask) (*string, string) {
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

func detailsKindOf(details map[string]any) string {
	if details == nil {
		return ""
	}
	kind, _ := details["kind"].(string)
	return strings.ToLower(strings.TrimSpace(kind))
}

// readMigrationSteps is the step log an older image's migration wrote, or
// nothing once retirement has folded it into the retirement record and dropped
// the table.
func readMigrationSteps(ctx context.Context, tx *sql.Tx) ([]map[string]any, error) {
	steps := []map[string]any{}
	present, err := workTableExists(ctx, tx, "sp_work_migration")
	if err != nil || !present {
		return steps, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT seq,at,step,state,detail FROM sp_work_migration ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	return steps, rows.Err()
}

// readRetirementSteps is what the move recorded: its snapshot, its fence, what
// it carried and dropped, and the migration steps it folded in.
func readRetirementSteps(ctx context.Context, tx *sql.Tx) ([]map[string]any, error) {
	steps := []map[string]any{}
	present, err := workTableExists(ctx, tx, "sp_work_retirement")
	if err != nil || !present {
		return steps, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT seq,at,step,detail FROM sp_work_retirement ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var at, step, detail string
		if err := rows.Scan(&seq, &at, &step, &detail); err != nil {
			return nil, err
		}
		entry := map[string]any{"seq": seq, "at": at, "step": step}
		var decoded any
		if json.Unmarshal([]byte(detail), &decoded) == nil {
			entry["detail"] = decoded
		}
		steps = append(steps, entry)
	}
	return steps, rows.Err()
}

func (s *sessionDB) workStateReportOf(ctx context.Context, db *sql.DB) (map[string]any, error) {
	var report map[string]any
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		result, readErr := workStateReport(ctx, tx)
		if readErr != nil {
			return readErr
		}
		report = result.(map[string]any)
		return nil
	})
	return report, err
}

// workStateReport is where this slot's records stand and what was recorded on
// the way here: every step with its own detail, and the counts as they are now,
// so a reader re-checks rather than trusting a run's own word. It outlives the
// move — a parent has to know which records this slot keeps before it touches
// any of them, and after the move the answer is still a fact rather than an
// assumption.
func workStateReport(ctx context.Context, tx *sql.Tx) (any, error) {
	state, err := workState(ctx, tx)
	if err != nil {
		return nil, err
	}
	version := workSchemaVersion
	if err = tx.QueryRowContext(ctx, "SELECT version FROM sp_work_meta").Scan(&version); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	queries := map[string]string{
		"work_tasks":          "SELECT count(*) FROM sp_work_tasks",
		"work_turns":          "SELECT count(*) FROM sp_work_turns",
		"open_turns":          "SELECT count(*) FROM sp_work_turns WHERE request IS NOT NULL AND ended_at IS NULL",
		"bookkeeping_records": "SELECT count(*) FROM sp_work_task_book",
		"monitors":            "SELECT count(*) FROM sp_work_monitor",
		"tasks_from_ledger":   "SELECT count(*) FROM sp_work_task_origin WHERE origin='" + workOriginLedger + "'",
		"turn_origins":        "SELECT count(*) FROM sp_work_turn_origin",
		"retired_ids":         "SELECT count(*) FROM sp_work_retired",
	}
	// The tables the move removes are counted only while they are there: a count
	// of a table that is gone would be an error rather than the zero it means,
	// and their absence is itself part of the answer.
	for _, table := range workRetiredTables {
		present, existsErr := workTableExists(ctx, tx, table)
		if existsErr != nil {
			return nil, existsErr
		}
		if present {
			queries[table] = "SELECT count(*) FROM " + table
		}
	}
	for name, query := range queries {
		var count int
		if err := tx.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	steps, err := readMigrationSteps(ctx, tx)
	if err != nil {
		return nil, err
	}
	report := map[string]any{"state": state, "schema_version": version, "counts": counts}
	if len(steps) != 0 {
		report["migration_steps"] = steps
	}
	retirement, err := readRetirementSteps(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(retirement) != 0 {
		report["retirement"] = retirement
	}
	if state == workStateRetired {
		report["removed_tools"] = workRetiredToolNames()
	}
	return report, nil
}
