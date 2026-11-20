// The work records of a chat slot: the tasks a person has, and the turns that
// carried each one. This is the schema the repository owner agreed on — a task
// row and a turn row, nothing else — and it is the only place work lives now.
//
// Nothing is smuggled into the two agreed entities: a reader of sp_work_tasks
// sees the agreed columns only. What a task and a turn deliberately do not
// carry — the parent's own bookkeeping, its monitoring loops, a turn's claim
// token and the outside action it recorded — sits in records of its own beside
// them, each named for what it is: sp_work_task_book
// (session_work_parent.go), sp_work_monitor (session_work_monitor.go),
// sp_work_turn_claim and sp_work_turn_admin.
//
// The ledger these records came from is gone, and so are the tables that held
// what a migration preserved verbatim beside each moved row (AC-N10,
// session_work_retire.go). sp_work_meta carries this namespace's version and
// the state of that one-way move, which is also the fence it holds while it
// runs.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// workSchema is split in two because retirement drops one half and keeps the
// other. workSchemaLive is what a slot keeps for good: the agreed records, the
// receipts and retired ids, the internal management counters and claim tokens,
// the parent's own bookkeeping, and where a record came from. workSchemaTransition
// is the machinery that moves a ledger into them, and a retired slot must not
// get it back — a CREATE TABLE IF NOT EXISTS over a retired database would
// resurrect the dropped tables as empty ones, which reads as "the transition is
// still here" to every tool that checks for them.
const workSchemaLive = `CREATE TABLE IF NOT EXISTS sp_work_meta (version INTEGER NOT NULL, state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sp_work_tasks (
	id TEXT PRIMARY KEY,
	session_id TEXT,
	title TEXT NOT NULL,
	description TEXT,
	completion_criteria TEXT,
	status TEXT NOT NULL,
	version INTEGER NOT NULL,
	created_at TEXT,
	updated_at TEXT
);
CREATE TABLE IF NOT EXISTS sp_work_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL REFERENCES sp_work_tasks(id),
	session_seq INTEGER,
	request TEXT,
	instruction TEXT,
	response TEXT,
	explanation TEXT,
	question TEXT,
	choices TEXT NOT NULL DEFAULT '[]',
	artifacts TEXT NOT NULL DEFAULT '[]',
	requested_at TEXT NOT NULL,
	started_at TEXT,
	ended_at TEXT
);
CREATE INDEX IF NOT EXISTS sp_work_turns_task ON sp_work_turns (task_id, id);
CREATE TABLE IF NOT EXISTS sp_work_receipts (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, response TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sp_work_retired (id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS sp_work_turn_admin (
	turn_id INTEGER PRIMARY KEY,
	operation_key TEXT NOT NULL,
	operation_target TEXT NOT NULL,
	dispatch_state TEXT NOT NULL,
	dispatched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_turn_claim (
	turn_id INTEGER PRIMARY KEY,
	claim_token TEXT NOT NULL,
	claimed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_task_guard (
	task_id TEXT PRIMARY KEY,
	turn_version INTEGER NOT NULL,
	book_version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_task_book (
	task_id TEXT PRIMARY KEY,
	stage TEXT NOT NULL,
	owner TEXT NOT NULL,
	sessions TEXT NOT NULL,
	details TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_task_origin (task_id TEXT PRIMARY KEY, origin TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sp_work_turn_origin (
	turn_id INTEGER PRIMARY KEY,
	origin TEXT NOT NULL,
	source_id TEXT NOT NULL,
	author TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS sp_work_turn_origin_source ON sp_work_turn_origin (origin, source_id);
CREATE TABLE IF NOT EXISTS sp_work_monitor (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	state TEXT NOT NULL,
	note TEXT NOT NULL,
	version INTEGER NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_retirement (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	step TEXT NOT NULL,
	detail TEXT NOT NULL
);`

// The tables this image no longer creates and retirement removes: the ledger
// this slot's records came from, the verbatim copies a migration kept beside
// each moved row, and the migration's own step log. They are named here because
// dropping them is the only thing left that touches them — every reader asks
// whether the table is there first (workTableExists), so a slot that was never
// migrated by an older image behaves the same as one that was.
var workRetiredTables = []string{
	"sp_work_task_legacy", "sp_work_turn_legacy", "sp_work_migration",
	"sp_ledger_task_choices", "sp_ledger_task_events", "sp_ledger_user_requests",
	"sp_ledger_requests", "sp_ledger_retired", "sp_ledger_tasks", "sp_ledger_meta",
}

const (
	// workSchemaVersion is the version a slot carries while it still has a
	// ledger behind it, and the lowest version this image reads. A database
	// written by a version this image does not know is refused rather than read
	// with the columns of this one.
	workSchemaVersion = 1
	// workSchemaVersionRetired is the version a slot carries once the ledger and
	// the move's leftovers are gone. A database at this version is one an image
	// that only knows version 1 must **not** serve: that image would recreate
	// the ledger tables it expects and serve an empty ledger as if the records
	// were lost. Writing the version makes it fail closed on the first call
	// (schema_version) instead. That is the one-way door retirement is: checked,
	// not assumed.
	workSchemaVersionRetired = 2

	// The states a slot can be in. ready means the ledger is still behind this
	// slot and its records have not been adopted yet; migrated is a slot an
	// older image moved and left the ledger standing beside; retiring is the
	// fence the one-way move holds; retired is a slot whose records are only in
	// the work records — which is also where a brand new slot starts, because
	// there is no ledger to adopt. rolled_back is the state an older image's
	// rollback export left behind: the ledger was authoritative again, so the
	// move starts from it (session_work_retire.go).
	workStateReady      = "ready"
	workStateMigrating  = "migrating"
	workStateMigrated   = "migrated"
	workStateRolledBack = "rolled_back"
)

const (
	// workTitleMax and the bounds below exist for the reason the ledger's do: a
	// slot's storage budget is shared with the session's own archive, so one
	// task cannot be an attachment.
	workTitleMax    = 512
	workTextMax     = 8192
	workTurnTextMax = 65536
	// workChoiceMax bounds one turn's offered choices. A person picks from a
	// short list or writes their own.
	workChoiceMax       = 6
	workChoiceLabelMax  = 80
	workChoiceTextMax   = 1000
	workArtifactMax     = 20
	workArtifactTextMax = 1024
	// workListLimitDefault/Max page tasks and turns the way the ledger pages.
	workListLimitDefault = 20
	workListLimitMax     = 100
	// workLegacySessionMax/BytesMax bound the parent's own bookkeeping beside a
	// task for the same reason the task's text is bounded.
	workLegacySessionMax = 64
	workLegacyBytesMax   = 64 << 10
	// workSnapshotNameTries bounds the search for a free snapshot name within
	// one second before the caller is asked to retry.
	workSnapshotNameTries = 50
)

// workStatuses is the task status domain. It is the ledger's own domain
// unchanged: a migration that renamed a status would move work a person had
// already seen as blocked or completed.
var workStatuses = []string{"planned", "running", "blocked", "review", "completed", "unknown"}

func isWorkStatus(s string) bool {
	for _, candidate := range workStatuses {
		if candidate == s {
			return true
		}
	}
	return false
}

// workTask is one task: who has it, what it is, how it will be known to be
// done, where it stands, and the version a writer has to hold to change it.
//
// A nil SessionID is "not assigned to a session"; nil CreatedAt/UpdatedAt is
// "not recorded", which is what a row migrated from a ledger that never kept
// task timestamps honestly says. Nothing else is nullable, and no field is
// added here — the parent's own bookkeeping lives in sp_work_task_legacy.
type workTask struct {
	ID                 string  `json:"id"`
	SessionID          *string `json:"session_id"`
	Title              string  `json:"title"`
	Description        *string `json:"description"`
	CompletionCriteria *string `json:"completion_criteria"`
	Status             string  `json:"status"`
	Version            int64   `json:"version"`
	CreatedAt          *string `json:"created_at"`
	UpdatedAt          *string `json:"updated_at"`
}

// workChoice is one sentence the parent offers for the next request. There is no
// id: the app sends the chosen request_text back as the next turn's request, and
// which turn it answers is what decides whether the pick is still current
// (session_work_turns.go).
type workChoice struct {
	Label       string `json:"label"`
	RequestText string `json:"request_text"`
}

// workTurn is one exchange on a task: the person's own words, the instruction
// the parent passed to the session that does the work, that session's final
// answer verbatim, and the parent's explanation of it. Request/Instruction/
// Response/Explanation are nullable because a turn is written as it happens —
// a turn the app has just appended has a request and nothing else yet — and
// because a migrated turn must be able to say "not recorded" instead of
// borrowing a summary written for something else.
type workTurn struct {
	ID          int64        `json:"id"`
	TaskID      string       `json:"task_id"`
	SessionSeq  *int64       `json:"session_seq"`
	Request     *string      `json:"request"`
	Instruction *string      `json:"instruction"`
	Response    *string      `json:"response"`
	Explanation *string      `json:"explanation"`
	Question    *string      `json:"question"`
	Choices     []workChoice `json:"choices"`
	Artifacts   []string     `json:"artifacts"`
	RequestedAt string       `json:"requested_at"`
	StartedAt   *string      `json:"started_at"`
	EndedAt     *string      `json:"ended_at"`
}

// workTaskInput is a task as a writer gives it. The pointers distinguish "leave
// this as it is" from "set it to nothing" on a patch; task_put is a full
// replacement and requires all of them.
type workTaskInput struct {
	SessionID          *string `json:"session_id,omitempty"`
	Title              *string `json:"title,omitempty"`
	Description        *string `json:"description,omitempty"`
	CompletionCriteria *string `json:"completion_criteria,omitempty"`
	Status             *string `json:"status,omitempty"`
}

// workTurnInput is a turn as a writer gives it. Every field is optional: the
// app appends a request, and the parent fills the rest of the same turn as the
// work moves, so a patch that names one field leaves the others alone.
type workTurnInput struct {
	SessionSeq  *int64        `json:"session_seq,omitempty"`
	Request     *string       `json:"request,omitempty"`
	Instruction *string       `json:"instruction,omitempty"`
	Response    *string       `json:"response,omitempty"`
	Explanation *string       `json:"explanation,omitempty"`
	Question    *string       `json:"question,omitempty"`
	Choices     []workChoice  `json:"choices,omitempty"`
	Artifacts   []string      `json:"artifacts,omitempty"`
	RequestedAt *string       `json:"requested_at,omitempty"`
	StartedAt   *string       `json:"started_at,omitempty"`
	EndedAt     *string       `json:"ended_at,omitempty"`
	clear       []string      `json:"-"`
	rawFields   []string      `json:"-"`
	rawChoices  *[]workChoice `json:"-"`
}

type workArgs struct {
	ID              string `json:"id,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	TurnID          *int64 `json:"turn_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	ExpectedVersion *int64 `json:"expected_version,omitempty"`
	// ExpectedTurnVersion and ExpectedBookVersion are the compare-and-swap
	// values for the two lanes that are not the task's own state: writes to its
	// turns, and the parent's bookkeeping beside it (session_work_guard.go).
	// They are internal management counters, not fields of the agreed records.
	ExpectedTurnVersion *int64         `json:"expected_turn_version,omitempty"`
	ExpectedBookVersion *int64         `json:"expected_book_version,omitempty"`
	Task                *workTaskInput `json:"task,omitempty"`
	Turn                *workTurnInput `json:"turn,omitempty"`
	SessionID           string         `json:"session_id,omitempty"`
	Status              string         `json:"status,omitempty"`
	AfterID             string         `json:"after_id,omitempty"`
	AfterTurnID         *int64         `json:"after_turn_id,omitempty"`
	Open                *bool          `json:"open,omitempty"`
	Limit               int            `json:"limit,omitempty"`
	OperationKey        string         `json:"operation_key,omitempty"`
	OperationTarget     string         `json:"operation_target,omitempty"`
	DispatchState       string         `json:"dispatch_state,omitempty"`
	Observed            string         `json:"observed,omitempty"`
	DryRun              *bool          `json:"dry_run,omitempty"`
	// Paths are the snapshot files a prune call names. They are the one
	// argument in this area that is not a record id: deleting a backup is a
	// file operation, and naming it exactly is what keeps it deliberate
	// (session_work_backups.go).
	Paths []string `json:"paths,omitempty"`
	// AllEligible deletes every snapshot the retention policy has released,
	// instead of naming them one by one (session_work_backups.go).
	AllEligible *bool `json:"all_eligible,omitempty"`
	// Legacy is the parent's own bookkeeping beside a task, not part of the
	// agreed task (session_work_parent.go).
	Legacy *workLegacyInput `json:"record,omitempty"`
	// Monitor is one of the parent's own monitoring loops, which is not a task
	// and never was (session_work_monitor.go).
	Monitor *workMonitorInput `json:"monitor,omitempty"`
	// ClaimToken proves the caller is the parent that took this turn. It is
	// internal management state, not a field of the agreed turn
	// (session_work_turns.go).
	ClaimToken string `json:"claim_token,omitempty"`
}

var workTools = []string{
	"task_get", "task_list", "task_put", "task_delete",
	"turn_append", "turn_patch", "turn_list", "turn_dispatch",
	// The parent's own bookkeeping beside a task, which is not part of the
	// agreed model and has a record of its own (session_work_parent.go).
	taskBookGetTool, taskBookPutTool,
	// The parent's monitoring loops, which are not a person's work and never
	// became tasks. They used to be rows in the ledger; they are a record of
	// their own now (session_work_monitor.go).
	monitorGetTool, monitorListTool, monitorPutTool, monitorDeleteTool,
	// Where this slot's records stand, and the snapshots the move left behind.
	// Both outlive the move: the first is what a parent branches on, the second
	// is what makes a retained backup deletable on purpose rather than kept for
	// ever (session_work_retire.go, session_work_backups.go).
	workStateTool, workBackupsTool, workBackupsPruneTool,
	// The one-way door itself: it adopts what this slot still needs and removes
	// everything the move leaves behind. A retired slot no longer lists it.
	workRetireTool,
}

// isWorkTransitionTool reports the tools a retired slot stops listing. Only the
// retirement itself is left: a slot that has nothing to retire offering to
// retire is the same confusion as a migrated slot offering to migrate. The
// refusal stays in place for a client holding an older list — a list is a hint
// and a guard is a guarantee.
func isWorkTransitionTool(name string) bool {
	return name == workRetireTool
}

// workRemovedToolSuccessor names what to call instead of a tool that is gone.
// The ledger tools, the migration and the rollback export are not in this image
// any more (AC-N10), and a parent that still calls one is told the name of the
// call that does the job rather than "unknown tool" — the same discipline the
// cutover's own refusals followed.
func workRemovedToolSuccessor(name string) string {
	switch name {
	case "ledger_get", "ledger_list":
		return "task_get, task_list and turn_list"
	case "ledger_put", "ledger_delete":
		return "task_put and task_delete, or " + monitorPutTool + "/" + monitorDeleteTool + " for a monitoring loop"
	case "ledger_choices_put", "ledger_choices_clear":
		return "turn_patch with choices"
	case "ledger_requests_list":
		return "turn_list with open=true"
	case "ledger_requests_claim", "ledger_requests_ack":
		return "turn_patch"
	case "ledger_requests_dispatch":
		return "turn_dispatch"
	case "ledger_events_list":
		return "turn_list — a task's turns are its history"
	case workLegacyGetTool:
		return taskBookGetTool
	case workLegacyPutTool:
		return taskBookPutTool
	case "work_migration_report":
		return workStateTool
	case "work_migrate", workRollbackTool:
		return workRetireTool + ", which is the one way this slot's records move now"
	default:
		return ""
	}
}

// workRollbackTool is a name this image no longer implements, kept only so a
// call to it is answered with its successor (workRemovedToolSuccessor). It is
// not in workTools, so it is neither listed nor callable. Rolling the image
// back is not a step any more: retirement is a one-way door and the version
// gate makes an older image fail closed rather than serve a ledger that no
// longer exists.
const workRollbackTool = "work_rollback_export"

func isRemovedWorkTool(name string) bool { return workRemovedToolSuccessor(name) != "" }

func isWorkTool(name string) bool {
	for _, candidate := range workTools {
		if candidate == name {
			return true
		}
	}
	return false
}

// workMutations are the tools whose answer is replayed from a receipt. A tool
// that is not here reads and is therefore safe to repeat.
func isWorkMutation(name string) bool {
	switch name {
	case "task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch",
		taskBookPutTool, monitorPutTool, monitorDeleteTool, workRetireTool, workBackupsPruneTool:
		return true
	}
	return false
}

// migrateWork creates this namespace transactionally on first use and refuses a
// version it was not written for. It never touches a user table, never changes
// PRAGMA user_version, and never repairs a collision destructively — a table
// named sp_work_* that this schema did not write is a refusal, not something to
// drop.
// A new slot is born with no ledger and nothing to adopt, so it starts in the
// terminal state rather than in `ready`: `ready` means "there are records in a
// ledger that have not moved yet", and saying that about a database that never
// had one would send a parent looking for a move to make. A database that does
// have ledger tables starts at `ready`, whatever this image thinks of them —
// that is the one case where there is something to adopt.
func migrateWork(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'sp_work_%'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.ExecContext(ctx, workSchemaLive); err != nil {
			return err
		}
		ledger, err := workTableExists(ctx, tx, "sp_ledger_tasks")
		if err != nil {
			return err
		}
		version, state := workSchemaVersionRetired, workStateRetired
		if ledger {
			version, state = workSchemaVersion, workStateReady
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO sp_work_meta VALUES (?,?)", version, state)
		return err
	}
	var version int
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT version,state FROM sp_work_meta").Scan(&version, &state); err != nil {
		return workFail("schema_version", "work namespace collision or invalid schema; no tables changed")
	}
	// Additive on purpose: a database created by an earlier version still gets
	// the tables a later additive change introduces, and CREATE TABLE IF NOT
	// EXISTS leaves the ones already there alone. The tables retirement drops
	// are never in this statement, so a retired slot cannot get them back as
	// empty ones — which would read as "the ledger is still here" to every
	// reader that checks.
	switch version {
	case workSchemaVersion, workSchemaVersionRetired:
		_, err := tx.ExecContext(ctx, workSchemaLive)
		return err
	default:
		return workFail("schema_version", "unsupported work schema; no downgrade performed")
	}
}

func workState(ctx context.Context, tx *sql.Tx) (string, error) {
	var state string
	err := tx.QueryRowContext(ctx, "SELECT state FROM sp_work_meta").Scan(&state)
	return state, err
}

func setWorkState(ctx context.Context, tx *sql.Tx, state string) error {
	_, err := tx.ExecContext(ctx, "UPDATE sp_work_meta SET state=?", state)
	return err
}

// requireWorkWritable refuses a write while the one-way move holds the fence.
// The move adopts what the ledger still holds and drops it in one transaction,
// so a write admitted in the middle would be adopted twice or not at all. It is
// a retry for the caller, not a loss.
func requireWorkWritable(ctx context.Context, tx *sql.Tx) error {
	state, err := workState(ctx, tx)
	if err != nil {
		return err
	}
	switch state {
	case workStateMigrating, workStateRetiring:
		return workFail("conflict", "the work records are being moved; retry when it has finished")
	}
	return nil
}

// workReceipt replays a mutation's first committed answer. The contract is the
// ledger's: the same request_id with the same arguments returns what was
// committed then, and the same id with different arguments is a conflict rather
// than a second action.
func workReceipt(ctx context.Context, tx *sql.Tx, name string, a workArgs) (json.RawMessage, string, error) {
	payload, err := json.Marshal(a)
	if err != nil {
		return nil, "", err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(append([]byte(name), payload...)))
	var stored, response string
	err = tx.QueryRowContext(ctx, "SELECT fingerprint,response FROM sp_work_receipts WHERE id=?", a.RequestID).
		Scan(&stored, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fingerprint, nil
	}
	if err != nil {
		return nil, "", err
	}
	if stored != fingerprint {
		return nil, "", workFail("conflict", "request_id already used with different arguments")
	}
	return json.RawMessage(response), fingerprint, nil
}

func storeWorkReceipt(ctx context.Context, tx *sql.Tx, requestID, fingerprint string, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sp_work_receipts VALUES (?,?,?)", requestID, fingerprint, string(encoded))
	return err
}

// work runs one tool in its own transaction, so a record and the receipt that
// makes it replayable commit together or not at all.
func (s *sessionDB) work(ctx context.Context, name string, a workArgs) (any, error) {
	if err := validateWork(name, a); err != nil {
		return nil, err
	}
	// Retirement takes a consistent snapshot, which cannot be taken inside a
	// transaction, and commits its fence before it adopts anything so that an
	// interrupted run is still fenced when the next one arrives. So it owns its
	// transactions rather than running in the one below.
	if name == workRetireTool {
		return s.runWorkRetire(ctx, a, workClock())
	}
	// The snapshot tools work on files beside the database rather than on rows
	// in it, so they are not inside the record transaction either.
	if name == workBackupsTool || name == workBackupsPruneTool {
		return s.runWorkBackups(ctx, name, a, workClock())
	}
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = migrateWork(ctx, tx); err != nil {
		return nil, err
	}
	if isWorkMutation(name) {
		if err = requireWorkWritable(ctx, tx); err != nil {
			return nil, err
		}
	}
	fingerprint := ""
	if isWorkMutation(name) {
		replay, print, receiptErr := workReceipt(ctx, tx, name, a)
		if receiptErr != nil {
			return nil, receiptErr
		}
		if replay != nil {
			return replay, tx.Commit()
		}
		fingerprint = print
	}
	now := workClock()
	var result any
	switch name {
	case "task_get":
		result, err = readWorkTask(ctx, tx, a.ID)
	case "task_list":
		result, err = listWorkTasks(ctx, tx, a)
	case "task_put":
		result, err = putWorkTask(ctx, tx, a, now)
	case "task_delete":
		result, err = deleteWorkTask(ctx, tx, a)
	case "turn_append":
		result, err = appendWorkTurn(ctx, tx, a, now)
	case "turn_patch":
		result, err = patchWorkTurn(ctx, tx, a, now)
	case "turn_list":
		result, err = listWorkTurns(ctx, tx, a)
	case "turn_dispatch":
		result, err = dispatchWorkTurn(ctx, tx, a, now)
	case workStateTool:
		result, err = workStateReport(ctx, tx)
	case taskBookGetTool:
		result, err = readWorkLegacy(ctx, tx, a.TaskID)
	case taskBookPutTool:
		result, err = putWorkLegacy(ctx, tx, a, now)
	case monitorGetTool, monitorListTool, monitorPutTool, monitorDeleteTool:
		result, err = runMonitorTool(ctx, tx, name, a, now)
	default:
		return nil, workFail("invalid_argument", "unknown work tool")
	}
	if err != nil {
		return nil, err
	}
	if fingerprint != "" {
		if err = storeWorkReceipt(ctx, tx, a.RequestID, fingerprint, result); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// callWork is the tools/call entry. Unknown and null-valued fields are refused
// before anything runs, so a caller that misspells a field learns that rather
// than having it silently ignored.
func (s *sessionDB) callWork(ctx context.Context, name string, raw json.RawMessage) (any, *jsonRPCError) {
	var fields map[string]json.RawMessage
	err := json.Unmarshal(raw, &fields)
	var a workArgs
	if err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		err = decoder.Decode(&a)
	}
	if err == nil {
		err = checkWorkFields(name, fields)
	}
	if err == nil && fields["turn"] != nil {
		err = readWorkTurnFields(&a, fields["turn"])
	}
	if err != nil {
		return sessionDBToolResult(workError{"invalid_argument", "invalid work arguments"}, true), nil
	}
	ctx, cancel := context.WithTimeout(ctx, sessionDBCallTimeout)
	defer cancel()
	result, err := s.work(ctx, name, a)
	if err != nil {
		var known workError
		if !errors.As(err, &known) {
			known = workError{"storage_error", "work storage operation failed; whether it committed is unknown"}
		}
		return sessionDBToolResult(known, true), nil
	}
	return sessionDBToolResult(result, false), nil
}

func checkWorkFields(name string, fields map[string]json.RawMessage) error {
	for _, definition := range workToolDefinitions() {
		if definition["name"] != name {
			continue
		}
		schema := definition["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		for key, value := range fields {
			if _, ok := properties[key]; !ok {
				return errors.New("unexpected field " + key)
			}
			if string(value) == "null" {
				return errors.New("field " + key + " must not be null")
			}
		}
		for _, key := range schema["required"].([]string) {
			if _, ok := fields[key]; !ok {
				return errors.New("missing required field " + key)
			}
		}
		return nil
	}
	return errors.New("unknown work tool")
}

// readWorkTurnFields records which turn fields the caller actually named, which
// is what tells a patch "set this to nothing" (an explicit empty string) from
// "leave it alone" (absent). Without it a patch that fills a response would
// erase the question beside it.
func readWorkTurnFields(a *workArgs, raw json.RawMessage) error {
	var named map[string]json.RawMessage
	if err := json.Unmarshal(raw, &named); err != nil {
		return err
	}
	if a.Turn == nil {
		a.Turn = &workTurnInput{}
	}
	allowed := map[string]bool{
		"session_seq": true, "request": true, "instruction": true, "response": true,
		"explanation": true, "question": true, "choices": true, "artifacts": true,
		"requested_at": true, "started_at": true, "ended_at": true,
	}
	for key, value := range named {
		if !allowed[key] {
			return errors.New("unexpected turn field " + key)
		}
		a.Turn.rawFields = append(a.Turn.rawFields, key)
		if string(value) == "null" {
			a.Turn.clear = append(a.Turn.clear, key)
		}
	}
	if raw, ok := named["choices"]; ok && string(raw) != "null" {
		var choices []workChoice
		if err := json.Unmarshal(raw, &choices); err != nil {
			return err
		}
		a.Turn.rawChoices = &choices
	}
	return nil
}

func (t *workTurnInput) named(field string) bool {
	for _, candidate := range t.rawFields {
		if candidate == field {
			return true
		}
	}
	return false
}

func (t *workTurnInput) cleared(field string) bool {
	for _, candidate := range t.clear {
		if candidate == field {
			return true
		}
	}
	return false
}

func workNow(now time.Time) string { return now.UTC().Format(time.RFC3339) }

func recordWorkMigrationStep(ctx context.Context, tx *sql.Tx, step, state string, detail any, now time.Time) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO sp_work_migration (at,step,state,detail) VALUES (?,?,?,?)",
		workNow(now), step, state, string(encoded))
	return err
}
