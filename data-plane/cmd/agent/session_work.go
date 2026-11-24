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
// The ledger these records came from is gone, and so is everything that moved
// it: the migration, the rollback export, the retirement, the snapshot tools
// and the state report they were read through (AC-N10). sp_work_meta carries
// one thing now — this namespace's version — because a slot has no transition
// to be in the middle of, and a "state" nothing can change is not a fact a
// caller should be able to branch on.
//
// What replaces that report is not another tool but the gate every work call
// already passes through: migrateWork decides whether this image can serve this
// database at all, and refuses it by name if it cannot. A caller learns where it
// stands from whether its own call is answered, which cannot drift from the
// truth the way a separate report can.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// workSchemaLive is what a slot keeps: the agreed records, the receipts and
// retired ids, the internal management counters and claim tokens, the parent's
// own bookkeeping, and where a record came from. The tables a transition used to
// need are not here and must never be re-added — a CREATE TABLE IF NOT EXISTS
// over a database that is past the move would resurrect them as empty ones,
// which reads as "the transition is still here" to anything that checks.
const workSchemaLive = `CREATE TABLE IF NOT EXISTS sp_work_meta (version INTEGER NOT NULL);
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
CREATE TABLE IF NOT EXISTS sp_work_relay (
	id TEXT PRIMARY KEY,
	seq INTEGER NOT NULL UNIQUE,
	sender TEXT NOT NULL,
	recipient TEXT NOT NULL,
	kind TEXT NOT NULL,
	subject TEXT NOT NULL,
	body TEXT NOT NULL,
	task_id TEXT,
	state TEXT NOT NULL,
	result TEXT,
	version INTEGER NOT NULL,
	attempts INTEGER NOT NULL,
	sent_at TEXT NOT NULL,
	claimed_at TEXT,
	settled_at TEXT
);
CREATE INDEX IF NOT EXISTS sp_work_relay_inbox ON sp_work_relay (recipient, state, seq);
CREATE TABLE IF NOT EXISTS sp_work_relay_claim (
	message_id TEXT PRIMARY KEY,
	claim_token TEXT NOT NULL,
	incarnation TEXT NOT NULL,
	claimed_at TEXT NOT NULL
);`

// The records that existed only to manage the move onto the work records, and
// that this image removes once a slot is past it: the step logs of the
// migration and the retirement, and the verbatim copies the migration kept
// beside each moved row. None of them is anyone's work — they are a history of
// a transition, read by tools this image no longer has.
//
// They are dropped on the one-time step to workSchemaVersionFinal, which is
// only taken on a database whose records already *are* the work records. A
// database still holding the ledger never reaches that step, so this list never
// drops a row that is the only copy of something.
var workTransitionTables = []string{
	"sp_work_migration", "sp_work_retirement",
	"sp_work_task_legacy", "sp_work_turn_legacy",
}

// The ledger itself, which an image with the retirement door dropped once its
// rows had been adopted. This image has no door, so it never drops these: their
// presence is what tells the schema gate that this slot's records were never
// moved, and dropping them would destroy the only copy.
var workLedgerTables = []string{
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
	// workSchemaVersionFinal is the version this image writes and reads: the
	// work records with no transition bookkeeping left beside them. The step up
	// from workSchemaVersionRetired drops the records only the move's own tools
	// read (workTransitionTables), and is taken once, on open.
	//
	// The bump is what keeps the removal honest in both directions. An image
	// that still has the retirement tools refuses a database at this version
	// rather than reporting a step log that is gone, and this image refuses
	// anything above it.
	//
	// It is also what the `state` column used to be read for. A version says
	// which shape this database is in, which is a fact about the file; a state
	// said where a move had got to, and no move can run here. So the column goes
	// with the rest of the transition's bookkeeping and the version is the whole
	// answer.
	workSchemaVersionFinal = 3
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
	// ClaimToken proves the caller is the parent that took this turn, or the
	// lane that took a relay message. It is internal management state, not a
	// field of the agreed turn (session_work_turns.go, session_work_relay.go).
	ClaimToken string `json:"claim_token,omitempty"`
	// Message is one handover between the two lanes of a manager. Its sender
	// and recipient are not in it: a lane's own role comes from its MCP
	// registration, so neither lane can send as the other
	// (session_work_relay.go).
	Message *relayInput `json:"message,omitempty"`
	// Box selects which side of the relay a list reads — what was sent to this
	// lane, or what it sent.
	Box string `json:"box,omitempty"`
	// AfterSeq is the relay's page cursor. It is the message's own arrival
	// number, so a page boundary neither repeats nor skips one.
	AfterSeq *int64 `json:"after_seq,omitempty"`
	// Outcome and Result settle a relay message: processed or failed, and what
	// the sender reads about it.
	Outcome string `json:"outcome,omitempty"`
	Result  string `json:"result,omitempty"`
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
	// The handover between a manager's two lanes, which is neither a task nor a
	// turn: it is what the parent and the monitor pass each other and how they
	// learn it was dealt with (session_work_relay.go). These are listed only to
	// a caller that is one of the two lanes.
	relaySendTool, relayListTool, relayClaimTool, relayAckTool, relayStatusTool,
}

// workLedgerWrites are the calls that change the work records a person reads —
// the records the monitor lane owns (AC-N11). They are not refused here but in
// front of the arguments (session_db_mcp.go), so a parent lane learns that the
// records are not its to write rather than that its arguments were wrong.
func isWorkLedgerWrite(name string) bool {
	switch name {
	case "task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch",
		taskBookPutTool, monitorPutTool, monitorDeleteTool:
		return true
	}
	return false
}

// workRemovedTools names what to call instead of a tool that is gone. The
// ledger tools, the migration, the rollback export and now the retirement and
// its snapshot tools are not in this image any more (AC-N10), and a parent that
// still calls one is told the name of the call that does the job rather than
// "unknown tool" — the same discipline the cutover's own refusals followed.
//
// A removed name is never reused for something else, so these entries stay for
// as long as a parent could still be holding an older tools/list.
const workNoMoveAnswer = "this image performs no migration; the work records are the records — " +
	"read them with task_list and turn_list"

var workRemovedTools = map[string]string{
	"ledger_get":               "task_get, task_list and turn_list",
	"ledger_list":              "task_get, task_list and turn_list",
	"ledger_put":               "task_put and task_delete, or " + monitorPutTool + "/" + monitorDeleteTool + " for a monitoring loop",
	"ledger_delete":            "task_put and task_delete, or " + monitorPutTool + "/" + monitorDeleteTool + " for a monitoring loop",
	"ledger_choices_put":       "turn_patch with choices",
	"ledger_choices_clear":     "turn_patch with choices",
	"ledger_requests_list":     "turn_list with open=true",
	"ledger_requests_claim":    "turn_patch",
	"ledger_requests_ack":      "turn_patch",
	"ledger_requests_dispatch": "turn_dispatch",
	"ledger_events_list":       "turn_list — a task's turns are its history",
	workLegacyGetTool:          taskBookGetTool,
	workLegacyPutTool:          taskBookPutTool,
	// The move, everything that managed it, and the report they were read
	// through. None of these has a successor call, and none is replaced under
	// another name: there is no move to make, no snapshot of one to keep, and no
	// transition state to be in. So the message says what is true instead of
	// naming a tool — pointing at a management call that no longer exists is how
	// a parent ends up looking for the next one.
	"work_migration_report": workNoMoveAnswer,
	"work_migrate":          workNoMoveAnswer,
	"work_rollback_export":  workNoMoveAnswer,
	workRetireTool:          workNoMoveAnswer,
	workStateTool: "nothing — this image keeps no transition state to report. " +
		"Whether this slot can be served is answered by your own call: " +
		"a refusal with code schema_version means it cannot, and says why",
	workBackupsTool:      "nothing — the snapshots the move left are no longer this image's to report",
	workBackupsPruneTool: "nothing — the snapshots the move left are no longer this image's to delete",
}

const (
	// The names the transition offered, kept only so a call to one is answered
	// with workRemovedTools rather than "unknown tool". None is in workTools, so
	// none is listed or callable, and none is re-added under another name.
	workRetireTool       = "work_retire"
	workStateTool        = "work_state"
	workBackupsTool      = "work_backups"
	workBackupsPruneTool = "work_backups_prune"
)

func isRemovedWorkTool(name string) bool { _, ok := workRemovedTools[name]; return ok }

func workRemovedToolSuccessor(name string) string { return workRemovedTools[name] }

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
		taskBookPutTool, monitorPutTool, monitorDeleteTool:
		return true
	}
	return isRelayMutation(name)
}

// migrateWork creates this namespace transactionally on first use and refuses a
// version it was not written for. It never touches a user table, never changes
// PRAGMA user_version, and never repairs a collision destructively — a table
// named sp_work_* that this schema did not write is a refusal, not something to
// drop.
// A new slot is born with no ledger and nothing to adopt, so it is born at the
// final version. A database that still has ledger tables is refused instead:
// this image removed the door that adopted them, so serving it would mean
// answering "no work" about rows that are sitting right there. That refusal is
// the whole protection for a slot the move never reached — it is louder than an
// empty page and it loses nothing, because the ledger is left exactly as it is
// for an image that can still move it.
//
// This is also the only place a caller learns where it stands. There is no state
// report any more: every work call passes through here first, so a call that is
// answered is the statement that this database is servable, and a refusal
// carries the reason by name.
func migrateWork(ctx context.Context, tx *sql.Tx) error {
	// Any one of the ledger's tables is enough to refuse on. Checking only the
	// task table would let a slot whose tasks were adopted but whose requests
	// were not through, and the unanswered requests are the rows a person is
	// waiting on.
	for _, table := range workLedgerTables {
		present, err := workTableExists(ctx, tx, table)
		if err != nil {
			return err
		}
		if present {
			return workFail("schema_version",
				"this slot's records are still in the ledger ("+table+") and this image has no "+
					"migration; nothing was read or changed — serve this slot with the image that "+
					"still offers work_retire, which moves them, and deploy this image to it afterwards")
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'sp_work_%'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.ExecContext(ctx, workSchemaLive); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO sp_work_meta (version) VALUES (?)", workSchemaVersionFinal)
		return err
	}
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT version FROM sp_work_meta").Scan(&version); err != nil {
		return workFail("schema_version", "work namespace collision or invalid schema; no tables changed")
	}
	// Additive on purpose: a database created by an earlier version still gets
	// the tables a later additive change introduces, and CREATE TABLE IF NOT
	// EXISTS leaves the ones already there alone. The transition's own tables
	// are never in this statement, so the step below cannot be undone by the
	// next open recreating them as empty ones.
	switch version {
	case workSchemaVersionFinal:
		_, err := tx.ExecContext(ctx, workSchemaLive)
		return err
	case workSchemaVersionRetired:
		// A slot an older image retired: its records are already the work
		// records, so the only thing left is the bookkeeping the move kept about
		// itself. Dropping it and writing the version is one transaction with
		// the caller's own work, so a crash leaves either the old shape or the
		// new one.
		if _, err := tx.ExecContext(ctx, workSchemaLive); err != nil {
			return err
		}
		return endWorkTransitionRecords(ctx, tx)
	case workSchemaVersion:
		// Version 1 with no ledger beside it: an older image was mid-move when
		// the ledger went, which this code cannot reconstruct. Refusing is the
		// only answer that does not invent a state.
		return workFail("schema_version",
			"this slot was left mid-migration and this image has no migration; "+
				"nothing was read or changed — restore it with the image that still offers work_retire")
	default:
		return workFail("schema_version", "unsupported work schema; no downgrade performed")
	}
}

// workTx runs one read or write against the work records in its own
// transaction, with the schema gate ahead of it — so a caller can never reach
// the records of a database this image refuses to serve.
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

// endWorkTransitionRecords is the one step that finishes the move for good: it
// drops the records only the move's own tools read, and replaces sp_work_meta
// with the single column that is still a fact.
//
// It is reached only from a database whose rows are already the work records, so
// each drop is of a copy or of a log, never of the last copy of anyone's work.
// Rebuilding the meta table rather than altering it keeps this independent of
// which SQLite build is underneath, and the whole step rides the caller's
// transaction — a crash leaves the old shape or the new one, never half.
func endWorkTransitionRecords(ctx context.Context, tx *sql.Tx) error {
	for _, table := range workTransitionTables {
		if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		"CREATE TABLE sp_work_meta_final (version INTEGER NOT NULL)",
		"INSERT INTO sp_work_meta_final (version) VALUES (" + strconv.Itoa(workSchemaVersionFinal) + ")",
		"DROP TABLE sp_work_meta",
		"ALTER TABLE sp_work_meta_final RENAME TO sp_work_meta",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
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
//
// caller is which lane of this manager is calling and which incarnation of it.
// Both come from the lane's own MCP registration, which the platform renders and
// the model cannot change, so they are configuration rather than arguments — a
// lane cannot claim to be the other one, and an incarnation cannot claim to be
// an earlier one whose work it is recovering (session_db_mcp.go).
func (s *sessionDB) work(ctx context.Context, caller workCaller, name string, a workArgs) (any, error) {
	if err := validateWork(name, a); err != nil {
		return nil, err
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
	// There is no fence to check before a write any more. The fence existed for
	// the window in which a move held the database, and migrateWork above
	// refuses every version a half-finished move could have left — so a write
	// that gets this far is on a database no move can touch.
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
	case taskBookGetTool:
		result, err = readWorkLegacy(ctx, tx, a.TaskID)
	case taskBookPutTool:
		result, err = putWorkLegacy(ctx, tx, a, now)
	case monitorGetTool, monitorListTool, monitorPutTool, monitorDeleteTool:
		result, err = runMonitorTool(ctx, tx, name, a, now)
	case relaySendTool, relayListTool, relayClaimTool, relayAckTool, relayStatusTool:
		result, err = runRelayTool(ctx, tx, name, caller.lane, caller.incarnation, a, now)
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
func (s *sessionDB) callWork(ctx context.Context, caller workCaller, name string, raw json.RawMessage) (any, *jsonRPCError) {
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
		err = checkWorkFields(caller.lane, name, fields)
	}
	if err == nil && fields["turn"] != nil {
		err = readWorkTurnFields(&a, fields["turn"])
	}
	if err != nil {
		return sessionDBToolResult(workError{"invalid_argument", "invalid work arguments"}, true), nil
	}
	ctx, cancel := context.WithTimeout(ctx, sessionDBCallTimeout)
	defer cancel()
	result, err := s.work(ctx, caller, name, a)
	if err != nil {
		var known workError
		if !errors.As(err, &known) {
			known = workError{"storage_error", "work storage operation failed; whether it committed is unknown"}
		}
		return sessionDBToolResult(known, true), nil
	}
	return sessionDBToolResult(result, false), nil
}

func checkWorkFields(lane, name string, fields map[string]json.RawMessage) error {
	for _, definition := range workToolDefinitions(lane) {
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
