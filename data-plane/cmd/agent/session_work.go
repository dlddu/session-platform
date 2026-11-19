// The work records of a chat slot: the tasks a person has, and the turns that
// carried each one. This is the schema the repository owner agreed on — a task
// row and a turn row, nothing else — and it is deliberately narrower than the
// ledger beside it (session_ledger.go).
//
// Why a second namespace rather than a reshaped sp_ledger_tasks: the ledger
// document carries the parent's own bookkeeping (approval scope, session
// cursors and seqs, preservation state, monitor rows) and that does not fit in
// a task and a turn. Dropping it to fit the agreed shape would lose records the
// parent still acts on, so the ledger tables stay exactly as they are and the
// parts of a migrated row that have no column here are preserved verbatim in
// sp_work_task_legacy / sp_work_turn_legacy. Nothing new is smuggled into the
// two agreed entities: a reader of sp_work_tasks sees the agreed columns only.
//
// sp_ledger_meta stays at version 1 for the reason the request and choice
// tables kept it there: these tables are additive, so a rollback to an image
// that never heard of them still opens the database and still serves the
// ledger. sp_work_meta carries this namespace's own version, and its state is
// the fence the migration holds (session_work_migrate.go).
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

const workSchema = `CREATE TABLE IF NOT EXISTS sp_work_meta (version INTEGER NOT NULL, state TEXT NOT NULL);
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
CREATE TABLE IF NOT EXISTS sp_work_task_legacy (
	task_id TEXT PRIMARY KEY,
	legacy_version INTEGER NOT NULL,
	legacy_status TEXT NOT NULL,
	kind TEXT NOT NULL,
	stage TEXT NOT NULL,
	owner TEXT NOT NULL,
	sessions TEXT NOT NULL,
	details TEXT NOT NULL,
	provenance TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_turn_legacy (
	turn_id INTEGER PRIMARY KEY,
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	source_seq INTEGER,
	claim_state TEXT NOT NULL,
	admin TEXT NOT NULL,
	provenance TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS sp_work_turn_legacy_source ON sp_work_turn_legacy (source, source_id);
CREATE TABLE IF NOT EXISTS sp_work_turn_admin (
	turn_id INTEGER PRIMARY KEY,
	operation_key TEXT NOT NULL,
	operation_target TEXT NOT NULL,
	dispatch_state TEXT NOT NULL,
	dispatched_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_task_guard (
	task_id TEXT PRIMARY KEY,
	turn_version INTEGER NOT NULL,
	book_version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sp_work_migration (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	step TEXT NOT NULL,
	state TEXT NOT NULL,
	detail TEXT NOT NULL
);`

const (
	// workSchemaVersion is this namespace's own version. A database written by a
	// later version is refused rather than read with the columns of this one.
	workSchemaVersion = 1

	// The fence the migration holds. A write refused with workStateMigrating is
	// a retry for the caller, not a loss: the migration is one transaction and
	// takes the ledger with it, so admitting a write in the middle would copy a
	// row the source no longer has.
	workStateReady     = "ready"
	workStateMigrating = "migrating"
	workStateMigrated  = "migrated"
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
	// Legacy is the parent's own bookkeeping beside a task, not part of the
	// agreed task (session_work_parent.go).
	Legacy *workLegacyInput `json:"record,omitempty"`
}

var workTools = []string{
	"task_get", "task_list", "task_put", "task_delete",
	"turn_append", "turn_patch", "turn_list", "turn_dispatch",
	"work_migrate", "work_migration_report",
	// The parent's own bookkeeping beside a task, last because it is not part
	// of the agreed model (session_work_parent.go).
	workLegacyGetTool, workLegacyPutTool,
	workRollbackTool,
}

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
	case "task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch", "work_migrate",
		workLegacyPutTool, workRollbackTool:
		return true
	}
	return false
}

// migrateWork creates this namespace transactionally on first use and refuses a
// version it was not written for. It never touches a user table, never changes
// PRAGMA user_version, and never repairs a collision destructively — a table
// named sp_work_* that this schema did not write is a refusal, not something to
// drop.
func migrateWork(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'sp_work_%'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.ExecContext(ctx, workSchema); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO sp_work_meta VALUES (?,?)", workSchemaVersion, workStateReady)
		return err
	}
	var version int
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT version,state FROM sp_work_meta").Scan(&version, &state); err != nil {
		return ledgerFail("schema_version", "work namespace collision or invalid schema; no tables changed")
	}
	if version != workSchemaVersion {
		return ledgerFail("schema_version", "unsupported work schema; no downgrade performed")
	}
	// Additive on purpose: a database created by this version still gets the
	// tables a later additive change introduces, and CREATE TABLE IF NOT EXISTS
	// leaves the ones already there alone.
	_, err := tx.ExecContext(ctx, workSchema)
	return err
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

// requireWorkWritable refuses a write while the migration holds the fence. The
// migration copies the ledger in one transaction, so a write admitted in the
// middle would either be copied twice or not at all.
func requireWorkWritable(ctx context.Context, tx *sql.Tx) error {
	state, err := workState(ctx, tx)
	if err != nil {
		return err
	}
	if state == workStateMigrating {
		return ledgerFail("conflict", "the work migration holds this database; retry when it has finished")
	}
	if state == workStateRolledBack {
		// A rollback has been prepared, so the ledger is authoritative and the
		// older image may already be writing it. A work write now would be the
		// divergence the export exists to prevent.
		return ledgerFail("conflict",
			"a rollback export has been taken: the ledger is authoritative while this slot is rolled_back, "+
				"so write it with the ledger tools — work_migrate brings this slot forward again")
	}
	return nil
}

// legacyLedgerWrites are the ledger tools that change a record. Reads are not
// here: a migrated slot keeps its ledger readable, because the monitor rows and
// the task history the parent still works from live only there.
func isLegacyLedgerWrite(name string) bool {
	switch name {
	case "ledger_put", "ledger_delete", "ledger_choices_put", "ledger_choices_clear",
		"ledger_requests_claim", "ledger_requests_dispatch", "ledger_requests_ack":
		return true
	}
	return false
}

// guardLegacyLedgerWrite stops the two records from diverging. While the
// migration runs, a ledger write would be copied twice or not at all; once it
// has run, a ledger write to a row the work records own would change something
// nothing reads any more, and the same work would then exist in two places with
// different answers.
//
// It refuses only where that divergence is possible. A preserved row the work
// records do not own — a monitor row, which is the parent's own bookkeeping and
// was never a person's work — stays writable through the tools it always had:
// nothing in the work records reads it, so there is nothing to diverge from, and
// refusing it would leave the parent unable to record that its own loops ran.
// Creating a *new* ledger task stays refused either way, because the app reads
// the work records and a task written to the ledger now would be work nobody can
// see (session_work_parent.go).
func guardLegacyLedgerWrite(ctx context.Context, tx *sql.Tx, name string, a ledgerArgs) error {
	if !isLegacyLedgerWrite(name) {
		return nil
	}
	state, err := workState(ctx, tx)
	if err != nil {
		return err
	}
	switch state {
	case workStateMigrating:
		return ledgerFail("conflict", "the work migration holds this database; retry when it has finished")
	case workStateMigrated:
		if legacyLedgerOwnedWrite(name) {
			return ledgerFail("conflict",
				"choices and user requests belong to the turn records now; use turn_append, turn_patch and turn_dispatch — "+
					name+" would write a row nothing reads")
		}
		if !legacyLedgerTaskWrite(name) {
			return ledgerFail("conflict",
				"this slot's tasks moved to the work records; "+name+" would write a row nothing reads")
		}
		owned, err := legacyWriteTargetOwned(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if owned {
			return ledgerFail("conflict",
				"that task moved to the work records; use task_put and turn_* for it, and work_legacy_put for its "+
					"approval, cursor and preservation bookkeeping — "+name+" would write a row nothing reads")
		}
		var preserved int
		if err = tx.QueryRowContext(ctx,
			"SELECT count(*) FROM sp_ledger_tasks WHERE id=?", a.ID).Scan(&preserved); err != nil {
			return err
		}
		if preserved == 0 {
			return ledgerFail("conflict",
				"a new task goes in the work records now; use task_put — a task written to the ledger "+
					"after the cutover would be work nothing reads")
		}
	}
	return nil
}

// legacyWriteTargetOwned reports whether the work records own the task a legacy
// write names.
func legacyWriteTargetOwned(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	_, owned, err := workTaskVersionOf(ctx, tx, taskID)
	return owned, err
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
		return nil, "", ledgerFail("conflict", "request_id already used with different arguments")
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
	// The migration manages its own transactions: a consistent snapshot cannot
	// be taken inside one, and its fence has to be committed before the copy so
	// that an interrupted copy is still fenced when the next run arrives.
	if name == "work_migrate" {
		return s.runWorkMigration(ctx, a, ledgerNow())
	}
	if name == workRollbackTool {
		return s.runWorkRollback(ctx, a, ledgerNow())
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
	now := ledgerNow()
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
	case "work_migration_report":
		result, err = workMigrationReport(ctx, tx)
	case workLegacyGetTool:
		result, err = readWorkLegacy(ctx, tx, a.TaskID)
	case workLegacyPutTool:
		result, err = putWorkLegacy(ctx, tx, a, now)
	default:
		return nil, ledgerFail("invalid_argument", "unknown work tool")
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
		return sessionDBToolResult(ledgerError{"invalid_argument", "invalid work arguments"}, true), nil
	}
	ctx, cancel := context.WithTimeout(ctx, sessionDBCallTimeout)
	defer cancel()
	result, err := s.work(ctx, name, a)
	if err != nil {
		var known ledgerError
		if !errors.As(err, &known) {
			known = ledgerError{"storage_error", "work storage operation failed; whether it committed is unknown"}
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
