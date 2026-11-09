package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// The ledger shares the session database, never a control-plane credential.
// Its namespace and version are independent of raw SQL users' schemas.
const ledgerSchema = `CREATE TABLE sp_ledger_meta (version INTEGER NOT NULL);
INSERT INTO sp_ledger_meta VALUES (1);
CREATE TABLE sp_ledger_tasks (id TEXT PRIMARY KEY, version INTEGER NOT NULL, status TEXT NOT NULL, stage TEXT NOT NULL, owner TEXT NOT NULL, document TEXT NOT NULL);
CREATE TABLE sp_ledger_retired (id TEXT PRIMARY KEY);
CREATE TABLE sp_ledger_requests (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, response TEXT NOT NULL);`

var ledgerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type ledgerSession struct {
	ID         string `json:"id"`
	Ownership  string `json:"ownership"`
	Purpose    string `json:"purpose"`
	NextOffset int64  `json:"nextOffset"`
	Seq        *int64 `json:"seq,omitempty"`
	EndSeq     *int64 `json:"end_seq,omitempty"`
	Cleanup    string `json:"cleanup"`
	Reason     string `json:"reason,omitempty"`
	NextAction string `json:"next_action,omitempty"`
}
type ledgerTask struct {
	Title    string          `json:"title"`
	Status   string          `json:"status"`
	Stage    string          `json:"stage"`
	Owner    string          `json:"owner"`
	Sessions []ledgerSession `json:"sessions"`
	// Bounded JSON holds approval, result/PR/head, validation, notification,
	// preservation and timestamped observations; it is not an output archive.
	Details map[string]any `json:"details"`
}
type ledgerArgs struct {
	ID              string      `json:"id,omitempty"`
	RequestID       string      `json:"request_id,omitempty"`
	ExpectedVersion *int64      `json:"expected_version,omitempty"`
	Task            *ledgerTask `json:"task,omitempty"`
	Status          string      `json:"status,omitempty"`
	Stage           string      `json:"stage,omitempty"`
	Owner           string      `json:"owner,omitempty"`
	SessionID       string      `json:"session_id,omitempty"`
	AfterID         string      `json:"after_id,omitempty"`
	Limit           int         `json:"limit,omitempty"`
}
type ledgerRecord struct {
	ID      string     `json:"id"`
	Version int64      `json:"version"`
	Task    ledgerTask `json:"task"`
}
type ledgerError struct {
	Code      string `json:"code"`
	ErrorText string `json:"error"`
}

func (e ledgerError) Error() string         { return e.ErrorText }
func ledgerFail(code, message string) error { return ledgerError{code, message} }

func ledgerToolDefinitions() []map[string]any {
	out := []map[string]any{}
	for _, name := range []string{"ledger_get", "ledger_list", "ledger_put", "ledger_delete"} {
		props := map[string]any{}
		required := []string{}
		add := func(n, typ string) { props[n] = map[string]any{"type": typ} }
		if name == "ledger_list" {
			for _, f := range []string{"status", "stage", "owner", "session_id", "after_id"} {
				add(f, "string")
			}
			props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 20}
		} else {
			add("id", "string")
			required = append(required, "id")
		}
		if name == "ledger_put" || name == "ledger_delete" {
			add("request_id", "string")
			props["expected_version"] = map[string]any{"type": "integer", "minimum": 0}
			required = append(required, "request_id", "expected_version")
		}
		if name == "ledger_put" {
			props["task"] = map[string]any{"type": "object", "description": "Full replacement: title, status (planned/running/blocked/review/completed/unknown), stage, owner (parent ID), sessions [{id, ownership: owned/reused/unknown, purpose, nextOffset, seq?, end_seq?, cleanup: pending/retained/deleting/deleted, reason?, next_action?}], details object for approval/result/PR/validation/notification/preservation/observations. Max 64KiB. No credentials or transcript.", "required": []string{"title", "status", "stage", "owner", "sessions", "details"}}
			required = append(required, "task")
		}
		out = append(out, map[string]any{"name": name, "description": "Chat-slot task ledger (kept across chat sessions on the same slot); never calls platform session create/delete or sends notifications. put is full replacement with atomic expected_version (0=create); mutation request_id replays the original committed response, changed payload conflicts. list is ID-keyset pagination (next_cursor -> after_id), not a snapshot. Errors carry code invalid_argument/not_found/conflict/storage_error/schema_version. Get before update; preserve unrelated fields.", "inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}})
	}
	return out
}
func validateLedger(name string, a ledgerArgs) error {
	bad := func(s string) error { return ledgerFail("invalid_argument", s) }
	if name != "ledger_get" && name != "ledger_list" && name != "ledger_put" && name != "ledger_delete" {
		return bad("unknown ledger tool")
	}
	if name != "ledger_list" && !ledgerID.MatchString(a.ID) {
		return bad("id must be 1..128 letters, digits, _ or -")
	}
	if name == "ledger_list" {
		if a.Limit < 0 || a.Limit > 100 || (a.AfterID != "" && !ledgerID.MatchString(a.AfterID)) {
			return bad("invalid pagination")
		}
		for _, s := range []string{a.Status, a.Stage, a.Owner, a.SessionID} {
			if len(s) > 128 {
				return bad("filter too long")
			}
		}
		return nil
	}
	if name == "ledger_get" {
		return nil
	}
	if !ledgerID.MatchString(a.RequestID) || a.ExpectedVersion == nil || *a.ExpectedVersion < 0 || *a.ExpectedVersion >= 9007199254740991 {
		return bad("request_id and safe nonnegative expected_version required")
	}
	if name == "ledger_delete" {
		if *a.ExpectedVersion == 0 {
			return bad("delete needs positive expected_version")
		}
		return nil
	}
	t := a.Task
	if t == nil || strings.TrimSpace(t.Title) == "" || len(t.Title) > 512 || !ledgerID.MatchString(t.Owner) || strings.TrimSpace(t.Stage) == "" || len(t.Stage) > 128 || t.Sessions == nil || t.Details == nil {
		return bad("title, stage, owner, sessions and details required")
	}
	if !strings.Contains("|planned|running|blocked|review|completed|unknown|", "|"+t.Status+"|") || t.Status == "" {
		return bad("invalid status")
	}
	if len(t.Sessions) > 100 {
		return bad("too many sessions")
	}
	seen := map[string]bool{}
	for _, s := range t.Sessions {
		if !ledgerID.MatchString(s.ID) || seen[s.ID] || strings.TrimSpace(s.Purpose) == "" || len(s.Purpose) > 512 {
			return bad("invalid or duplicate session")
		}
		seen[s.ID] = true
		if s.Ownership != "owned" && s.Ownership != "reused" && s.Ownership != "unknown" {
			return bad("invalid ownership")
		}
		if s.Cleanup != "pending" && s.Cleanup != "retained" && s.Cleanup != "deleting" && s.Cleanup != "deleted" {
			return bad("invalid cleanup")
		}
		if s.NextOffset < 0 || s.NextOffset > 9007199254740991 || (s.Seq != nil && (*s.Seq < 0 || *s.Seq > 9007199254740991)) || (s.EndSeq != nil && (*s.EndSeq < 0 || *s.EndSeq > 9007199254740991)) {
			return bad("invalid cursor or seq")
		}
		if s.Cleanup == "retained" && (strings.TrimSpace(s.Reason) == "" || strings.TrimSpace(s.NextAction) == "") {
			return bad("retention needs reason and next_action")
		}
		if (s.Cleanup == "deleting" || s.Cleanup == "deleted") && s.Ownership != "owned" {
			return bad("only owned sessions may be recorded as deleting/deleted")
		}
	}
	b, _ := json.Marshal(t)
	if len(b) > 65536 {
		return bad("task exceeds 64KiB")
	}
	return nil
}
func migrateLedger(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'sp_ledger_%'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err := tx.ExecContext(ctx, ledgerSchema)
		return err
	}
	var v int
	if err := tx.QueryRowContext(ctx, "SELECT version FROM sp_ledger_meta").Scan(&v); err != nil {
		return ledgerFail("schema_version", "ledger namespace collision or invalid schema; no tables changed")
	}
	if v != 1 {
		return ledgerFail("schema_version", "unsupported ledger schema; no downgrade performed")
	}
	return nil
}
func readLedger(ctx context.Context, tx *sql.Tx, id string) (ledgerRecord, error) {
	r := ledgerRecord{ID: id}
	var doc string
	err := tx.QueryRowContext(ctx, "SELECT version,document FROM sp_ledger_tasks WHERE id=?", id).Scan(&r.Version, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ledgerFail("not_found", "task not found")
	}
	if err != nil {
		return r, err
	}
	err = decodeLedgerTask(doc, &r.Task)
	return r, err
}
func (s *sessionDB) ledger(ctx context.Context, name string, a ledgerArgs) (any, error) {
	if err := validateLedger(name, a); err != nil {
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
	if err = migrateLedger(ctx, tx); err != nil {
		return nil, err
	}
	var result any
	if name == "ledger_get" {
		result, err = readLedger(ctx, tx, a.ID)
	} else if name == "ledger_list" {
		limit := a.Limit
		if limit == 0 {
			limit = 20
		}
		rows, e := tx.QueryContext(ctx, `SELECT id,version,document FROM sp_ledger_tasks WHERE id>? AND (?='' OR status=?) AND (?='' OR stage=?) AND (?='' OR owner=?) AND (?='' OR EXISTS (SELECT 1 FROM json_each(document,'$.sessions') WHERE json_extract(value,'$.id')=?)) ORDER BY id LIMIT ?`, a.AfterID, a.Status, a.Status, a.Stage, a.Stage, a.Owner, a.Owner, a.SessionID, a.SessionID, limit+1)
		if e != nil {
			return nil, e
		}
		records := []ledgerRecord{}
		next := ""
		size := 0
		for rows.Next() {
			var r ledgerRecord
			var doc string
			if e = rows.Scan(&r.ID, &r.Version, &doc); e != nil {
				break
			}
			if e = decodeLedgerTask(doc, &r.Task); e != nil {
				break
			}
			if len(records) == limit || (len(records) > 0 && size+len(doc) > sessionDBMaxResultBytes/2) {
				next = records[len(records)-1].ID
				break
			}
			size += len(doc)
			records = append(records, r)
		}
		err = errors.Join(e, rows.Err())
		rows.Close()
		result = map[string]any{"tasks": records, "next_cursor": next}
	} else {
		b, _ := json.Marshal(a)
		hash := fmt.Sprintf("%x", sha256.Sum256(append([]byte(name), b...)))
		var oldHash, response string
		e := tx.QueryRowContext(ctx, "SELECT fingerprint,response FROM sp_ledger_requests WHERE id=?", a.RequestID).Scan(&oldHash, &response)
		if e == nil {
			if hash != oldHash {
				return nil, ledgerFail("conflict", "request_id already used with different arguments")
			}
			return json.RawMessage(response), tx.Commit()
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		var version int64
		e = tx.QueryRowContext(ctx, "SELECT version FROM sp_ledger_tasks WHERE id=?", a.ID).Scan(&version)
		if errors.Is(e, sql.ErrNoRows) {
			if *a.ExpectedVersion != 0 {
				return nil, ledgerFail("not_found", "task not found")
			}
			var retired int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_ledger_retired WHERE id=?", a.ID).Scan(&retired); err != nil {
				return nil, err
			}
			if retired != 0 {
				return nil, ledgerFail("conflict", "deleted task ID cannot be reused")
			}
		} else if e != nil {
			return nil, e
		} else if *a.ExpectedVersion != version || *a.ExpectedVersion == 0 {
			return nil, ledgerFail("conflict", "expected_version does not match")
		}
		if name == "ledger_delete" {
			_, err = tx.ExecContext(ctx, "DELETE FROM sp_ledger_tasks WHERE id=? AND version=?", a.ID, *a.ExpectedVersion)
			if err == nil {
				_, err = tx.ExecContext(ctx, "INSERT INTO sp_ledger_retired VALUES (?)", a.ID)
			}
			result = map[string]any{"id": a.ID, "deleted": true, "version": version}
		} else {
			doc, _ := json.Marshal(a.Task)
			version++
			if *a.ExpectedVersion == 0 {
				_, err = tx.ExecContext(ctx, "INSERT INTO sp_ledger_tasks VALUES (?,?,?,?,?,?)", a.ID, version, a.Task.Status, a.Task.Stage, a.Task.Owner, string(doc))
			} else {
				_, err = tx.ExecContext(ctx, "UPDATE sp_ledger_tasks SET version=?,status=?,stage=?,owner=?,document=? WHERE id=? AND version=?", version, a.Task.Status, a.Task.Stage, a.Task.Owner, string(doc), a.ID, *a.ExpectedVersion)
			}
			result = ledgerRecord{a.ID, version, *a.Task}
		}
		if err != nil {
			return nil, err
		}
		encoded, e := json.Marshal(result)
		if e != nil {
			return nil, e
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO sp_ledger_requests VALUES (?,?,?)", a.RequestID, hash, string(encoded))
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *sessionDB) callLedger(ctx context.Context, name string, raw json.RawMessage) (any, *jsonRPCError) {
	var a ledgerArgs
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	d.UseNumber()
	err := d.Decode(&a)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("trailing arguments")
		}
	}
	// Each tool has its own allowed fields, even though the internal union is shared.
	var fields map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(raw, &fields)
	}
	for _, def := range ledgerToolDefinitions() {
		if def["name"] == name {
			schema := def["inputSchema"].(map[string]any)
			for _, key := range schema["required"].([]string) {
				if v, ok := fields[key]; !ok || string(v) == "null" {
					err = errors.New("missing required field")
				}
			}
			props := schema["properties"].(map[string]any)
			for k := range fields {
				if _, ok := props[k]; !ok {
					err = errors.New("unexpected field")
				}
			}
		}
	}
	if name == "ledger_list" {
		if _, ok := fields["limit"]; ok && a.Limit < 1 {
			err = errors.New("invalid limit")
		}
	}
	if name == "ledger_put" && err == nil {
		var taskFields map[string]json.RawMessage
		_ = json.Unmarshal(fields["task"], &taskFields)
		for _, key := range []string{"title", "status", "stage", "owner", "sessions", "details"} {
			if v, ok := taskFields[key]; !ok || string(v) == "null" {
				err = errors.New("missing task field")
			}
		}
		var sessions []map[string]json.RawMessage
		_ = json.Unmarshal(taskFields["sessions"], &sessions)
		for _, rel := range sessions {
			for _, key := range []string{"id", "ownership", "purpose", "nextOffset", "cleanup"} {
				if v, ok := rel[key]; !ok || string(v) == "null" {
					err = errors.New("missing session field")
				}
			}
		}
	}
	if err != nil {
		return sessionDBToolResult(ledgerError{"invalid_argument", "invalid ledger arguments"}, true), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := s.ledger(ctx, name, a)
	if err != nil {
		var e ledgerError
		if !errors.As(err, &e) {
			e = ledgerError{"storage_error", "ledger storage operation failed; reconcile with get or retry identical request"}
		}
		return sessionDBToolResult(e, true), nil
	}
	return sessionDBToolResult(result, false), nil
}

func decodeLedgerTask(doc string, task *ledgerTask) error {
	d := json.NewDecoder(strings.NewReader(doc))
	d.UseNumber()
	return d.Decode(task)
}
