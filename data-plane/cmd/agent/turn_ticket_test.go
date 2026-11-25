package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A receipt is asked about after the fact, by a caller that has been doing
// something else. The tests here are all one question: can it ever answer with
// a turn that is not the one it names.

// TestTicketIsIssuedOnlyForAPromptThatWasQueued is the dangling-receipt case. A
// refused write must hand back nothing, because a number with no entry behind it
// is a number that would later resolve against whatever did get that slot.
func TestTicketIsIssuedOnlyForAPromptThatWasQueued(t *testing.T) {
	c := newHeldClaudeLane(t)

	ticket, err := c.enqueue("the first request", false)
	if err != nil || ticket == "" {
		t.Fatalf("enqueue = %q, %v; want a receipt", ticket, err)
	}
	// Admission closed is the refusal every other one is shaped like.
	c.mu.Lock()
	c.accepting = false
	c.mu.Unlock()
	refused, err := c.enqueue("the second request", false)
	if err == nil {
		t.Fatal("a closed lane took a prompt")
	}
	if refused != "" {
		t.Fatalf("a refused write returned the receipt %q; there is no turn to ask about", refused)
	}
	// And the number the refusal would have taken is not in the book: asking
	// about it says absent rather than resolving to the queued prompt.
	n, _, parseErr := parseTurnTicket(ticket)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	answer, err := c.answerTurnTicket(formatTurnTicket(n + 1))
	if err != nil {
		t.Fatal(err)
	}
	if answer.State != turnTicketAbsent {
		t.Fatalf("the receipt after the last issued one = %q, want %q", answer.State, turnTicketAbsent)
	}
}

// TestTwoRequestsAtOnceGetTwoReceipts is the concurrency case: identical words
// must not collapse into one turn.
func TestTwoRequestsAtOnceGetTwoReceipts(t *testing.T) {
	c := newHeldClaudeLane(t)

	const same = "do the same thing"
	first, err := c.enqueue(same, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.enqueue(same, false)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two writes of the same words got one receipt %q", first)
	}
}

// TestAReceiptFromAnEarlierRunIsNotThisRunsTurn is the restart case, and the
// reason the epoch is checked before the book is consulted: the numbers start
// again with the run, so a stale receipt's number is very likely in range.
func TestAReceiptFromAnEarlierRunIsNotThisRunsTurn(t *testing.T) {
	c := newHeldClaudeLane(t)
	if _, err := c.enqueue("the request this run took", false); err != nil {
		t.Fatal(err)
	}

	// Same number, a different run. This is exactly the shape of a receipt the
	// parent kept across a restart of the monitor lane.
	stale := "someearlierrun-1"
	answer, err := c.answerTurnTicket(stale)
	if err != nil {
		t.Fatal(err)
	}
	if answer.State != turnTicketUnknownRun {
		t.Fatalf("a receipt from another run = %q, want %q", answer.State, turnTicketUnknownRun)
	}
	if answer.Response != "" || answer.Seq != 0 {
		t.Fatalf("a receipt from another run resolved to a turn: %+v", answer)
	}
	// This run's own receipt for the same number still answers normally, which
	// is what makes the refusal above about the run and not about the number.
	mine, err := c.answerTurnTicket(formatTurnTicket(1))
	if err != nil {
		t.Fatal(err)
	}
	if mine.State == turnTicketUnknownRun {
		t.Fatalf("this run's own receipt was refused as another run's: %+v", mine)
	}
}

func TestAMalformedReceiptIsRefusedRatherThanGuessed(t *testing.T) {
	c := newHeldClaudeLane(t)
	for _, raw := range []string{"", "1", "-1", agentEpoch + "-", "-" + agentEpoch, agentEpoch + "-0", agentEpoch + "-x"} {
		if _, err := c.answerTurnTicket(raw); err == nil {
			t.Errorf("the receipt %q was accepted", raw)
		}
	}
}

// TestADroppedReceiptSaysExpiredRatherThanAnsweringWrong is the bound on the
// book. Dropping the oldest entries is fine; answering about them with a later
// turn's words is not.
func TestADroppedReceiptSaysExpiredRatherThanAnsweringWrong(t *testing.T) {
	book := newTurnTicketBook()
	for i := 0; i < turnTicketsKept+5; i++ {
		book.issue("now")
	}
	if _, state := book.lookup(1); state != turnTicketExpired {
		t.Fatalf("the oldest dropped receipt = %q, want %q", state, turnTicketExpired)
	}
	if _, state := book.lookup(uint64(turnTicketsKept + 5)); state != turnTicketQueued {
		t.Fatalf("the newest receipt = %q, want %q", state, turnTicketQueued)
	}
	if _, state := book.lookup(uint64(turnTicketsKept + 6)); state != turnTicketAbsent {
		t.Fatalf("a receipt never issued = %q, want %q", state, turnTicketAbsent)
	}
}

// TestAReceiptResolvesToItsOwnTurnsAnswer is the whole point, end to end on one
// lane: three turns run, and each receipt returns the words of its own.
func TestAReceiptResolvesToItsOwnTurnsAnswer(t *testing.T) {
	replies := []string{"first answer", "second answer", "third answer"}
	runner := newFakeClaudeRunner()
	runner.output = func(index int, opts runnerOptions) {
		_, _ = fmt.Fprintf(opts.Stdout, "%s\n", replies[index])
	}
	c, _ := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	var tickets []string
	for i := 0; i < 3; i++ {
		ticket, err := c.enqueue("request", false)
		if err != nil {
			t.Fatal(err)
		}
		tickets = append(tickets, ticket)
	}
	waitForIdleLane(t, c)

	for i, ticket := range tickets {
		answer, err := c.answerTurnTicket(ticket)
		if err != nil {
			t.Fatal(err)
		}
		if answer.State != turnTicketDone {
			t.Fatalf("receipt %d = %q, want %q", i, answer.State, turnTicketDone)
		}
		if answer.Seq != i+1 {
			t.Errorf("receipt %d names turn %d, want %d", i, answer.Seq, i+1)
		}
		want := replies[i]
		if !strings.Contains(answer.Response, want) {
			t.Fatalf("receipt %d answered %q, want the turn that said %q", i, answer.Response, want)
		}
		// The sharp part: no other turn's words are in it.
		for j, other := range replies {
			if j != i && strings.Contains(answer.Response, other) {
				t.Fatalf("receipt %d also carries turn %d's answer %q:\n%s", i, j, other, answer.Response)
			}
		}
		// Nor the request that caused it — the answer starts after the input.
		if strings.Contains(answer.Response, "\x1esession-platform input") {
			t.Errorf("receipt %d carries its own input marker:\n%s", i, answer.Response)
		}
	}
}

// TestTheTurnEndpointIsLoopbackOnly: a receipt names a conversation, so the
// only caller is the sidecar in this pod.
func TestTheTurnEndpointIsLoopbackOnly(t *testing.T) {
	c := newHeldClaudeLane(t)
	ticket, err := c.enqueue("request", false)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, agentTurnPath+"?ticket="+ticket, nil)
	req.RemoteAddr = "10.1.2.3:4567"
	serveAgentTurn(recorder, req, c)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("an off-pod caller got %d, want 403", recorder.Code)
	}
}

// waitForIdleLane waits for the lane's worker to drain.
func waitForIdleLane(t *testing.T, c *claudeWorkload) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if c.queueState().Idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the lane never drained its queue")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newHeldClaudeLane is a lane whose turns never finish, so a queued prompt
// stays queued for as long as a test needs it to.
func newHeldClaudeLane(t *testing.T) *claudeWorkload {
	t.Helper()
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	t.Cleanup(func() { close(runner.release) })
	c, _ := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)
	return c
}
