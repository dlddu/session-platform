// What the session offers a person to say about one task, and the task's own
// history.
//
// Choices are written by the session, never by the app: the app used to carry a
// fixed list keyed off a task's status, which could only ever repeat the status
// back ("it is blocked — ask why"). The session knows what this task is
// actually waiting on, so it writes the sentences, and the app shows them
// unchanged. A choice carries the task version it was written against, so a
// task that moves afterwards leaves its choices behind: a stale choice is not
// shown and not actable, without anyone having to delete it.
//
// Events are the task's history as a person reads it: the requests they left,
// what the session did with them, and the task's own transitions. They are
// append-only and written in the same transaction as the change they describe,
// so a history row cannot exist for a change that did not commit — and a
// replayed mutation adds none. They are not an agent log: every row is a
// bounded, named fact, and the claim token, credentials and transcript never
// reach them.
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

// Additive, and sp_ledger_meta stays at version 1 for the same reason the
// user-request table did: an older sidecar image must keep opening this
// database after a rollback, and it can — it never looks at these tables.
const ledgerChoicesSchema = `CREATE TABLE IF NOT EXISTS sp_ledger_task_choices (
	task_id TEXT NOT NULL,
	choice_id TEXT NOT NULL,
	position INTEGER NOT NULL,
	label TEXT NOT NULL,
	request_text TEXT NOT NULL,
	basis_version INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (task_id, choice_id)
);
CREATE TABLE IF NOT EXISTS sp_ledger_task_events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	at TEXT NOT NULL,
	kind TEXT NOT NULL,
	actor TEXT NOT NULL,
	summary TEXT NOT NULL,
	detail TEXT NOT NULL,
	request_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sp_ledger_task_events_task ON sp_ledger_task_events (task_id, seq);`

// addRequestChoiceColumn adds the request row's choice_id to a database written
// by the earlier image. Additive and idempotent: a column that is already there
// is left alone, and an older sidecar that never selects it keeps working.
func addRequestChoiceColumn(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info('sp_ledger_user_requests')")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == "choice_id" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "ALTER TABLE sp_ledger_user_requests ADD COLUMN choice_id TEXT NOT NULL DEFAULT ''")
	return err
}

const (
	// choiceMax bounds one task's offered choices. A person picks from a short
	// list or writes their own; a long list is a worse screen, not a richer one.
	choiceMax = 6
	// choiceLabelMax is the button's words, choiceTextMax the sentence it fills
	// the request box with.
	choiceLabelMax = 80
	choiceTextMax  = 1000
	// eventSummaryMax bounds one history line, eventDetailMax its bounded JSON.
	eventSummaryMax = 200
	eventDetailMax  = 4096
	// eventListLimitDefault/Max page the history the same way tasks are paged.
	eventListLimitDefault = 20
	eventListLimitMax     = 100
)

// Event kinds. Each is a fact a person can read; nothing here is a log level.
const (
	eventTaskCreated       = "task_created"
	eventTaskUpdated       = "task_updated"
	eventTaskDeleted       = "task_deleted"
	eventRequestStored     = "request_stored"
	eventRequestClaimed    = "request_claimed"
	eventRequestDispatched = "request_dispatched"
	eventRequestSettled    = "request_settled"
	eventChoicesPublished  = "choices_published"
	eventChoicesCleared    = "choices_cleared"
)

// ledgerChoice is one thing the session offers to say about a task. The id is
// the session's own stable handle: the app sends it back with the request, so
// the session can tell which of its own sentences the person picked.
type ledgerChoice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	RequestText string `json:"request_text"`
	// BasisVersion is the task version these were written against. The reader
	// compares it with the task's current version; they are equal or the choice
	// is history.
	BasisVersion int64  `json:"basis_version"`
	CreatedAt    string `json:"created_at"`
}

// ledgerTaskEvent is one line of a task's history.
type ledgerTaskEvent struct {
	Seq       int64          `json:"seq"`
	TaskID    string         `json:"task_id"`
	At        string         `json:"at"`
	Kind      string         `json:"kind"`
	Actor     string         `json:"actor"`
	Summary   string         `json:"summary"`
	Detail    map[string]any `json:"detail,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
}

var ledgerChoiceTools = []string{"ledger_choices_put", "ledger_choices_clear", "ledger_events_list"}

func isLedgerChoiceTool(name string) bool {
	for _, n := range ledgerChoiceTools {
		if n == name {
			return true
		}
	}
	return false
}

func ledgerChoiceToolDefinitions() []map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	return []map[string]any{
		{
			"name":        "ledger_choices_put",
			"description": "Replaces what the app offers a person to say about one task: up to 6 entries of {choice_id, label (<=80 chars), request_text (<=1000 chars)}. expected_version must be the task's current version — a mismatch is a conflict, and once the task moves to a later version these entries stop being served, so a changed task never shows sentences written about its older state. The entries are shown as written; they grant no tool, approval or merge.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
				"choices": map[string]any{"type": "array", "maxItems": choiceMax, "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"choice_id": str(), "label": str(), "request_text": str(),
					},
					"required":             []string{"choice_id", "label", "request_text"},
					"additionalProperties": false,
				}},
			}, "required": []string{"id", "request_id", "expected_version", "choices"}, "additionalProperties": false},
		},
		{
			"name":        "ledger_choices_clear",
			"description": "Removes every offered choice of one task, so the app shows that none are offered and leaves free text. expected_version must be the task's current version.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
			}, "required": []string{"id", "request_id", "expected_version"}, "additionalProperties": false},
		},
		{
			"name":        "ledger_events_list",
			"description": "One task's history, oldest seq first: its own transitions and what happened to the requests left on it. Filters: id (task, required), after_seq (keyset cursor), limit (1..100, default 20). Rows are append-only and written with the change they describe; reading changes nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id":        str(),
				"after_seq": map[string]any{"type": "integer", "minimum": 0},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": eventListLimitMax, "default": eventListLimitDefault},
			}, "required": []string{"id"}, "additionalProperties": false},
		},
	}
}

func validateLedgerChoiceTool(name string, a ledgerArgs) error {
	bad := func(s string) error { return ledgerFail("invalid_argument", s) }
	if !ledgerID.MatchString(a.ID) {
		return bad("id must be 1..128 letters, digits, _ or -")
	}
	if name == "ledger_events_list" {
		if a.Limit < 0 || a.Limit > eventListLimitMax {
			return bad("invalid pagination")
		}
		if a.AfterSeq != nil && (*a.AfterSeq < 0 || *a.AfterSeq > requestSafeInt) {
			return bad("invalid after_seq")
		}
		return nil
	}
	if !ledgerID.MatchString(a.RequestID) {
		return bad("request_id must be 1..128 letters, digits, _ or -")
	}
	if a.ExpectedVersion == nil || *a.ExpectedVersion < 1 || *a.ExpectedVersion > requestSafeInt {
		return bad("positive expected_version required")
	}
	if name == "ledger_choices_clear" {
		return nil
	}
	if a.Choices == nil {
		return bad("choices required (an empty array clears them)")
	}
	if len(a.Choices) > choiceMax {
		return bad(fmt.Sprintf("at most %d choices", choiceMax))
	}
	seen := map[string]bool{}
	for _, c := range a.Choices {
		if !ledgerID.MatchString(c.ChoiceID) || seen[c.ChoiceID] {
			return bad("choice_id must be 1..128 letters, digits, _ or - and unique")
		}
		seen[c.ChoiceID] = true
		if strings.TrimSpace(c.Label) == "" || len(c.Label) > choiceLabelMax {
			return bad(fmt.Sprintf("label must be 1..%d characters", choiceLabelMax))
		}
		if strings.TrimSpace(c.RequestText) == "" || len(c.RequestText) > choiceTextMax {
			return bad(fmt.Sprintf("request_text must be 1..%d characters", choiceTextMax))
		}
	}
	return nil
}

// recordTaskEvent appends one history row. It runs inside the caller's
// transaction, so the row and the change it describes commit together or not at
// all — and a mutation that replays its receipt never reaches here, which is
// what keeps the history free of duplicates.
func recordTaskEvent(ctx context.Context, tx *sql.Tx, e ledgerTaskEvent, now time.Time) error {
	if e.At == "" {
		e.At = now.UTC().Format(time.RFC3339)
	}
	if len(e.Summary) > eventSummaryMax {
		e.Summary = e.Summary[:eventSummaryMax]
	}
	detail := "{}"
	if e.Detail != nil {
		encoded, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		if len(encoded) > eventDetailMax {
			// Keep the row rather than the oversized payload: a history line
			// that says "too large to keep" is still a true line.
			encoded, _ = json.Marshal(map[string]any{"omitted": "detail exceeded the bound"})
		}
		detail = string(encoded)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sp_ledger_task_events (task_id,at,kind,actor,summary,detail,request_id)
		 VALUES (?,?,?,?,?,?,?)`,
		e.TaskID, e.At, e.Kind, e.Actor, e.Summary, detail, e.RequestID)
	return err
}

// freshChoices are the choices written against the task's current version.
// Anything older is left in the table but not served: the task has moved, so
// the sentences were written about a state that no longer holds.
func freshChoices(ctx context.Context, tx *sql.Tx, taskID string, version int64) ([]ledgerChoice, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT choice_id,label,request_text,basis_version,created_at
		 FROM sp_ledger_task_choices WHERE task_id=? AND basis_version=? ORDER BY position`,
		taskID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ledgerChoice{}
	for rows.Next() {
		var c ledgerChoice
		if err := rows.Scan(&c.ID, &c.Label, &c.RequestText, &c.BasisVersion, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// attachChoices fills in each record's offered choices. One query per record
// keeps the "fresh" test exact: it is the record's own version that decides.
func attachChoices(ctx context.Context, tx *sql.Tx, records []*ledgerRecord) error {
	for _, r := range records {
		choices, err := freshChoices(ctx, tx, r.ID, r.Version)
		if err != nil {
			return err
		}
		if len(choices) > 0 {
			r.Choices = choices
		}
	}
	return nil
}

// ledgerChoiceCall runs one of the choice/history tools inside the ledger's own
// transaction.
func (s *sessionDB) ledgerChoiceCall(ctx context.Context, tx *sql.Tx, name string, a ledgerArgs, now time.Time) (any, error) {
	if name == "ledger_events_list" {
		return listTaskEvents(ctx, tx, a.ID, a.AfterSeq, a.Limit)
	}
	// Mutations replay their first committed answer, as every ledger mutation
	// does — a retry neither rewrites the choices nor adds a history row.
	payload, _ := json.Marshal(a)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(append([]byte(name), payload...)))
	var oldFingerprint, response string
	err := tx.QueryRowContext(ctx, "SELECT fingerprint,response FROM sp_ledger_requests WHERE id=?", a.RequestID).
		Scan(&oldFingerprint, &response)
	if err == nil {
		if fingerprint != oldFingerprint {
			return nil, ledgerFail("conflict", "request_id already used with different arguments")
		}
		return json.RawMessage(response), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	record, err := readLedger(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}
	if record.Version != *a.ExpectedVersion {
		// The task moved since the session read it, so these sentences are
		// about an older state. Read it again and write them against the
		// version that now holds.
		return nil, ledgerFail("conflict", "expected_version does not match the task's current version")
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sp_ledger_task_choices WHERE task_id=?", a.ID); err != nil {
		return nil, err
	}
	stored := []ledgerChoice{}
	if name == "ledger_choices_put" {
		at := now.UTC().Format(time.RFC3339)
		for i, c := range a.Choices {
			if _, err = tx.ExecContext(ctx,
				`INSERT INTO sp_ledger_task_choices
				 (task_id,choice_id,position,label,request_text,basis_version,created_at) VALUES (?,?,?,?,?,?,?)`,
				a.ID, c.ChoiceID, i, c.Label, c.RequestText, record.Version, at); err != nil {
				return nil, err
			}
			stored = append(stored, ledgerChoice{
				ID: c.ChoiceID, Label: c.Label, RequestText: c.RequestText,
				BasisVersion: record.Version, CreatedAt: at,
			})
		}
	}
	kind, summary := eventChoicesCleared, "보낼 수 있는 선택지를 비웠다"
	if len(stored) > 0 {
		kind = eventChoicesPublished
		summary = fmt.Sprintf("보낼 수 있는 선택지 %d개를 올렸다", len(stored))
	}
	labels := make([]string, 0, len(stored))
	for _, c := range stored {
		labels = append(labels, c.Label)
	}
	if err = recordTaskEvent(ctx, tx, ledgerTaskEvent{
		TaskID: a.ID, Kind: kind, Actor: record.Task.Owner, Summary: summary,
		Detail:    map[string]any{"basis_version": record.Version, "labels": labels},
		RequestID: a.RequestID,
	}, now); err != nil {
		return nil, err
	}
	result := map[string]any{"id": a.ID, "basis_version": record.Version, "choices": stored}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO sp_ledger_requests VALUES (?,?,?)",
		a.RequestID, fingerprint, string(encoded)); err != nil {
		return nil, err
	}
	return result, nil
}

// listTaskEvents pages one task's history. The cursor is the row's own seq, so
// a page boundary cannot repeat or skip a row the way a time-ordered cursor
// could when two rows share a second.
func listTaskEvents(ctx context.Context, tx *sql.Tx, taskID string, afterSeq *int64, limit int) (any, error) {
	if limit == 0 {
		limit = eventListLimitDefault
	}
	after := int64(0)
	if afterSeq != nil {
		after = *afterSeq
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT seq,task_id,at,kind,actor,summary,detail,request_id FROM sp_ledger_task_events
		 WHERE task_id=? AND seq>? ORDER BY seq LIMIT ?`, taskID, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ledgerTaskEvent{}
	next := int64(0)
	for rows.Next() {
		var e ledgerTaskEvent
		var detail string
		if err := rows.Scan(&e.Seq, &e.TaskID, &e.At, &e.Kind, &e.Actor, &e.Summary, &detail, &e.RequestID); err != nil {
			return nil, err
		}
		if len(out) == limit {
			next = out[len(out)-1].Seq
			break
		}
		if detail != "" && detail != "{}" {
			_ = json.Unmarshal([]byte(detail), &e.Detail)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := map[string]any{"events": out}
	if next != 0 {
		result["next_cursor"] = next
	}
	return result, nil
}

// staleChoiceMessage is the sentence a refused pick carries all the way to the
// person's screen. The control plane and the app's backend match on it, so it
// changes only together with them.
const staleChoiceMessage = "that choice is no longer offered for this task"

// checkLedgerChoiceFresh refuses a pick that the session no longer offers.
//
// Hiding stale choices on the read side is not enough on its own: a screen
// opened before the task moved still holds the old sentences and can submit
// one. So the write side checks the same rule the read side applies — the
// choice must exist on this task and must have been written against the task's
// version as it stands now — and additionally that the basis the caller saw
// agrees, which catches the case where the same choice_id was re-published
// against a newer version. A refusal keeps the draft on the person's screen;
// nothing is stored.
func checkLedgerChoiceFresh(ctx context.Context, tx *sql.Tx, taskID, choiceID string, callerBasis, taskVersion int64) error {
	var basis int64
	err := tx.QueryRowContext(ctx,
		"SELECT basis_version FROM sp_ledger_task_choices WHERE task_id=? AND choice_id=?",
		taskID, choiceID).Scan(&basis)
	if errors.Is(err, sql.ErrNoRows) {
		return ledgerFail("stale_choice", staleChoiceMessage)
	}
	if err != nil {
		return err
	}
	if basis != taskVersion || (callerBasis != 0 && callerBasis != basis) {
		return ledgerFail("stale_choice", staleChoiceMessage)
	}
	return nil
}
