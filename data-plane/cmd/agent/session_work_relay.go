// The relay between the two lanes of one manager (AC-N11): the parent that
// talks to the person, and the monitor that runs the work.
//
// They are two execution contexts in the same pod, each with its own
// conversation, cursor and tool surface, so neither can speak into the other's
// turn. What they need is a place to hand something over — an instruction or a
// decision from the parent, a result or a question back from the monitor — that
// survives the parent still writing a long answer, the monitor's turn ending,
// and either lane's container restarting. The conversation is none of those
// things: it is in a pod's memory, it is consumed by being read, and it has no
// way to say "this was dealt with".
//
// So the handover is a record, in the slot's own database, beside the work it is
// about. One row per message, holding who sent it, who it is for, what it says
// and — the part a sender actually asks about — whether the other lane has dealt
// with it yet.
//
// Three properties are the whole point, and each is a mechanism rather than a
// rule in a skill:
//
//   - **A resend is not a second message.** Every send carries the sender's own
//     request_id, and the receipt contract the work records already have returns
//     the first committed answer for an identical retry and refuses a reused id
//     with different words. A lane whose answer was lost resends the same call
//     and learns what it already did.
//   - **Exactly one lane runs a message, once.** Taking one is a compare-and-swap
//     that mints a claim token; the holder is the only caller whose ack can
//     settle it, and a settled message is never claimable again. Two claims on
//     one message leave one winner and one conflict.
//   - **A restart loses no message and repeats none silently.** A claim records
//     the incarnation that took it. A pod has one lane per role, so a call from a
//     new incarnation of that role is proof the earlier one is gone: its claim is
//     recoverable, and the recovering caller is *told* (`recovered`, `attempts`)
//     rather than handed a clean slate it would read as a first attempt.
//
// What this is not: external exactly-once. A message says what the other lane
// should do; whether the outside action behind it happened once is the outside
// system's business, keyed on the operation key the turn records before acting
// (session_work_turns.go). The relay guarantees one holder, one settle and no
// silent repeat — and it says so to the caller instead of implying more.
package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	relaySendTool   = "relay_send"
	relayListTool   = "relay_list"
	relayClaimTool  = "relay_claim"
	relayAckTool    = "relay_ack"
	relayStatusTool = "relay_status"

	// The two lanes of a manager. They are also the two values of the role a
	// lane's MCP registration carries (session_db_mcp.go): a sender is never an
	// argument, because a lane that could name itself could speak as the other.
	relayLaneParent  = "parent"
	relayLaneMonitor = "monitor"

	// What a message is. The kinds are the three things the lanes actually hand
	// over, and they are a closed set so a reader can tell a standing
	// instruction from an answer to one question.
	relayKindInstruction = "instruction"
	relayKindDecision    = "decision"
	relayKindResult      = "result"
	relayKindQuestion    = "question"

	// Where a message stands. `pending` is waiting, `claimed` is taken by a
	// lane that has not reported back, and `processed`/`failed` are settled —
	// the two answers a sender's "has this been dealt with" can end on.
	relayStatePending   = "pending"
	relayStateClaimed   = "claimed"
	relayStateProcessed = "processed"
	relayStateFailed    = "failed"

	// relaySubjectMax and relayBodyMax bound one message for the reason a
	// task's text is bounded: the slot's storage budget is shared with the
	// session's own archive, so a message is a message rather than a transcript.
	// A long answer belongs in a turn's `response`, which the message can name.
	relaySubjectMax = 512
	relayBodyMax    = 16384
	relayResultMax  = 16384
)

// workCaller is who is calling this database: which lane of the manager, and
// which incarnation of that lane. Both are configuration — the lane's MCP
// registration, rendered by the platform on every boot — rather than arguments,
// which is what makes them an identity the model cannot assert
// (session_db_mcp.go). An empty lane is a manager running a single lane, which
// is every slot provisioned before the pair.
type workCaller struct {
	lane        string
	incarnation string
}

var relayKinds = []string{relayKindInstruction, relayKindDecision, relayKindResult, relayKindQuestion}

var relayLanes = []string{relayLaneParent, relayLaneMonitor}

func isRelayKind(s string) bool { return containsString(relayKinds, s) }

func isRelayLane(s string) bool { return containsString(relayLanes, s) }

// relayOtherLane is who a lane's message is for. It is derived, never given: a
// manager has two lanes, so "the other one" is the whole addressing scheme and
// there is nothing for a caller to aim somewhere else.
func relayOtherLane(lane string) string {
	if lane == relayLaneParent {
		return relayLaneMonitor
	}
	return relayLaneParent
}

// relayMessage is one handover as both lanes read it.
//
// Attempts is how many times it has been claimed, which is the honest answer to
// "has anyone started on this before": a recovered claim does not reset it, so a
// monitor picking up after a restart can see that an earlier incarnation already
// had it and reconcile before repeating an outside action.
type relayMessage struct {
	ID        string  `json:"id"`
	Seq       int64   `json:"seq"`
	Sender    string  `json:"sender"`
	Recipient string  `json:"recipient"`
	Kind      string  `json:"kind"`
	Subject   string  `json:"subject"`
	Body      string  `json:"body"`
	TaskID    *string `json:"task_id"`
	State     string  `json:"state"`
	Result    *string `json:"result"`
	// Version is the compare-and-swap value a claim has to hold. It moves on
	// every claim and on the settle, so a caller that read a message before
	// another lane touched it is told to read again rather than winning.
	Version   int64   `json:"version"`
	Attempts  int64   `json:"attempts"`
	SentAt    string  `json:"sent_at"`
	ClaimedAt *string `json:"claimed_at"`
	SettledAt *string `json:"settled_at"`
}

// relayInput is a message as the sender gives it. Sender and recipient are not
// here on purpose — see relayOtherLane.
type relayInput struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	TaskID  string `json:"task_id,omitempty"`
}

func relayToolDefinitions(lane string) []map[string]any {
	if lane == "" {
		// An unpaired manager has no other lane to hand anything to, and a tool
		// whose only possible answer is a refusal is one the model should not be
		// shown.
		return nil
	}
	other := relayOtherLane(lane)
	str := func() map[string]any { return map[string]any{"type": "string"} }
	message := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "enum": relayKinds,
				"description": "instruction (do this), decision (the person chose this), result (what came of it) or question (the other lane has to answer before going on)."},
			"subject": map[string]any{"type": "string", "description": "One line naming what this is about (1..512 characters)."},
			"body":    map[string]any{"type": "string", "description": "The message itself (1..16384 characters). A long answer belongs in a turn's response, which this can name instead of carrying."},
			"task_id": map[string]any{"type": "string", "description": "The work task this is about, when it is about one. Omitted is a message about the manager itself."},
		},
		"required": []string{"kind", "subject", "body"}, "additionalProperties": false,
	}
	return []map[string]any{
		{
			"name": relaySendTool,
			"description": "Hands one message to the " + other + " lane of this manager. You are the " + lane +
				" lane: the recipient is the other lane and cannot be chosen. The message waits in the database, so it survives the other lane being mid-turn, its turn ending and either lane restarting — nothing is sent into a conversation and no turn starts. " +
				"request_id makes a resend safe: the same id with the same message returns the answer that was committed the first time, and the same id with a different message is a conflict rather than a second message. Reusing a message id for different words is also a conflict.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "message": message,
			}, "required": []string{"id", "request_id", "message"}, "additionalProperties": false},
		},
		{
			"name": relayListTool,
			"description": "Lists messages, oldest seq first, and changes nothing — listing is not taking. box=inbox (default) is what the " + other +
				" lane sent you; box=outbox is what you sent it. open=true selects the ones still to be dealt with (pending, or claimed by an incarnation that is gone). after_seq and limit (1..100, default 20) page it.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"box":  map[string]any{"type": "string", "enum": []string{"inbox", "outbox"}, "description": "inbox (addressed to you, the default) or outbox (sent by you)."},
				"open": map[string]any{"type": "boolean", "description": "true selects only what still needs dealing with."},
				"after_seq": map[string]any{"type": "integer", "minimum": 0,
					"description": "Page cursor: the seq of the last message of the previous page."},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": workListLimitMax, "default": workListLimitDefault},
			}, "required": []string{}, "additionalProperties": false},
		},
		{
			"name": relayClaimTool,
			"description": "Takes one message addressed to you so that you are the lane working it. expected_version must be the version you read, so of two callers holding the same version exactly one wins and the other is told to read again. " +
				"The reply carries a claim_token that " + relayAckTool + " requires, and attempts — how many times this message has been claimed. A message a gone incarnation still held is recoverable: the reply says recovered=true with that incarnation, which means an earlier attempt may have acted outside this database and you establish what happened before repeating it. A settled message is never claimable.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(),
				"expected_version": map[string]any{"type": "integer", "minimum": 1},
			}, "required": []string{"id", "request_id", "expected_version"}, "additionalProperties": false},
		},
		{
			"name": relayAckTool,
			"description": "Settles the message you hold as processed or failed, with the result the sender reads. It needs the claim_token from your own claim: a token from a claim that was taken over is refused, so a lane that comes back late cannot report over the one working it now. Settling is final — the sender's " +
				relayStatusTool + " answers from it — and a second ack with the same request_id replays the first answer rather than settling twice.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "claim_token": str(),
				"outcome": map[string]any{"type": "string", "enum": []string{relayStateProcessed, relayStateFailed},
					"description": "processed or failed. Both are answers; a message looked at and dropped stays visibly open instead."},
				"result": map[string]any{"type": "string", "description": "What the sender reads about it (at most 16384 characters)."},
			}, "required": []string{"id", "request_id", "claim_token", "outcome"}, "additionalProperties": false},
		},
		{
			"name":        relayStatusTool,
			"description": "Whether one message has been dealt with: its state, how many times it was claimed, the result if it is settled, and when. This is what a sender asks instead of guessing from the conversation, and it changes nothing. Either lane may ask about a message of this manager; a message of neither lane does not exist here.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": str()},
				"required": []string{"id"}, "additionalProperties": false},
		},
	}
}

var relayTools = []string{relaySendTool, relayListTool, relayClaimTool, relayAckTool, relayStatusTool}

func isRelayTool(name string) bool { return containsString(relayTools, name) }

// isRelayMutation names the relay calls whose answer is replayed from a receipt.
// A claim is one of them: a claim whose answer was lost must return the same
// token rather than a second claim on the same message.
func isRelayMutation(name string) bool {
	return name == relaySendTool || name == relayClaimTool || name == relayAckTool
}

func validateRelayTool(name string, a workArgs) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if name == relayListTool {
		if a.Limit < 0 || a.Limit > workListLimitMax {
			return bad("invalid pagination")
		}
		if a.AfterSeq != nil && (*a.AfterSeq < 0 || *a.AfterSeq > requestSafeInt) {
			return bad("after_seq must be a safe nonnegative integer")
		}
		if a.Box != "" && a.Box != "inbox" && a.Box != "outbox" {
			return bad("box must be inbox or outbox")
		}
		return nil
	}
	if !workID.MatchString(a.ID) {
		return bad("id must be 1..128 letters, digits, _ or -")
	}
	switch name {
	case relayStatusTool:
		return nil
	case relaySendTool:
		if a.Message == nil {
			return bad("message required")
		}
		if !isRelayKind(a.Message.Kind) {
			return bad("kind must be one of " + strings.Join(relayKinds, ", "))
		}
		if strings.TrimSpace(a.Message.Subject) == "" || len(a.Message.Subject) > relaySubjectMax {
			return bad("subject must be 1..512 characters")
		}
		if strings.TrimSpace(a.Message.Body) == "" || len(a.Message.Body) > relayBodyMax {
			return bad("body must be 1..16384 characters — a long answer belongs in a turn's response")
		}
		if a.Message.TaskID != "" && !workID.MatchString(a.Message.TaskID) {
			return bad("task_id must be 1..128 letters, digits, _ or -")
		}
		return nil
	case relayClaimTool:
		if a.ExpectedVersion == nil || *a.ExpectedVersion < 1 || *a.ExpectedVersion > requestSafeInt {
			return bad("expected_version must be the version you read")
		}
		return nil
	case relayAckTool:
		if a.ClaimToken == "" {
			return bad("claim_token required — it is what proves this message is yours to settle")
		}
		if a.Outcome != relayStateProcessed && a.Outcome != relayStateFailed {
			return bad("outcome must be " + relayStateProcessed + " or " + relayStateFailed)
		}
		if len(a.Result) > relayResultMax {
			return bad("result must be at most 16384 characters")
		}
		return nil
	}
	return bad("unknown relay tool")
}

// runRelayTool runs one relay call inside the caller's transaction. lane is the
// caller's own, taken from its MCP registration and never from the arguments.
func runRelayTool(ctx context.Context, tx *sql.Tx, name, lane, incarnation string, a workArgs, now time.Time) (any, error) {
	if !isRelayLane(lane) {
		return nil, workFail("invalid_argument",
			"this session database is not serving a manager lane, so it has no other lane to hand a message to")
	}
	switch name {
	case relaySendTool:
		return sendRelayMessage(ctx, tx, lane, a, now)
	case relayListTool:
		return listRelayMessages(ctx, tx, lane, a)
	case relayClaimTool:
		return claimRelayMessage(ctx, tx, lane, incarnation, a, now)
	case relayAckTool:
		return ackRelayMessage(ctx, tx, lane, a, now)
	case relayStatusTool:
		return readRelayStatus(ctx, tx, lane, a.ID)
	}
	return nil, workFail("invalid_argument", "unknown relay tool")
}

func sendRelayMessage(ctx context.Context, tx *sql.Tx, lane string, a workArgs, now time.Time) (any, error) {
	existing, err := readRelayMessage(ctx, tx, a.ID)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	case err == nil:
		// The receipt above this already replayed an identical resend, so
		// reaching here with the id taken means different words under the same
		// name. Overwriting would make the recipient's claim refer to something
		// it never read.
		if existing.Sender != lane {
			return nil, workFail("conflict", "that message id belongs to a message the other lane sent")
		}
		return nil, workFail("conflict", "that message id is already used for different words; send the new message under an id of its own")
	}
	if a.Message.TaskID != "" {
		var present int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_tasks WHERE id=?", a.Message.TaskID).Scan(&present); err != nil {
			return nil, err
		}
		if present == 0 {
			// A message about a task nobody can look up is a message the
			// recipient cannot act on, and silently dropping the link would hide
			// that the sender meant a task at all.
			return nil, workFail("not_found", "this slot has no work task with that id")
		}
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM sp_work_relay").Scan(&seq); err != nil {
		return nil, err
	}
	stamp := workNow(now)
	var taskID any
	if a.Message.TaskID != "" {
		taskID = a.Message.TaskID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_relay
		 (id,seq,sender,recipient,kind,subject,body,task_id,state,result,version,attempts,sent_at,claimed_at,settled_at)
		 VALUES (?,?,?,?,?,?,?,?,?,NULL,1,0,?,NULL,NULL)`,
		a.ID, seq, lane, relayOtherLane(lane), a.Message.Kind, a.Message.Subject, a.Message.Body, taskID,
		relayStatePending, stamp); err != nil {
		return nil, err
	}
	return readRelayMessage(ctx, tx, a.ID)
}

func listRelayMessages(ctx context.Context, tx *sql.Tx, lane string, a workArgs) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = workListLimitDefault
	}
	after := int64(0)
	if a.AfterSeq != nil {
		after = *a.AfterSeq
	}
	column := "recipient"
	if a.Box == "outbox" {
		column = "sender"
	}
	query := `SELECT ` + relayColumns + ` FROM sp_work_relay WHERE ` + column + `=? AND seq>?`
	args := []any{lane, after}
	if a.Open != nil && *a.Open {
		query += " AND state IN (?,?)"
		args = append(args, relayStatePending, relayStateClaimed)
	}
	query += " ORDER BY seq LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relayMessage{}
	for rows.Next() {
		row, err := scanRelayMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var cursor int64
	if len(out) > limit {
		out = out[:limit]
		cursor = out[len(out)-1].Seq
	}
	return map[string]any{"messages": out, "next_cursor": cursor}, nil
}

// claimRelayMessage takes one message. The swap is a single conditional UPDATE
// whose row count is checked, so two lanes holding the same version cannot both
// win — and because the token is minted here rather than derived from the row,
// a caller that merely re-read the version cannot present one.
func claimRelayMessage(ctx context.Context, tx *sql.Tx, lane, incarnation string, a workArgs, now time.Time) (any, error) {
	row, err := readRelayMessage(ctx, tx, a.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workFail("not_found", "this manager has no relay message with that id")
	}
	if err != nil {
		return nil, err
	}
	if row.Recipient != lane {
		return nil, workFail("conflict", "that message is addressed to the "+row.Recipient+" lane; you are the "+lane+" lane")
	}
	if row.State == relayStateProcessed || row.State == relayStateFailed {
		return nil, workFail("conflict", "that message is already settled as "+row.State+"; a settled message is not claimed again")
	}
	prior, priorErr := readRelayClaim(ctx, tx, a.ID)
	if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
		return nil, priorErr
	}
	recovered := false
	if priorErr == nil {
		if prior.incarnation == incarnation {
			// The same incarnation still holds its own token in memory. Handing
			// out a second one would let one lane report twice on one message.
			return nil, workFail("conflict", "you already hold this message; ack it with the claim token you were given")
		}
		recovered = true
	}
	token, err := newClaimToken()
	if err != nil {
		return nil, err
	}
	stamp := workNow(now)
	result, err := tx.ExecContext(ctx,
		`UPDATE sp_work_relay SET state=?, version=version+1, attempts=attempts+1, claimed_at=?
		 WHERE id=? AND version=?`,
		relayStateClaimed, stamp, a.ID, *a.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, workFail("conflict", "that message moved under you; read it again and claim the version it has now")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_relay_claim (message_id,claim_token,incarnation,claimed_at) VALUES (?,?,?,?)
		 ON CONFLICT(message_id) DO UPDATE SET claim_token=excluded.claim_token,
		   incarnation=excluded.incarnation, claimed_at=excluded.claimed_at`,
		a.ID, token, incarnation, stamp); err != nil {
		return nil, err
	}
	taken, err := readRelayMessage(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}
	answer := map[string]any{"message": taken, "claim_token": token, "attempts": taken.Attempts, "recovered": recovered}
	if recovered {
		answer["prior_incarnation"] = prior.incarnation
		answer["prior_claimed_at"] = prior.claimedAt
	}
	return answer, nil
}

func ackRelayMessage(ctx context.Context, tx *sql.Tx, lane string, a workArgs, now time.Time) (any, error) {
	row, err := readRelayMessage(ctx, tx, a.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workFail("not_found", "this manager has no relay message with that id")
	}
	if err != nil {
		return nil, err
	}
	if row.Recipient != lane {
		return nil, workFail("conflict", "that message is addressed to the "+row.Recipient+" lane; you are the "+lane+" lane")
	}
	if row.State != relayStateClaimed {
		return nil, workFail("conflict", "that message is "+row.State+"; only a message you hold is settled")
	}
	held, err := readRelayClaim(ctx, tx, a.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workFail("conflict", "that message is not held by anyone; claim it before settling it")
	}
	if err != nil {
		return nil, err
	}
	if held.token != a.ClaimToken {
		return nil, workFail("conflict", "that claim token is not the one holding this message; it was taken over")
	}
	stamp := workNow(now)
	var result any
	if a.Result != "" {
		result = a.Result
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE sp_work_relay SET state=?, result=?, version=version+1, settled_at=? WHERE id=? AND version=?",
		a.Outcome, result, stamp, a.ID, row.Version); err != nil {
		return nil, err
	}
	// The token goes with the settle: a message that is answered has no holder,
	// so a late repeat of the same ack is refused by state rather than by a
	// token that would otherwise still match.
	if _, err := tx.ExecContext(ctx, "DELETE FROM sp_work_relay_claim WHERE message_id=?", a.ID); err != nil {
		return nil, err
	}
	return readRelayMessage(ctx, tx, a.ID)
}

// readRelayStatus is the sender's question. It is answered from the row rather
// than from a separate report for the reason the schema gate is: a record and
// the thing it describes cannot drift apart.
func readRelayStatus(ctx context.Context, tx *sql.Tx, lane, id string) (any, error) {
	row, err := readRelayMessage(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workFail("not_found", "this manager has no relay message with that id")
	}
	if err != nil {
		return nil, err
	}
	if row.Sender != lane && row.Recipient != lane {
		return nil, workFail("not_found", "this manager has no relay message with that id")
	}
	return map[string]any{
		"message":    row,
		"settled":    row.State == relayStateProcessed || row.State == relayStateFailed,
		"dealt_with": row.State == relayStateProcessed,
	}, nil
}

const relayColumns = `id,seq,sender,recipient,kind,subject,body,task_id,state,result,version,attempts,sent_at,claimed_at,settled_at`

type relayRowScanner interface {
	Scan(dest ...any) error
}

func scanRelayMessage(scanner relayRowScanner) (relayMessage, error) {
	var row relayMessage
	err := scanner.Scan(&row.ID, &row.Seq, &row.Sender, &row.Recipient, &row.Kind, &row.Subject, &row.Body,
		&row.TaskID, &row.State, &row.Result, &row.Version, &row.Attempts, &row.SentAt, &row.ClaimedAt, &row.SettledAt)
	return row, err
}

func readRelayMessage(ctx context.Context, tx *sql.Tx, id string) (relayMessage, error) {
	return scanRelayMessage(tx.QueryRowContext(ctx, "SELECT "+relayColumns+" FROM sp_work_relay WHERE id=?", id))
}

// relayClaim is who holds a message. It lives beside the message rather than in
// it: a claim token is internal management, and it never appears in a read a
// person or the app gets.
type relayClaim struct {
	token       string
	incarnation string
	claimedAt   string
}

func readRelayClaim(ctx context.Context, tx *sql.Tx, id string) (relayClaim, error) {
	var c relayClaim
	err := tx.QueryRowContext(ctx,
		"SELECT claim_token,incarnation,claimed_at FROM sp_work_relay_claim WHERE message_id=?", id).
		Scan(&c.token, &c.incarnation, &c.claimedAt)
	return c, err
}
