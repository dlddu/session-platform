// The receipt a queued prompt gets, and the answer that receipt resolves to
// (AC-N11).
//
// The parent lane of a manager hands the person's request straight to the
// monitor lane and must not then wait: its own turn is a person's turn, and a
// parent that blocked until the monitor answered would have stopped being a
// conversation. So the write returns at once with a receipt, the parent carries
// on, and later it asks what that receipt came to.
//
// Which makes the receipt the whole correctness problem. It is asked about
// after the fact, by a caller that has been doing something else, possibly
// about a turn that has already ended, possibly across a restart of the lane —
// and the one thing it must never do is answer with a *different* turn's words.
// Four ways that could happen, and what stops each:
//
//   - **A receipt that was never issued.** Receipts come from this counter and
//     nowhere else, so a number nobody was given is not in the table and
//     answers `absent`. A write that failed enqueues nothing and returns no
//     receipt at all, so there is no number to ask about.
//   - **Two requests at once.** The number is taken under the same lock that
//     appends to the queue, so two writes get two numbers and two entries. They
//     cannot collide even if the prompts are identical.
//   - **A turn that has already ended.** Each entry keeps the bounds of its own
//     output in the scrollback, written when that turn began and ended. The
//     scrollback is append-only within a run, so those bounds keep meaning the
//     same bytes however many turns follow.
//   - **A restart.** The receipt carries the run that issued it (agentEpoch,
//     scheduler_agent.go). A lane that restarted has a new epoch and an empty
//     table, and the counter starts again from 1 — so yesterday's receipt would
//     otherwise name today's first turn. Checking the epoch first turns that
//     into `unknown_run`, which is a different answer from `absent` because the
//     caller can act on it: the request was lost with the lane and asking again
//     is the caller's decision. Nothing here resends it.
//
// The table is bounded, because a lane runs turns for as long as the pod lives.
// When the oldest entries are dropped their receipts answer `expired` — again a
// named answer rather than a wrong one.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	// agentTurnPath answers one receipt. It is a lane path (manager_lane.go)
	// and loopback-only, because the only caller is the sidecar beside it.
	agentTurnPath = "/turn"

	// turnTicketsKept bounds the table. A manager's parent asks about a
	// receipt within a turn or two of handing it over, so this is far more
	// history than the design uses; it exists so that a long-lived pod cannot
	// grow this map without end.
	turnTicketsKept = 256

	// The states a receipt resolves to. The first three are this run's own
	// progress; the last two are the honest refusals.
	turnTicketQueued  = "queued"
	turnTicketRunning = "running"
	turnTicketDone    = "done"
	// turnTicketAbsent is a receipt this run never issued.
	turnTicketAbsent = "absent"
	// turnTicketUnknownRun is a receipt from a previous run of this lane.
	turnTicketUnknownRun = "unknown_run"
	// turnTicketExpired is a receipt this run issued and has since dropped.
	turnTicketExpired = "expired"
)

// turnTicket is one queued prompt as the lane tracks it.
type turnTicket struct {
	n     uint64
	state string
	// seq is the turn number this prompt became, once it started. It is the
	// number in the scrollback's own markers (AC-E3), so a reader of the
	// conversation and a holder of a receipt are talking about the same turn.
	seq int
	// status is the turn's end status: ok, error, timeout or canceled.
	status string
	// from and to bound this turn's answer in the scrollback. from is set when
	// the turn begins — just after its input marker, so the request is not in
	// its own answer — and to when it ends, before the end marker.
	from, to int
	queuedAt string
	startedAt,
	endedAt string
}

// turnTicketBook is a lane's receipts. It is in memory on purpose: a receipt is
// only meaningful for the run that issued it, because the answer it resolves to
// lives in that run's scrollback. Nothing here is a durable queue — a request
// lost with its lane stays lost and says so.
type turnTicketBook struct {
	mu     sync.Mutex
	next   uint64
	byNum  map[uint64]*turnTicket
	oldest uint64
}

func newTurnTicketBook() *turnTicketBook {
	return &turnTicketBook{byNum: map[uint64]*turnTicket{}}
}

// issue takes the next receipt. The caller holds the lane's queue lock, so the
// number and the queue entry are taken together.
func (b *turnTicketBook) issue(now string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	n := b.next
	b.byNum[n] = &turnTicket{n: n, state: turnTicketQueued, queuedAt: now}
	// Drop from the oldest end rather than at random, so what survives is the
	// recent history a caller could still be asking about.
	for uint64(len(b.byNum)) > turnTicketsKept {
		b.oldest++
		delete(b.byNum, b.oldest)
	}
	return n
}

func (b *turnTicketBook) begin(n uint64, seq, from int, now string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.byNum[n]; t != nil {
		t.state, t.seq, t.from, t.startedAt = turnTicketRunning, seq, from, now
	}
}

func (b *turnTicketBook) end(n uint64, to int, status, now string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.byNum[n]; t != nil {
		t.state, t.to, t.status, t.endedAt = turnTicketDone, to, status, now
	}
}

// lookup resolves a receipt within this run. The bool reports whether there is
// an entry; the state carries why there is not.
func (b *turnTicketBook) lookup(n uint64) (turnTicket, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.byNum[n]; t != nil {
		return *t, t.state
	}
	// A number this run has handed out but no longer keeps, versus one it never
	// handed out at all. Both refuse; they refuse differently because the
	// caller's next move differs.
	if n >= 1 && n <= b.next {
		return turnTicket{n: n}, turnTicketExpired
	}
	return turnTicket{n: n}, turnTicketAbsent
}

// formatTurnTicket is the receipt as its holder sees it: this run, and this
// prompt of it. The epoch is first so that an eye can tell two runs apart.
func formatTurnTicket(n uint64) string {
	return agentEpoch + "-" + strconv.FormatUint(n, 10)
}

// parseTurnTicket splits a receipt and reports whether it belongs to this run.
func parseTurnTicket(raw string) (n uint64, thisRun bool, err error) {
	epoch, number, ok := strings.Cut(strings.TrimSpace(raw), "-")
	if !ok || epoch == "" || number == "" {
		return 0, false, fmt.Errorf("a receipt is <run>-<number>, not %q", raw)
	}
	n, parseErr := strconv.ParseUint(number, 10, 64)
	if parseErr != nil || n == 0 {
		return 0, false, fmt.Errorf("a receipt is <run>-<number>, not %q", raw)
	}
	return n, epoch == agentEpoch, nil
}

// turnTicketAnswer is what a receipt resolves to.
type turnTicketAnswer struct {
	Ticket string `json:"ticket"`
	State  string `json:"state"`
	Epoch  string `json:"epoch"`
	// Seq is the turn in the conversation, 0 before it started.
	Seq int `json:"seq"`
	// Status is the turn's end status once it is done.
	Status string `json:"status,omitempty"`
	// Response is that turn's own output, and only that turn's: the bytes the
	// scrollback holds between this turn's output marker and its end marker.
	Response  string `json:"response,omitempty"`
	QueuedAt  string `json:"queued_at,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// answerTurnTicket resolves a receipt against this lane.
func (c *claudeWorkload) answerTurnTicket(raw string) (turnTicketAnswer, error) {
	n, thisRun, err := parseTurnTicket(raw)
	if err != nil {
		return turnTicketAnswer{}, err
	}
	answer := turnTicketAnswer{Ticket: raw, Epoch: agentEpoch}
	if !thisRun {
		// Checked before the table is consulted. The numbers restart with the
		// run, so a stale receipt's number is very likely in range — which is
		// exactly the mix-up this ordering prevents.
		answer.State = turnTicketUnknownRun
		return answer, nil
	}
	ticket, state := c.tickets.lookup(n)
	answer.State, answer.Seq, answer.Status = state, ticket.seq, ticket.status
	answer.QueuedAt, answer.StartedAt, answer.EndedAt = ticket.queuedAt, ticket.startedAt, ticket.endedAt
	switch state {
	case turnTicketRunning, turnTicketDone:
		to := ticket.to
		if state == turnTicketRunning {
			// Still going: everything written so far is this turn's, because
			// nothing else appends to the scrollback while a turn runs.
			to = c.out.size()
		}
		answer.Response = string(c.out.slice(ticket.from, to))
	}
	return answer, nil
}

func serveAgentTurn(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	if !fromLoopback(r) {
		http.Error(w, "a lane's receipts are only for the sidecar beside it", http.StatusForbidden)
		return
	}
	answer, err := c.answerTurnTicket(r.URL.Query().Get("ticket"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}
