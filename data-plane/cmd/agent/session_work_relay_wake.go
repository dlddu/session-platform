// Telling the monitor lane that a person has answered (AC-N14).
//
// A task that is waiting on a person is, by the monitor's own policy, a task it
// stops looking at: there is nothing to observe until the person acts, so
// polling it would be nagging. The consequence was that when the person finally
// did act — picked one of the sentences the session offered, or wrote their own,
// and pressed 요청 등록 — the words sat in the ledger until the monitor's next
// scheduled round came past. The work was already decided and nothing was
// running.
//
// So the slot says so, once, at the moment the request lands. The request route
// already writes the turn (session_work_views.go); when that turn is on a task
// that was waiting for a person's judgement, this hands the task and the turn to
// the monitor lane of this very pod over the hop the parent's own handover
// already uses (session_work_lane.go). If the lane is idle its worker starts
// immediately; if it is busy the prompt waits in that lane's queue. There is no
// second lane, no new worker, no new schedule and no timer: the monitor's one
// standing round is untouched and stays the backstop.
//
// What is handed over is the two ids and nothing else. Not the person's words —
// they are in the turn, and the monitor reads the turn. A notification that
// quoted them would be a second copy that can disagree with the record, and the
// whole point of the record is that it is the one.
//
// # Why there is a durable row for something this immediate
//
// Four seams, and none of them is closed by remembering a request_id in memory
// the way the parent's handover does (session_work_lane.go's
// laneAskIdempotencyKept) — that table dies with the sidecar, which is one of
// the seams:
//
//   - **Between the commit and the hop.** The turn is committed before the hop
//     is attempted, because the opposite order would hand over a turn that may
//     not exist. So the intent to hand over is written *inside* the same
//     transaction as the turn: either both are there or neither is.
//   - **Between the hop and its acknowledgement.** The lane can take the prompt
//     and the sidecar die before it records that it did. The claim is leased, so
//     the row becomes deliverable again — and the re-delivery says which attempt
//     it is, so the monitor can tell it may have seen this before.
//   - **A restart of either side.** The sidecar's restart is the lease above.
//     The *lane's* restart is worse: the prompt was queued and is gone with the
//     conversation, and a receipt from a previous run of a lane answers
//     `unknown_run` by design (turn_ticket.go). That answer is the evidence this
//     sweep acts on — it re-delivers exactly when the lane that took the prompt
//     is no longer there.
//   - **The same request sent twice.** The app's own id already makes the store
//     idempotent; this row is keyed by the *turn*, so a replay of that id finds
//     the handover that was already recorded instead of queueing a second turn.
//
// # Taking it once
//
// Two things can act on one request: this delivery and the monitor's standing
// round, which rediscovers the open turn from the ledger regardless. They must
// not both do the work. What makes that safe is not this file — it is the turn
// itself. A turn the monitor has taken has `started_at` written on it under the
// turn lane's own counter, and that is read here narrowly, by task and turn id,
// immediately before every hop: a turn that has been started is never delivered,
// and a row for one is dropped. The monitor's side of the same rule is in the
// delivered prompt and in its skill contract: re-read those two ids, and if the
// turn is already started or ended, do nothing.
//
// A row that has been delivered and whose turn has since been started is
// deleted rather than kept as a "done" row, for the reason a finished
// monitoring loop is deleted (session_work_monitor.go): a row nothing will read
// again is the stale copy this area exists to stop accumulating.
package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

const (
	// The states one handover row is in. `delivering` is a claim rather than a
	// state of the world: it says a hop is in flight, and it is leased because
	// the process holding it can die mid-hop.
	workWakePending    = "pending"
	workWakeDelivering = "delivering"
	workWakeDelivered  = "delivered"

	// workWakeClaimLease is how long a claim is honoured before another pass
	// may take the row back. It is twice one hop's ceiling (laneCallTimeout),
	// so a hop that is merely slow is never raced by a second one; anything
	// longer than that is a sidecar that is no longer running the hop at all.
	workWakeClaimLease = 2 * laneCallTimeout

	// workWakeSweepMax bounds one pass. A person's requests arrive a human
	// round trip apart, so a backlog this size means the lane has been
	// unreachable for a while; the rest stay on the row and the next request —
	// or the monitor's own round, which reads the ledger anyway — picks them
	// up. What is left behind is logged rather than dropped silently.
	workWakeSweepMax = 16

	// The states the request route reports back for one handover. `accepted` is
	// the lane having taken the prompt; `pending` is the intent recorded and
	// the lane not reachable. Neither says the work is done — only the turn
	// ending says that.
	workRelayAccepted = "accepted"
	workRelayPending  = "pending"
)

// workWakeIntent is one handover as the slot keeps it.
type workWakeIntent struct {
	TurnID int64
	TaskID string
	// SourceID is the app's own request id, kept so a reader of this row can
	// line it up with the request the person sent rather than only with the
	// turn it became.
	SourceID string
	State    string
	Attempts int64
	// Ticket is the receipt the lane issued for the prompt it took
	// (turn_ticket.go). It carries the lane's run, which is what lets a later
	// pass ask whether the lane that took this is still there.
	Ticket     string
	RecordedAt string
	ClaimedAt  string
}

// workRequestRelay is what the request route says about the handover. It is
// deliberately not the receipt: a receipt names a run of a lane inside this pod,
// and the reader of this answer is an app outside the session.
type workRequestRelay struct {
	State string `json:"state"`
	// Attempts is how many hops this handover has taken, so a reader can tell a
	// first delivery from a re-delivery after a lost acknowledgement.
	Attempts int64 `json:"attempts"`
}

// wakesMonitorLane reports whether this sidecar has a monitor lane to hand
// anything to. A manager running one lane has none, and that is not a degraded
// mode: the single lane is the one that reads the records, so there is nobody to
// notify. Handing the request to the parent instead would be answering a
// question nobody asked, so nothing is recorded and nothing is reported.
func (s *sessionDB) wakesMonitorLane() bool { return s.monitorLane != nil }

// taskAwaitsPersonsJudgement reports whether a task was waiting on the person
// who just wrote. Only then is there a monitor to unblock: a request on a task
// that is planned, running or completed is an ordinary exchange the monitor
// finds on its own round, and waking it for one would be the nagging this
// avoids.
//
// `blocked` is here because it is the same fact under the older word — the
// status domain carries both (session_work.go), and the readers above this
// treat the old one as an alias for `review`. A row that the review record
// resolves to `running` on evidence (AC-N13's technical_wait) can therefore
// still be delivered; that costs one prompt the monitor no-ops on after reading
// the task, and it is the safe direction to be wrong in. Keep this predicate
// the single place that decides, so the two words cannot drift apart here.
func taskAwaitsPersonsJudgement(status string) bool {
	return status == "review" || status == "blocked"
}

// noteRequestWake records the intent to hand one stored request over, inside the
// transaction that stored it. It reports whether there is a handover to attempt.
//
// Nothing is recorded for a request that needs no handover, which is what makes
// "no delivery" the default rather than a thing to remember to skip: no monitor
// lane, a task that was not waiting on anybody, or a turn that has already been
// dealt with.
func noteRequestWake(
	ctx context.Context, tx *sql.Tx, sourceID string, task workTask, turn workTurn, paired bool, now time.Time,
) (bool, error) {
	if !paired || !taskAwaitsPersonsJudgement(task.Status) {
		return false, nil
	}
	if turn.StartedAt != nil || turn.EndedAt != nil {
		// A replay whose turn the monitor has since picked up. The handover has
		// served its purpose; saying so again would start a turn about work
		// already in hand.
		return false, nil
	}
	stamp := workNow(now)
	// OR IGNORE, so a resend of the same request id finds the handover already
	// recorded with its own attempt count rather than resetting it to a first
	// delivery that never happened.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO sp_work_request_wake (turn_id,task_id,source_id,state,attempts,ticket,recorded_at,claimed_at)
		 VALUES (?,?,?,?,0,'',?,'') ON CONFLICT(turn_id) DO NOTHING`,
		turn.ID, task.ID, sourceID, workWakePending, stamp)
	return err == nil, err
}

// relayRequestWake hands over what is waiting and reports what became of this
// request's own handover.
//
// It runs after the storing transaction has committed, never inside it: the hop
// is a network call, and holding the slot's single connection across one would
// stop every other reader of this database for its duration.
//
// Older rows are swept in the same pass. That is the whole recovery path, and it
// is deliberately not a loop of its own: a pass costs one hop per waiting row
// and happens only when a person has just acted, so there is no worker here to
// wake a lane nobody asked for. A row left behind by a sidecar that died has to
// wait for the next request or for the monitor's own standing round, which reads
// the ledger and finds the open turn regardless.
func (s *sessionDB) relayRequestWake(ctx context.Context, turnID int64) *workRequestRelay {
	db, err := s.handle(ctx)
	if err != nil {
		s.logger.Info("the monitor lane handover could not reach the work records", "err", err)
		return &workRequestRelay{State: workRelayPending}
	}
	var target workWakeIntent
	var found bool
	var others []workWakeIntent
	if err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var readErr error
		if target, found, readErr = readWakeIntent(ctx, tx, turnID); readErr != nil {
			return readErr
		}
		others, readErr = openWakeIntents(ctx, tx, turnID, workClock(), workWakeSweepMax+1)
		return readErr
	}); err != nil {
		s.logger.Info("the waiting monitor lane handovers could not be read", "err", err)
		return &workRequestRelay{State: workRelayPending}
	}
	if !found {
		// The row only leaves when the turn it names has been taken or the task
		// it names is gone (deliverWakeIntent), so its absence here is the
		// handover having already served its purpose.
		return &workRequestRelay{State: workRelayAccepted}
	}
	// This request's own handover goes first: it is the one the person is
	// waiting on an answer about, and the pass is bounded.
	state, attempts := s.deliverWakeIntent(ctx, db, target)
	if len(others) > workWakeSweepMax {
		// Said out loud rather than trimmed quietly: a bounded pass that looks
		// like a complete one is how a backlog stops being visible.
		s.logger.Info("more monitor lane handovers are waiting than one pass carries",
			"carried", workWakeSweepMax, "waiting", len(others))
		others = others[:workWakeSweepMax]
	}
	for _, intent := range others {
		s.deliverWakeIntent(ctx, db, intent)
	}
	return &workRequestRelay{State: state, Attempts: attempts}
}

// deliverWakeIntent carries one handover: narrow re-read, claim, hop,
// acknowledge. Each step is its own transaction, so the hop is never inside one.
func (s *sessionDB) deliverWakeIntent(ctx context.Context, db *sql.DB, intent workWakeIntent) (string, int64) {
	attempt := intent.Attempts
	pruned := false
	if err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		// The target is re-read here, by this task and this turn alone, because
		// everything that would make the handover wrong has happened since the
		// row was written: the turn may have been started by the monitor's own
		// round, the task may have been deleted. This is the gate that makes the
		// work happen once.
		turn, loadErr := loadWorkTurn(ctx, tx, intent.TaskID, intent.TurnID)
		if loadErr != nil {
			var known workError
			if errors.As(loadErr, &known) && known.Code == "not_found" {
				pruned = true
				return dropWakeIntent(ctx, tx, intent.TurnID)
			}
			return loadErr
		}
		if turn.StartedAt != nil || turn.EndedAt != nil {
			pruned = true
			return dropWakeIntent(ctx, tx, intent.TurnID)
		}
		return nil
	}); err != nil {
		s.logger.Info("a monitor lane handover could not be re-read", "turn", intent.TurnID, "err", err)
		return workRelayPending, attempt
	}
	if pruned {
		// The monitor has the turn, or there is no turn left to hand over.
		return workRelayAccepted, attempt
	}
	// A row a lane already took is re-delivered only on the lane's own evidence
	// that the run holding it is gone. Age alone must never re-deliver it: every
	// later request would then queue another turn about the same answer.
	if intent.State == workWakeDelivered && !s.wakeIntentLaneIsGone(ctx, intent) {
		return workRelayAccepted, attempt
	}
	var deliver bool
	if err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		var claimErr error
		if intent.State == workWakeDelivered {
			deliver, attempt, claimErr = claimWakeIntentForRedelivery(
				ctx, tx, intent.TurnID, intent.Ticket, workClock())
			return claimErr
		}
		deliver, attempt, claimErr = claimWakeIntent(ctx, tx, intent.TurnID, workClock())
		return claimErr
	}); err != nil {
		s.logger.Info("a monitor lane handover could not be claimed", "turn", intent.TurnID, "err", err)
		return workRelayPending, attempt
	}
	if !deliver {
		// Another pass holds a live claim on this row, or it moved under this
		// one. Neither is a failure, and neither is a second hop.
		return workRelayAccepted, attempt
	}
	ticket, err := s.monitorLane.ask(ctx, wakeDeliveryPrompt(intent, attempt))
	if err != nil {
		s.logger.Info("the monitor lane did not take a handover", "turn", intent.TurnID, "err", err)
		if releaseErr := s.workTx(ctx, db, func(tx *sql.Tx) error {
			return releaseWakeIntent(ctx, tx, intent.TurnID)
		}); releaseErr != nil {
			s.logger.Info("a refused handover could not be put back", "turn", intent.TurnID, "err", releaseErr)
		}
		return workRelayPending, attempt
	}
	if ackErr := s.workTx(ctx, db, func(tx *sql.Tx) error {
		return markWakeDelivered(ctx, tx, intent.TurnID, ticket, workClock())
	}); ackErr != nil {
		// The lane has the prompt and this did not get written down. Reporting
		// it as accepted is the truth about the hop; the lease is what makes the
		// row deliverable again, and the monitor is told which attempt it is.
		s.logger.Info("a delivered handover could not be acknowledged", "turn", intent.TurnID, "err", ackErr)
	}
	return workRelayAccepted, attempt
}

// wakeDeliveryPrompt is the whole of what the monitor lane is told. It is the
// platform's own sentence rather than a wrapper around the person's — their
// words are the turn's, and this says where to read them.
//
// It names which attempt this is because the monitor may have seen it before: a
// delivery whose acknowledgement was lost, or whose lane restarted, arrives
// again under the same `delivery` id. The three refusals it ends with are the
// ones this path can actually produce, and each is a thing the monitor must not
// do on the strength of being woken.
//
// Keep this in step with the consuming contract in the plugin marketplace
// (skillset/task-monitoring-lifecycle/references/review-request-wake.md), which
// quotes it: a prompt the skill cannot recognise is a prompt that reads as a
// person speaking.
func wakeDeliveryPrompt(intent workWakeIntent, attempt int64) string {
	return "session-platform: a person has answered on a task that was waiting for their judgement.\n\n" +
		"task_id=" + intent.TaskID +
		" turn_id=" + strconv.FormatInt(intent.TurnID, 10) +
		" delivery=" + wakeDeliveryID(intent.TurnID) +
		" attempt=" + strconv.FormatInt(attempt, 10) + "\n\n" +
		"Read that one task and that one turn out of the work records before you decide anything. " +
		"They are the only new fact, and the person's words are in the turn — they are not quoted here, " +
		"because the record is the one copy.\n" +
		"Take it once. If that turn has already been started or ended, it is already yours or already " +
		"settled: record nothing and start nothing. An attempt above 1 means this was delivered before, " +
		"so check that before acting.\n" +
		"This is not an approval, not a new task, and not permission to widen anything. Your own standing " +
		"round is unchanged and this does not replace it — do not add a schedule for it."
}

// wakeDeliveryID names one handover across its re-deliveries. It is the turn,
// because the turn is what is being handed over: an id that changed per attempt
// would make two deliveries of one request look like two requests.
func wakeDeliveryID(turnID int64) string { return "wake-" + strconv.FormatInt(turnID, 10) }

// openWakeIntents is every other handover this pass may act on, oldest turn
// first: the ones waiting, the ones whose claim has expired, and the ones a lane
// took — the last because whether that lane is still running them is a question
// only the lane can answer, and it cannot be asked from inside a transaction.
//
// The turn the caller is answering about is excluded, because the caller carries
// that one itself and first.
func openWakeIntents(ctx context.Context, tx *sql.Tx, except int64, now time.Time, limit int) ([]workWakeIntent, error) {
	expired := workNow(now.Add(-workWakeClaimLease))
	rows, err := tx.QueryContext(ctx,
		`SELECT turn_id,task_id,source_id,state,attempts,ticket,recorded_at,claimed_at
		   FROM sp_work_request_wake
		  WHERE turn_id<>?
		    AND (state=? OR (state=? AND claimed_at<=?) OR state=?)
		  ORDER BY turn_id LIMIT ?`,
		except, workWakePending, workWakeDelivering, expired, workWakeDelivered, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workWakeIntent{}
	for rows.Next() {
		var intent workWakeIntent
		if err := rows.Scan(&intent.TurnID, &intent.TaskID, &intent.SourceID, &intent.State,
			&intent.Attempts, &intent.Ticket, &intent.RecordedAt, &intent.ClaimedAt); err != nil {
			return nil, err
		}
		out = append(out, intent)
	}
	return out, rows.Err()
}

// claimWakeIntent takes one waiting row for a hop and returns the attempt
// number that hop is. One conditional UPDATE, so two passes holding the same
// row cannot both get through: reading the state and then writing it would let
// them.
//
// The claim is honoured for workWakeClaimLease and then it is not, because the
// alternative is a row that one dead process owns for ever. A row that was
// delivered is not reachable from here at any age — that needs the lane's own
// evidence (claimWakeIntentForRedelivery).
func claimWakeIntent(ctx context.Context, tx *sql.Tx, turnID int64, now time.Time) (bool, int64, error) {
	stamp := workNow(now)
	expired := workNow(now.Add(-workWakeClaimLease))
	result, err := tx.ExecContext(ctx,
		`UPDATE sp_work_request_wake
		    SET state=?, attempts=attempts+1, claimed_at=?
		  WHERE turn_id=? AND (state=? OR (state=? AND claimed_at<=?))`,
		workWakeDelivering, stamp, turnID,
		workWakePending, workWakeDelivering, expired)
	if err != nil {
		return false, 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, 0, err
	}
	attempts, err := wakeIntentAttempts(ctx, tx, turnID)
	return true, attempts, err
}

// claimWakeIntentForRedelivery is the same claim for a row a lane already took,
// used once the lane has said that run of it is gone. It is separate so that a
// delivered row can never be re-claimed by the ordinary path on the strength of
// its age alone.
func claimWakeIntentForRedelivery(ctx context.Context, tx *sql.Tx, turnID int64, ticket string, now time.Time) (bool, int64, error) {
	result, err := tx.ExecContext(ctx,
		`UPDATE sp_work_request_wake SET state=?, attempts=attempts+1, claimed_at=?
		  WHERE turn_id=? AND state=? AND ticket=?`,
		workWakeDelivering, workNow(now), turnID, workWakeDelivered, ticket)
	if err != nil {
		return false, 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, 0, err
	}
	attempts, err := wakeIntentAttempts(ctx, tx, turnID)
	return true, attempts, err
}

func wakeIntentAttempts(ctx context.Context, tx *sql.Tx, turnID int64) (int64, error) {
	var attempts int64
	err := tx.QueryRowContext(ctx,
		"SELECT attempts FROM sp_work_request_wake WHERE turn_id=?", turnID).Scan(&attempts)
	return attempts, err
}

func markWakeDelivered(ctx context.Context, tx *sql.Tx, turnID int64, ticket string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE sp_work_request_wake SET state=?, ticket=?, claimed_at=? WHERE turn_id=?",
		workWakeDelivered, ticket, workNow(now), turnID)
	return err
}

// releaseWakeIntent puts a row the lane would not take back where it was. The
// attempt count is not wound back: it counts hops, and a refused hop happened.
func releaseWakeIntent(ctx context.Context, tx *sql.Tx, turnID int64) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE sp_work_request_wake SET state=?, claimed_at='' WHERE turn_id=? AND state=?",
		workWakePending, turnID, workWakeDelivering)
	return err
}

func dropWakeIntent(ctx context.Context, tx *sql.Tx, turnID int64) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM sp_work_request_wake WHERE turn_id=?", turnID)
	return err
}

// readWakeIntent reads one row by the turn it names.
func readWakeIntent(ctx context.Context, tx *sql.Tx, turnID int64) (workWakeIntent, bool, error) {
	var intent workWakeIntent
	err := tx.QueryRowContext(ctx,
		`SELECT turn_id,task_id,source_id,state,attempts,ticket,recorded_at,claimed_at
		   FROM sp_work_request_wake WHERE turn_id=?`, turnID).
		Scan(&intent.TurnID, &intent.TaskID, &intent.SourceID, &intent.State,
			&intent.Attempts, &intent.Ticket, &intent.RecordedAt, &intent.ClaimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return workWakeIntent{}, false, nil
	}
	if err != nil {
		return workWakeIntent{}, false, err
	}
	return intent, true, nil
}

// wakeIntentLaneIsGone asks the lane whether the run that took this prompt is
// still there. `unknown_run` is the lane's own answer for a receipt from a
// previous run of itself (turn_ticket.go), and it is the only answer that means
// the queued prompt went with the conversation.
//
// Anything else — queued, running, done, or a refusal this code cannot
// interpret — is treated as "still there". Being wrong in that direction costs
// the monitor's standing round finding the turn a little later; being wrong the
// other way queues a second turn about work in hand.
func (s *sessionDB) wakeIntentLaneIsGone(ctx context.Context, intent workWakeIntent) bool {
	if intent.State != workWakeDelivered || intent.Ticket == "" {
		return false
	}
	answer, err := s.monitorLane.answer(ctx, intent.Ticket)
	if err != nil {
		return false
	}
	state, _ := answer["state"].(string)
	return state == turnTicketUnknownRun
}
