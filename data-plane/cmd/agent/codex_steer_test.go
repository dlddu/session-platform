package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A steered message reaches the runner as one JSON line, and the runner's
// reply comes back through the projector as the request's outcome (AC-J9).
func TestSteerDesk_AppliedRoundTrip(t *testing.T) {
	var desk steerDesk
	out := &steerBuffer{}
	desk.open(out)
	defer desk.close()
	projector := newCodexStreamProjector(io.Discard).withSteerDesk(&desk)

	var outcome steerOutcome
	done := make(chan error, 1)
	go func() {
		var err error
		outcome, err = desk.steer(context.Background(), "focus on the newer paper")
		done <- err
	}()

	line := waitForLine(t, out)
	var sent struct{ Type, ID, Text string }
	if err := json.Unmarshal([]byte(line), &sent); err != nil {
		t.Fatalf("the runner must get one JSON line: %q: %v", line, err)
	}
	if sent.Type != "steer" || sent.Text != "focus on the newer paper" || sent.ID == "" {
		t.Fatalf("line = %+v", sent)
	}
	// The runner's reply goes through the stream projector, which must route
	// it to the desk and keep it out of the session output.
	reply := `{"type":"session_platform.steer","id":"` + sent.ID + `","status":"applied","turn_id":"t1","reason":null}` + "\n"
	if _, err := projector.Write([]byte(reply)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("steer = %v", err)
	}
	if !outcome.applied() || outcome.TurnID != "t1" {
		t.Fatalf("outcome = %+v, want applied on t1", outcome)
	}
}

// codex refusing the steer (its expectedTurnId precondition) is reported as
// a rejection with the reason, not as a success.
func TestSteerDesk_RejectedKeepsTheReason(t *testing.T) {
	var desk steerDesk
	out := &steerBuffer{}
	desk.open(out)
	defer desk.close()
	projector := newCodexStreamProjector(io.Discard).withSteerDesk(&desk)

	var outcome steerOutcome
	done := make(chan error, 1)
	go func() {
		var err error
		outcome, err = desk.steer(context.Background(), "too late")
		done <- err
	}()
	var sent struct{ ID string }
	_ = json.Unmarshal([]byte(waitForLine(t, out)), &sent)
	reply := `{"type":"session_platform.steer","id":"` + sent.ID + `","status":"rejected","reason":"expectedTurnId does not match the active turn"}` + "\n"
	if _, err := projector.Write([]byte(reply)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if outcome.applied() || !strings.Contains(outcome.Reason, "expectedTurnId") {
		t.Fatalf("outcome = %+v, want the refusal reason", outcome)
	}
}

// With no turn running there is nothing to steer into, and an oversized or
// empty message never reaches the runner.
func TestSteerDesk_RefusesWithoutATurnOrWithBadText(t *testing.T) {
	var desk steerDesk
	if _, err := desk.steer(context.Background(), "hello"); !errors.Is(err, errSteerNoTurn) {
		t.Fatalf("steer with no turn = %v, want errSteerNoTurn", err)
	}
	desk.open(&steerBuffer{})
	defer desk.close()
	for _, text := range []string{"", strings.Repeat("가", maxSteerTextRunes+1)} {
		if _, err := desk.steer(context.Background(), text); !errors.Is(err, errSteerInvalid) {
			t.Fatalf("steer(%d runes) = %v, want errSteerInvalid", len([]rune(text)), err)
		}
	}
}

// The turn ending releases whoever waits: a reply that will never come is a
// rejection, not a hang.
func TestSteerDesk_CloseReleasesWaiters(t *testing.T) {
	var desk steerDesk
	desk.open(&steerBuffer{})
	done := make(chan steerOutcome, 1)
	go func() {
		outcome, _ := desk.steer(context.Background(), "mid-flight")
		done <- outcome
	}()
	waitForLine(t, nil) // let the writer get its line out first
	desk.close()
	select {
	case outcome := <-done:
		if outcome.applied() {
			t.Fatalf("outcome = %+v, want a rejection once the turn is gone", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close must release a waiting steer")
	}
}

// Two messages in flight cannot be crossed: each reply goes to its own id.
func TestSteerDesk_RepliesAreRoutedPerMessage(t *testing.T) {
	var desk steerDesk
	out := &steerBuffer{}
	desk.open(out)
	defer desk.close()
	projector := newCodexStreamProjector(io.Discard).withSteerDesk(&desk)

	first := make(chan steerOutcome, 1)
	second := make(chan steerOutcome, 1)
	go func() { o, _ := desk.steer(context.Background(), "one"); first <- o }()
	id1 := idOf(t, waitForLine(t, out))
	go func() { o, _ := desk.steer(context.Background(), "two"); second <- o }()
	id2 := idOf(t, waitForLine(t, out))
	if id1 == id2 {
		t.Fatal("two messages must not share an id")
	}
	// Answer the second one first: routing is by id, not by arrival order.
	_, _ = projector.Write([]byte(`{"type":"session_platform.steer","id":"` + id2 + `","status":"applied","turn_id":"t9"}` + "\n"))
	_, _ = projector.Write([]byte(`{"type":"session_platform.steer","id":"` + id1 + `","status":"rejected","reason":"no"}` + "\n"))
	if o := <-second; o.TurnID != "t9" || !o.applied() {
		t.Fatalf("second = %+v", o)
	}
	if o := <-first; o.applied() {
		t.Fatalf("first = %+v, want the rejection", o)
	}
}

// The reply line is a protocol line: the projector must not write it into
// the session output a person reads.
func TestCodexProjector_SteerReplyStaysOutOfTheOutput(t *testing.T) {
	var desk steerDesk
	desk.open(&steerBuffer{})
	defer desk.close()
	var out strings.Builder
	projector := newCodexStreamProjector(&out).withSteerDesk(&desk)
	if _, err := projector.Write([]byte(`{"type":"session_platform.steer","id":"x","status":"applied"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("session output = %q, want nothing", out.String())
	}
}

func idOf(t *testing.T, line string) string {
	t.Helper()
	var sent struct{ ID string }
	if err := json.Unmarshal([]byte(line), &sent); err != nil {
		t.Fatalf("line %q: %v", line, err)
	}
	return sent.ID
}

// waitForLine returns the next whole line the desk wrote. A nil buffer just
// yields the scheduler.
func waitForLine(t *testing.T, out *steerBuffer) string {
	t.Helper()
	if out == nil {
		time.Sleep(50 * time.Millisecond)
		return ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line, ok := out.takeLine(); ok {
			return line
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the desk wrote no line")
	return ""
}

type steerBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *steerBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *steerBuffer) takeLine() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, c := range b.data {
		if c == '\n' {
			line := string(b.data[:i])
			b.data = b.data[i+1:]
			return line, true
		}
	}
	return "", false
}

var _ io.Writer = (*steerBuffer)(nil)
