// User requests on a ledger task: what a person asked about one task, kept on
// the slot until the parent picks it up, and the record of what the parent did
// with it.
//
// Why a table of its own, beside sp_ledger_tasks rather than inside the task
// document: a request arrives from outside the session (the app, through the
// control plane) while the parent may be writing the same task's document with
// ledger_put. Appending to the document would mean read-modify-write on it, so
// an arriving request and a parent's own update would race and one would be
// lost. A row in its own table is a single INSERT that touches no task field —
// details, approval and cursors are left exactly as the parent wrote them.
//
// The lifecycle is deliberately not "list means done". A request is claimed
// (one parent wins), the parent records what it is about to do *before* it does
// it, and only an ack with an outcome closes the request. A claim that lapses
// after the parent had already recorded a dispatch does NOT return to pending:
// something may already have happened outside this database, so the request
// moves to reconciling, where the next parent has to find out what landed
// before anything is sent again.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ledgerRequestsSchema is additive and deliberately leaves sp_ledger_meta at
// version 1. An older sidecar image must keep opening this database after a
// rollback, and it can: it never looks at this table. Bumping the schema
// version would have made that rollback refuse the whole ledger — tasks
// included — to gain nothing.
const ledgerRequestsSchema = `CREATE TABLE IF NOT EXISTS sp_ledger_user_requests (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	version INTEGER NOT NULL,
	text TEXT NOT NULL,
	author TEXT NOT NULL,
	created_at TEXT NOT NULL,
	status TEXT NOT NULL,
	claim_owner TEXT NOT NULL,
	claim_token TEXT NOT NULL,
	claim_expires_at TEXT NOT NULL,
	operation_key TEXT NOT NULL,
	operation_target TEXT NOT NULL,
	dispatch_state TEXT NOT NULL,
	dispatched_at TEXT NOT NULL,
	related_seq INTEGER,
	result TEXT NOT NULL,
	settled_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS sp_ledger_user_requests_seq ON sp_ledger_user_requests (seq);
CREATE INDEX IF NOT EXISTS sp_ledger_user_requests_task ON sp_ledger_user_requests (task_id, seq);`

// Where a request stands. Only an ack moves it to processed or failed, and
// reconciling is reachable only from a lapsed claim that had already dispatched
// something — never from a fresh request.
const (
	requestPending     = "pending"
	requestClaimed     = "claimed"
	requestReconciling = "reconciling"
	requestProcessed   = "processed"
	requestFailed      = "failed"
)

// What the parent has done about the request in the outside world. The parent
// writes intended *before* acting, so a crash between acting and acking is
// still visible here as "something may be out there".
const (
	dispatchNone       = ""
	dispatchIntended   = "intended"
	dispatchDispatched = "dispatched"
	dispatchSettled    = "settled"
)

const (
	// requestTextMax bounds one request. It is a person's sentence about one
	// task, not an attachment; the app's own limit is smaller.
	requestTextMax = 8192
	// requestResultMax bounds what the parent writes back.
	requestResultMax = 4096
	// requestClaimTTLDefault is how long a claim is honoured without an ack.
	// Past it the request is reconcilable by another parent — not redoable.
	requestClaimTTLDefault = 15 * time.Minute
	requestClaimTTLMax     = 6 * time.Hour
	// requestListLimitDefault/Max bound one page, as the task list does.
	requestListLimitDefault = 20
	requestListLimitMax     = 100
	// requestSafeInt is the largest integer that survives a JSON round trip
	// through a model, matching the task ledger's own bound.
	requestSafeInt = 9007199254740991
)

// ledgerRequest is one user request as every reader sees it. Status, dispatch
// state and result are the whole answer to "has this been dealt with" — a
// reader never has to infer it from the task document.
type ledgerRequest struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Seq       int64  `json:"seq"`
	Version   int64  `json:"version"`
	Text      string `json:"text"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`
	// ChoiceID is the session's own choice the person picked, empty when they
	// typed their own words. It lets the session tell which of its sentences
	// came back — and the history line say so.
	ChoiceID string `json:"choice_id,omitempty"`
	// The claim's holder, never its token: a reader that did not claim the
	// request must not be able to ack it.
	ClaimOwner      string `json:"claim_owner,omitempty"`
	ClaimExpiresAt  string `json:"claim_expires_at,omitempty"`
	OperationKey    string `json:"operation_key,omitempty"`
	OperationTarget string `json:"operation_target,omitempty"`
	DispatchState   string `json:"dispatch_state,omitempty"`
	DispatchedAt    string `json:"dispatched_at,omitempty"`
	RelatedSeq      *int64 `json:"related_seq,omitempty"`
	Result          string `json:"result,omitempty"`
	SettledAt       string `json:"settled_at,omitempty"`
}

// ledgerRequestSummary rides along with a task so a plain ledger_get or
// ledger_list shows that work is waiting. Without it a parent would have to
// call a second tool to find out whether anything arrived, and a parent that
// forgot would never see the request at all.
type ledgerRequestSummary struct {
	Pending     int   `json:"pending"`
	Claimed     int   `json:"claimed"`
	Reconciling int   `json:"reconciling"`
	LastSeq     int64 `json:"last_seq"`
}

var ledgerRequestTools = []string{
	"ledger_requests_list", "ledger_requests_claim",
	"ledger_requests_dispatch", "ledger_requests_ack",
}

func isLedgerRequestTool(name string) bool {
	for _, n := range ledgerRequestTools {
		if n == name {
			return true
		}
	}
	return false
}

func ledgerRequestToolDefinitions() []map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	out := []map[string]any{
		{
			"name":        "ledger_requests_list",
			"description": "User requests stored on this slot's ledger tasks, oldest seq first. Filters: task_id, status (pending/claimed/reconciling/processed/failed), after_seq (keyset cursor), limit (1..100, default 20). Reading leaves every status unchanged — claim and ack are the transitions.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "status": str(),
				"after_seq": map[string]any{"type": "integer", "minimum": 0},
				"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": requestListLimitMax, "default": requestListLimitDefault},
			}, "required": []string{}, "additionalProperties": false},
		},
		{
			"name":        "ledger_requests_claim",
			"description": "Takes one request: pending -> claimed, atomic on expected_version, so one caller wins and the rest get conflict. claim_owner is the caller's parent ID; ttl_seconds (default 900, max 21600) bounds the claim. The reply carries claim_token, which dispatch and ack require. A claim granted over a lapsed claim that had recorded a dispatch returns status reconciling with reconcile:true and the earlier operation_key/operation_target, since that dispatch may have landed outside this database.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "claim_owner": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
				"ttl_seconds":      map[string]any{"type": "integer", "minimum": 30, "maximum": int64(requestClaimTTLMax / time.Second)},
			}, "required": []string{"id", "request_id", "expected_version", "claim_owner"}, "additionalProperties": false},
		},
		{
			"name":        "ledger_requests_dispatch",
			"description": "Records the caller's intent for this request ahead of the outside action: operation_key is the caller's idempotency key for that action, operation_target names what it acts on (turn seq, PR, session), dispatch_state is intended (decided, not sent) or dispatched (handed over). On a reconciling request an operation_key other than the recorded one is refused with conflict unless observed=not_accepted states the earlier dispatch was established never to have landed; the recorded key is accepted as a retry.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "claim_token": str(),
				"operation_key": str(), "operation_target": str(),
				"dispatch_state": str(), "observed": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
			}, "required": []string{"id", "request_id", "expected_version", "claim_token", "operation_key", "dispatch_state"}, "additionalProperties": false},
		},
		{
			"name":        "ledger_requests_ack",
			"description": "Closes the request with what happened: outcome processed or failed, result the sentence a person reads, related_seq the turn carrying it. Requires the claim_token that currently holds the request; a lapsed or superseded token is refused with conflict. It is the only transition to processed or failed.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "claim_token": str(),
				"outcome": str(), "result": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
				"related_seq":      map[string]any{"type": "integer", "minimum": 0},
			}, "required": []string{"id", "request_id", "expected_version", "claim_token", "outcome", "result"}, "additionalProperties": false},
		},
	}
	return out
}

func validateLedgerRequestTool(name string, a ledgerArgs) error {
	bad := func(s string) error { return ledgerFail("invalid_argument", s) }
	if name == "ledger_requests_list" {
		if a.TaskID != "" && !ledgerID.MatchString(a.TaskID) {
			return bad("task_id must be 1..128 letters, digits, _ or -")
		}
		if a.Status != "" && !isRequestStatus(a.Status) {
			return bad("status must be pending, claimed, reconciling, processed or failed")
		}
		if a.Limit < 0 || a.Limit > requestListLimitMax {
			return bad("invalid pagination")
		}
		if a.AfterSeq != nil && (*a.AfterSeq < 0 || *a.AfterSeq > requestSafeInt) {
			return bad("invalid after_seq")
		}
		return nil
	}
	if !ledgerID.MatchString(a.ID) {
		return bad("id must be 1..128 letters, digits, _ or -")
	}
	if !ledgerID.MatchString(a.RequestID) {
		return bad("request_id must be 1..128 letters, digits, _ or -")
	}
	if a.ExpectedVersion == nil || *a.ExpectedVersion < 1 || *a.ExpectedVersion > requestSafeInt {
		return bad("positive expected_version required")
	}
	switch name {
	case "ledger_requests_claim":
		if !ledgerID.MatchString(a.ClaimOwner) {
			return bad("claim_owner must be 1..128 letters, digits, _ or -")
		}
		if a.TTLSeconds != nil {
			ttl := *a.TTLSeconds
			if ttl < 30 || ttl > int64(requestClaimTTLMax/time.Second) {
				return bad("ttl_seconds must be 30..21600")
			}
		}
	case "ledger_requests_dispatch":
		if !ledgerID.MatchString(a.ClaimToken) {
			return bad("claim_token required")
		}
		if !ledgerID.MatchString(a.OperationKey) {
			return bad("operation_key must be 1..128 letters, digits, _ or -")
		}
		if len(a.OperationTarget) > 512 {
			return bad("operation_target too long")
		}
		if a.DispatchState != dispatchIntended && a.DispatchState != dispatchDispatched {
			return bad("dispatch_state must be intended or dispatched")
		}
		if a.Observed != "" && a.Observed != "not_accepted" {
			return bad("observed, when given, must be not_accepted")
		}
	case "ledger_requests_ack":
		if !ledgerID.MatchString(a.ClaimToken) {
			return bad("claim_token required")
		}
		if a.Outcome != requestProcessed && a.Outcome != requestFailed {
			return bad("outcome must be processed or failed")
		}
		if strings.TrimSpace(a.Result) == "" || len(a.Result) > requestResultMax {
			return bad("result must be 1.." + fmt.Sprint(requestResultMax) + " characters")
		}
		if a.RelatedSeq != nil && (*a.RelatedSeq < 0 || *a.RelatedSeq > requestSafeInt) {
			return bad("invalid related_seq")
		}
	}
	return nil
}

func isRequestStatus(s string) bool {
	switch s {
	case requestPending, requestClaimed, requestReconciling, requestProcessed, requestFailed:
		return true
	}
	return false
}

// appendLedgerRequest stores one request from outside the session. The author
// is given by the caller that authenticated the person (the app's backend,
// through the control plane); it is bookkeeping, like a task's owner, and never
// an authorization claim — nothing in the ledger grants a tool or a merge on
// the strength of it.
//
// The request's own id is its idempotency key: the same id with the same task
// and text returns the stored row (replayed), and the same id with different
// content is a conflict rather than a second row. That makes the app's retry of
// an answer it never saw safe.
func (s *sessionDB) appendLedgerRequest(ctx context.Context, id, taskID, text, author, choiceID string, now time.Time) (ledgerRequest, bool, error) {
	if !ledgerID.MatchString(id) || !ledgerID.MatchString(taskID) {
		return ledgerRequest{}, false, ledgerFail("invalid_argument", "id and task_id must be 1..128 letters, digits, _ or -")
	}
	if strings.TrimSpace(text) == "" || len(text) > requestTextMax {
		return ledgerRequest{}, false, ledgerFail("invalid_argument", "text must be 1.."+fmt.Sprint(requestTextMax)+" characters")
	}
	if !ledgerID.MatchString(author) {
		return ledgerRequest{}, false, ledgerFail("invalid_argument", "author must be 1..128 letters, digits, _ or -")
	}
	if choiceID != "" && !ledgerID.MatchString(choiceID) {
		return ledgerRequest{}, false, ledgerFail("invalid_argument", "choice_id must be 1..128 letters, digits, _ or -")
	}
	db, err := s.handle(ctx)
	if err != nil {
		return ledgerRequest{}, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ledgerRequest{}, false, err
	}
	defer tx.Rollback()
	if err = migrateLedger(ctx, tx); err != nil {
		return ledgerRequest{}, false, err
	}
	if existing, err := readLedgerRequest(ctx, tx, id); err == nil {
		// Already stored. Same content is the caller's retry; different content
		// under a used id is a mistake, not a second request.
		if existing.TaskID != taskID || existing.Text != text {
			return ledgerRequest{}, false, ledgerFail("conflict", "request id already used with different content")
		}
		return existing, true, tx.Commit()
	} else if !errors.Is(err, errLedgerRequestMissing) {
		return ledgerRequest{}, false, err
	}
	// The task must exist on this slot: a request is about one task, and a
	// request about a task this ledger does not have would never be found by
	// the parent. A retired (deleted) id is refused for the same reason.
	var taskCount, retired int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_ledger_tasks WHERE id=?", taskID).Scan(&taskCount); err != nil {
		return ledgerRequest{}, false, err
	}
	if taskCount == 0 {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_ledger_retired WHERE id=?", taskID).Scan(&retired); err != nil {
			return ledgerRequest{}, false, err
		}
		if retired != 0 {
			return ledgerRequest{}, false, ledgerFail("conflict", "that task was deleted")
		}
		return ledgerRequest{}, false, ledgerFail("not_found", "task not found")
	}
	var seq int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0)+1 FROM sp_ledger_user_requests").Scan(&seq); err != nil {
		return ledgerRequest{}, false, err
	}
	req := ledgerRequest{
		ID: id, TaskID: taskID, Seq: seq, Version: 1, Text: text, Author: author,
		CreatedAt: now.UTC().Format(time.RFC3339), Status: requestPending, ChoiceID: choiceID,
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sp_ledger_user_requests
		 (id,task_id,seq,version,text,author,created_at,status,claim_owner,claim_token,claim_expires_at,
		  operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at,choice_id)
		 VALUES (?,?,?,?,?,?,?,?,'','','','','','','',NULL,'','',?)`,
		req.ID, req.TaskID, req.Seq, req.Version, req.Text, req.Author, req.CreatedAt, req.Status, req.ChoiceID)
	if err != nil {
		return ledgerRequest{}, false, err
	}
	// One history line per stored request, in the same transaction: the person's
	// own words, which are theirs to read back.
	if err = recordTaskEvent(ctx, tx, ledgerTaskEvent{
		TaskID: taskID, Kind: eventRequestStored, Actor: author,
		Summary: "요청을 남겼다", RequestID: id,
		Detail: map[string]any{"text": text, "seq": seq, "choice_id": choiceID},
	}, now); err != nil {
		return ledgerRequest{}, false, err
	}
	return req, false, tx.Commit()
}

var errLedgerRequestMissing = errors.New("request not found")

// ledgerNow is the clock the request lifecycle reads. A claim's lapse is a
// time comparison, so a test has to be able to move time rather than wait for
// it; nothing else replaces it.
var ledgerNow = time.Now

func readLedgerRequest(ctx context.Context, tx *sql.Tx, id string) (ledgerRequest, error) {
	var r ledgerRequest
	var relatedSeq sql.NullInt64
	var claimToken string
	err := tx.QueryRowContext(ctx,
		`SELECT id,task_id,seq,version,text,author,created_at,status,claim_owner,claim_token,
		        claim_expires_at,operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at,
		        coalesce(choice_id,'')
		 FROM sp_ledger_user_requests WHERE id=?`, id).
		Scan(&r.ID, &r.TaskID, &r.Seq, &r.Version, &r.Text, &r.Author, &r.CreatedAt, &r.Status, &r.ClaimOwner, &claimToken,
			&r.ClaimExpiresAt, &r.OperationKey, &r.OperationTarget, &r.DispatchState, &r.DispatchedAt, &relatedSeq, &r.Result,
			&r.SettledAt, &r.ChoiceID)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errLedgerRequestMissing
	}
	if err != nil {
		return r, err
	}
	if relatedSeq.Valid {
		seq := relatedSeq.Int64
		r.RelatedSeq = &seq
	}
	return r, nil
}

// ledgerRequestSummaries counts what is waiting per task, for the ids given.
func ledgerRequestSummaries(ctx context.Context, tx *sql.Tx, taskIDs []string) (map[string]*ledgerRequestSummary, error) {
	out := map[string]*ledgerRequestSummary{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(taskIDs)), ",")
	args := make([]any, 0, len(taskIDs))
	for _, id := range taskIDs {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT task_id,status,count(*),max(seq) FROM sp_ledger_user_requests
		 WHERE task_id IN (`+placeholders+`) GROUP BY task_id,status`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, status string
		var count, lastSeq int64
		if err := rows.Scan(&taskID, &status, &count, &lastSeq); err != nil {
			return nil, err
		}
		summary := out[taskID]
		if summary == nil {
			summary = &ledgerRequestSummary{}
			out[taskID] = summary
		}
		switch status {
		case requestPending:
			summary.Pending += int(count)
		case requestClaimed:
			summary.Claimed += int(count)
		case requestReconciling:
			summary.Reconciling += int(count)
		}
		if lastSeq > summary.LastSeq {
			summary.LastSeq = lastSeq
		}
	}
	return out, rows.Err()
}

// ledgerRequests runs one of the parent's request tools inside the ledger's own
// transaction, so a claim and its receipt commit together.
func (s *sessionDB) ledgerRequests(ctx context.Context, tx *sql.Tx, name string, a ledgerArgs, now time.Time) (any, error) {
	if name == "ledger_requests_list" {
		return listLedgerRequests(ctx, tx, a)
	}
	// Mutations replay their first committed answer, the same contract as
	// ledger_put: a parent that lost the response can retry without acting
	// twice.
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
	current, err := readLedgerRequest(ctx, tx, a.ID)
	if errors.Is(err, errLedgerRequestMissing) {
		return nil, ledgerFail("not_found", "request not found")
	}
	if err != nil {
		return nil, err
	}
	if current.Version != *a.ExpectedVersion {
		return nil, ledgerFail("conflict", "expected_version does not match")
	}
	var result any
	switch name {
	case "ledger_requests_claim":
		result, err = claimLedgerRequest(ctx, tx, current, a, now)
	case "ledger_requests_dispatch":
		result, err = dispatchLedgerRequest(ctx, tx, current, a, now)
	case "ledger_requests_ack":
		result, err = ackLedgerRequest(ctx, tx, current, a, now)
	default:
		return nil, ledgerFail("invalid_argument", "unknown ledger tool")
	}
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO sp_ledger_requests VALUES (?,?,?)", a.RequestID, fingerprint, string(encoded)); err != nil {
		return nil, err
	}
	return result, nil
}

func listLedgerRequests(ctx context.Context, tx *sql.Tx, a ledgerArgs) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = requestListLimitDefault
	}
	after := int64(0)
	if a.AfterSeq != nil {
		after = *a.AfterSeq
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM sp_ledger_user_requests
		 WHERE seq>? AND (?='' OR task_id=?) AND (?='' OR status=?)
		 ORDER BY seq LIMIT ?`, after, a.TaskID, a.TaskID, a.Status, a.Status, limit+1)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []ledgerRequest{}
	next := int64(0)
	for i, id := range ids {
		if i == limit {
			next = out[len(out)-1].Seq
			break
		}
		req, err := readLedgerRequest(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	result := map[string]any{"requests": out}
	if next != 0 {
		result["next_cursor"] = next
	}
	return result, nil
}

// claimLedgerRequest hands the request to one parent. A pending request is
// claimed outright. A claim that has lapsed is a different thing: if the holder
// had recorded no dispatch, nothing can have happened outside and the request
// is simply claimed again; if it had, the request becomes a reconciliation —
// the new holder is told what was dispatched and must establish what landed.
func claimLedgerRequest(ctx context.Context, tx *sql.Tx, current ledgerRequest, a ledgerArgs, now time.Time) (any, error) {
	reconcile := false
	switch current.Status {
	case requestPending:
	case requestClaimed, requestReconciling:
		expiry, err := time.Parse(time.RFC3339, current.ClaimExpiresAt)
		if err != nil || now.Before(expiry) {
			return nil, ledgerFail("conflict", "another parent holds this request")
		}
		reconcile = current.DispatchState == dispatchIntended || current.DispatchState == dispatchDispatched
	default:
		return nil, ledgerFail("conflict", "request is already "+current.Status)
	}
	ttl := requestClaimTTLDefault
	if a.TTLSeconds != nil {
		ttl = time.Duration(*a.TTLSeconds) * time.Second
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	status := requestClaimed
	if reconcile {
		status = requestReconciling
	}
	next := current
	next.Version++
	next.Status = status
	next.ClaimOwner = a.ClaimOwner
	next.ClaimExpiresAt = now.Add(ttl).UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE sp_ledger_user_requests SET version=?,status=?,claim_owner=?,claim_token=?,claim_expires_at=?
		 WHERE id=? AND version=?`,
		next.Version, next.Status, next.ClaimOwner, token, next.ClaimExpiresAt, current.ID, current.Version); err != nil {
		return nil, err
	}
	if err := recordTaskEvent(ctx, tx, ledgerTaskEvent{
		TaskID: current.TaskID, Kind: eventRequestClaimed, Actor: a.ClaimOwner,
		Summary: "요청을 접수했다", RequestID: current.ID,
		Detail: map[string]any{"reconcile": reconcile, "status": status},
	}, now); err != nil {
		return nil, err
	}
	out := map[string]any{"request": next, "claim_token": token}
	if reconcile {
		// Say plainly that something may already be out there, and what it was.
		out["reconcile"] = true
		out["reconcile_reason"] = "the previous claim lapsed after recording a dispatch; establish what landed before sending anything again"
	}
	return out, nil
}

// dispatchLedgerRequest records the parent's intent before it acts. On a
// reconciling request a new operation key is refused unless the parent says it
// checked and the earlier dispatch never landed — a lapsed claim alone is not
// evidence, and re-sending on it would be the duplicate the whole record exists
// to prevent.
func dispatchLedgerRequest(ctx context.Context, tx *sql.Tx, current ledgerRequest, a ledgerArgs, now time.Time) (any, error) {
	if err := heldClaim(ctx, tx, current, a.ClaimToken, now); err != nil {
		return nil, err
	}
	if current.Status != requestClaimed && current.Status != requestReconciling {
		return nil, ledgerFail("conflict", "request is "+current.Status)
	}
	if current.Status == requestReconciling && current.OperationKey != "" && current.OperationKey != a.OperationKey && a.Observed != "not_accepted" {
		return nil, ledgerFail("conflict",
			"this request already dispatched "+current.OperationKey+
				"; reuse that operation_key, or pass observed=not_accepted once you have established it never landed")
	}
	next := current
	next.Version++
	next.OperationKey = a.OperationKey
	next.OperationTarget = a.OperationTarget
	next.DispatchState = a.DispatchState
	next.DispatchedAt = now.UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE sp_ledger_user_requests SET version=?,operation_key=?,operation_target=?,dispatch_state=?,dispatched_at=?
		 WHERE id=? AND version=?`,
		next.Version, next.OperationKey, next.OperationTarget, next.DispatchState, next.DispatchedAt, current.ID, current.Version); err != nil {
		return nil, err
	}
	// The key and the target, never a token: the history says what was handed
	// over, which is what a reader needs to reconcile it.
	if err := recordTaskEvent(ctx, tx, ledgerTaskEvent{
		TaskID: current.TaskID, Kind: eventRequestDispatched, Actor: current.ClaimOwner,
		Summary: "요청을 처리하려고 " + a.DispatchState + " 로 적었다", RequestID: current.ID,
		Detail: map[string]any{
			"dispatch_state": a.DispatchState, "operation_key": a.OperationKey,
			"operation_target": a.OperationTarget, "observed": a.Observed,
		},
	}, now); err != nil {
		return nil, err
	}
	return map[string]any{"request": next}, nil
}

// ackLedgerRequest closes the request with what happened. It is the only way to
// processed or failed: a list, a claim or a dispatch never settles anything, so
// a request that was looked at and dropped stays visibly unfinished.
func ackLedgerRequest(ctx context.Context, tx *sql.Tx, current ledgerRequest, a ledgerArgs, now time.Time) (any, error) {
	if err := heldClaim(ctx, tx, current, a.ClaimToken, now); err != nil {
		return nil, err
	}
	if current.Status != requestClaimed && current.Status != requestReconciling {
		return nil, ledgerFail("conflict", "request is "+current.Status)
	}
	next := current
	next.Version++
	next.Status = a.Outcome
	next.Result = a.Result
	next.RelatedSeq = a.RelatedSeq
	next.DispatchState = dispatchSettled
	next.SettledAt = now.UTC().Format(time.RFC3339)
	var related any
	if a.RelatedSeq != nil {
		related = *a.RelatedSeq
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sp_ledger_user_requests SET version=?,status=?,result=?,related_seq=?,dispatch_state=?,settled_at=?,
		        claim_token='',claim_expires_at=''
		 WHERE id=? AND version=?`,
		next.Version, next.Status, next.Result, related, next.DispatchState, next.SettledAt, current.ID, current.Version); err != nil {
		return nil, err
	}
	next.ClaimExpiresAt = ""
	if err := recordTaskEvent(ctx, tx, ledgerTaskEvent{
		TaskID: current.TaskID, Kind: eventRequestSettled, Actor: current.ClaimOwner,
		Summary: "요청을 " + a.Outcome + " 로 닫았다", RequestID: current.ID,
		Detail: map[string]any{"outcome": a.Outcome, "result": a.Result, "related_seq": related},
	}, now); err != nil {
		return nil, err
	}
	return map[string]any{"request": next}, nil
}

// heldClaim refuses a token that is not the current holder's and a claim that
// has lapsed. Both matter after a restart: a parent that comes back late must
// not be able to report over the parent that took the request meanwhile.
func heldClaim(ctx context.Context, tx *sql.Tx, current ledgerRequest, token string, now time.Time) error {
	var stored string
	if err := tx.QueryRowContext(ctx, "SELECT claim_token FROM sp_ledger_user_requests WHERE id=?", current.ID).Scan(&stored); err != nil {
		return err
	}
	if stored == "" || stored != token {
		return ledgerFail("conflict", "that claim_token does not hold this request")
	}
	expiry, err := time.Parse(time.RFC3339, current.ClaimExpiresAt)
	if err != nil || !now.Before(expiry) {
		return ledgerFail("conflict", "this claim has lapsed; claim the request again")
	}
	return nil
}

func newClaimToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// attachRequestSummaries fills in each record's waiting-request counts. It is
// one query for the whole page, so showing a parent that work arrived costs the
// same whether the page has one task or a hundred.
func attachRequestSummaries(ctx context.Context, tx *sql.Tx, records []*ledgerRecord) error {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	summaries, err := ledgerRequestSummaries(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, r := range records {
		if summary, ok := summaries[r.ID]; ok {
			r.Requests = summary
		}
	}
	return nil
}
