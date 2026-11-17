// Mid-turn steering for the Codex workloads (docs/prd/codex-workload.md
// AC-J9). A message the platform writes while a turn runs goes into that
// turn, not into the queue behind it: the control plane posts it here, the
// agent hands it to the runner on the runner's stdin, and the runner calls
// the app server's turn/steer. The runner answers on its stdout with a line
// the stream projector routes back to the waiting request, so the caller
// learns whether codex applied the message to the turn that was named
// (applied) or refused it (rejected) — never just "accepted" with nothing
// behind it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// steerPath is the control plane's route on the pod IP.
	steerPath = "/steer"
	// codexSteerType is the runner's reply line type, which the projector
	// takes out of the session output and hands to the desk.
	codexSteerType = "session_platform.steer"

	steerStatusApplied  = "applied"
	steerStatusRejected = "rejected"

	maxSteerBodyBytes = 64 << 10
	// A steered message is a chat turn's worth of text, not a prompt: the
	// 1 MiB prompt bound belongs to write.
	maxSteerTextRunes = 8000
	// steerReplyTimeout bounds the wait for the runner's answer. The runner
	// answers as soon as the app server does, so this only covers a runner
	// that died between the write and the reply.
	steerReplyTimeout = 30 * time.Second
)

var (
	errSteerNoTurn      = errors.New("no turn is running; a steered message belongs to the running turn")
	errSteerUnsupported = errors.New("this workload does not take mid-turn messages")
	errSteerInvalid     = errors.New("invalid steered message")
	errSteerRunnerGone  = errors.New("the runner stopped before it answered")
)

// steerable is an agentCLI whose turns take mid-turn messages (the Codex
// workloads, whose runner speaks turn/steer).
type steerable interface {
	takesSteering() bool
}

var _ steerable = codexCLI{}

func (codexCLI) takesSteering() bool { return true }

// steerOutcome is what one steered message did.
type steerOutcome struct {
	Status string `json:"status"`
	TurnID string `json:"turn_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (o steerOutcome) applied() bool { return o.Status == steerStatusApplied }

// steerDesk holds the running turn's steering channel: the runner's stdin
// while a turn is up, and the replies the projector routes back. It is
// turn-scoped on purpose — a message cannot be steered into a turn that is
// not running, and nothing outlives the turn it was meant for.
type steerDesk struct {
	mu      sync.Mutex
	in      io.Writer
	waiting map[string]chan steerOutcome
	seq     uint64
}

// open hands the desk the running turn's runner stdin.
func (d *steerDesk) open(in io.Writer) {
	d.mu.Lock()
	d.in = in
	d.waiting = map[string]chan steerOutcome{}
	d.mu.Unlock()
}

// close ends the turn's steering. Whoever still waits is answered rejected
// rather than left on a runner that is gone.
func (d *steerDesk) close() {
	d.mu.Lock()
	waiting := d.waiting
	d.in, d.waiting = nil, nil
	d.mu.Unlock()
	for _, reply := range waiting {
		select {
		case reply <- steerOutcome{Status: steerStatusRejected, Reason: errSteerRunnerGone.Error()}:
		default:
		}
	}
}

// resolve routes the runner's reply to the request that is waiting for it.
// A reply for an id nobody waits on is dropped: the waiter gave up, or the
// line is a duplicate.
func (d *steerDesk) resolve(id string, outcome steerOutcome) {
	d.mu.Lock()
	reply := d.waiting[id]
	delete(d.waiting, id)
	d.mu.Unlock()
	if reply == nil {
		return
	}
	select {
	case reply <- outcome:
	default:
	}
}

// steer hands text to the running turn and waits for the runner's verdict.
func (d *steerDesk) steer(ctx context.Context, text string) (steerOutcome, error) {
	if text == "" || utf8.RuneCountInString(text) > maxSteerTextRunes {
		return steerOutcome{}, fmt.Errorf("%w: text must be 1..%d characters", errSteerInvalid, maxSteerTextRunes)
	}
	reply := make(chan steerOutcome, 1)
	d.mu.Lock()
	if d.in == nil {
		d.mu.Unlock()
		return steerOutcome{}, errSteerNoTurn
	}
	d.seq++
	id := fmt.Sprintf("steer-%d", d.seq)
	d.waiting[id] = reply
	in := d.in
	d.mu.Unlock()

	line, err := json.Marshal(map[string]string{"type": "steer", "id": id, "text": text})
	if err != nil {
		d.forget(id)
		return steerOutcome{}, err
	}
	if _, err := in.Write(append(line, '\n')); err != nil {
		d.forget(id)
		return steerOutcome{}, fmt.Errorf("%w: %v", errSteerRunnerGone, err)
	}

	timeout := time.NewTimer(steerReplyTimeout)
	defer timeout.Stop()
	select {
	case outcome := <-reply:
		return outcome, nil
	case <-ctx.Done():
		d.forget(id)
		return steerOutcome{}, ctx.Err()
	case <-timeout.C:
		d.forget(id)
		return steerOutcome{}, errSteerRunnerGone
	}
}

func (d *steerDesk) forget(id string) {
	d.mu.Lock()
	delete(d.waiting, id)
	d.mu.Unlock()
}

// steerReply is the runner's stdout line for one steered message.
type steerReply struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	TurnID string `json:"turn_id"`
	Reason string `json:"reason"`
}
