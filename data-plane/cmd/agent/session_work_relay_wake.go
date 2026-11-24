// What makes the monitor lane actually run a message (AC-N11).
//
// The relay is a record, and a record on its own moves nothing: relay_send
// commits a row and returns, so without this file a message addressed to the
// monitor sits `pending` until something starts a monitor turn — and the only
// things that could are a person and the control plane's lane write. That would
// make the pair a pair in shape only: the parent would hand work over and the
// work would wait for a human to poke the lane that was built to not need one.
//
// So the sidecar that owns the database wakes the lane. It is the sidecar
// because it is the only process that opens the slot's file (session_db_mcp.go),
// and it reaches the lane the way the scheduler already reaches the person's
// chat: a loopback POST of one prompt onto that lane's own queue (AC-P2,
// scheduler.go). No new listener, no control-plane hop, nothing reachable from
// outside the pod that was not reachable before.
//
// Two decisions are what make this safe rather than a second source of truth:
//
//   - **The wake is level-triggered, not a delivery.** Every sweep asks the
//     database the same question — "is there a message for the monitor that
//     nobody has settled, and is the monitor free?" — and the prompt it sends
//     carries no message content at all. So a prompt that is dropped, a sidecar
//     that restarts mid-wake and a monitor whose turn died before it claimed all
//     end in the same place: the row is still unsettled, the next sweep asks
//     again. Nothing is delivered exactly once because nothing is delivered.
//   - **A held message is still a reason to wake an idle monitor.** `claimed`
//     counts as unsettled. A monitor that goes idle with a claim outstanding is
//     a monitor whose earlier incarnation died holding it, and relay_claim tells
//     the new one so (`recovered`, `attempts`, `prior_incarnation`). Counting
//     only `pending` would leave exactly that message to nobody.
//
// What it does not do: it never wakes the parent. A message for the parent is
// for the person's chat, and a turn nobody asked for would put text in front of
// a person who did not speak. The parent reads its inbox when it next answers —
// which is a turn a person started, as every parent turn is.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// managerMonitorWakeURLEnv is the monitor lane's own write address, on
	// loopback, given to the sidecar. Unset is a sidecar that wakes nothing,
	// which is every pod that does not run the pair. Keep in sync with
	// control-plane/internal/adapter/k8s (ManagerMonitorWakeURLEnvVar).
	managerMonitorWakeURLEnv = "MANAGER_MONITOR_WAKE_URL"

	// relayWakeSweep is how often the question gets asked again. It is the
	// recovery path rather than the delivery path — a relay_send notifies at
	// once — so it is tuned for "a lost wake costs this long", not for latency.
	relayWakeSweep = 15 * time.Second
	// relayWakeCall bounds one loopback call to the lane.
	relayWakeCall = 10 * time.Second
	// relayWakeBackoff is the sweep after the monitor has been woken
	// relayWakeMaxTries times without the unsettled set changing at all. The
	// lane is then either stuck or deliberately leaving the message; either way
	// asking every 15s only burns turns, and the message stays visibly open in
	// relay_list and relay_status rather than being dropped.
	relayWakeBackoff  = 10 * time.Minute
	relayWakeMaxTries = 5

	// relayWakePrompt is the whole prompt. It names no message on purpose: the
	// database is where the work is, so a prompt that carried a message could
	// disagree with it.
	relayWakePrompt = "The parent lane of this manager has handed you work that nobody has settled. " +
		"List it with " + relayListTool + " (box=inbox, open=true), take each message with " + relayClaimTool +
		", do what it says, and settle it with " + relayAckTool + " — the result you put there is what the parent reads back. " +
		"A claim that comes back recovered=true was held by an earlier run of this lane: establish what that run already did outside the database before repeating it. " +
		"This prompt carries no instruction of its own and is not a delivery — the messages are the instruction, and anything you leave unsettled is handed to you again."
)

// relayWakeSet is the unsettled inbox of the monitor lane, reduced to what a
// waker needs: whether there is anything, and whether it is the same anything as
// last time. The fingerprint moves when a message arrives, when one is claimed
// (attempts) and when one is settled (count), which is every change that means
// progress.
type relayWakeSet struct {
	count       int64
	fingerprint string
}

// monitorRelayWakeSet asks the one question the waker has. It reads nothing it
// does not need and — unlike every tool path — it does not migrate: a database
// no lane has ever called has no relay table, and creating one so that it can be
// counted as empty would make a passive sweep a writer.
func (s *sessionDB) monitorRelayWakeSet(ctx context.Context) (relayWakeSet, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return relayWakeSet{}, err
	}
	var present int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sp_work_relay'").Scan(&present); err != nil {
		return relayWakeSet{}, err
	}
	if present == 0 {
		return relayWakeSet{}, nil
	}
	var count, maxSeq, attempts int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(MAX(seq),0), COALESCE(SUM(attempts),0) FROM sp_work_relay
		 WHERE recipient=? AND state IN (?,?)`,
		relayLaneMonitor, relayStatePending, relayStateClaimed).Scan(&count, &maxSeq, &attempts); err != nil {
		return relayWakeSet{}, err
	}
	if count == 0 {
		return relayWakeSet{}, nil
	}
	return relayWakeSet{count: count, fingerprint: fmt.Sprintf("%d/%d/%d", count, maxSeq, attempts)}, nil
}

// relayWakeTarget is the lane as the waker uses it: is it free, and put this
// prompt on its queue. An interface so the sweep can be tested without a lane.
type relayWakeTarget interface {
	idle(context.Context) (bool, error)
	write(ctx context.Context, prompt string) error
}

// relayWaker turns an unsettled monitor inbox into a monitor turn.
type relayWaker struct {
	inbox  func(context.Context) (relayWakeSet, error)
	lane   relayWakeTarget
	logger *slog.Logger
	sweep  time.Duration

	// signal is the "a message just landed" nudge from relay_send. It is a
	// buffered chan of one, so a hundred sends while a turn runs collapse into
	// the one sweep that was going to ask the database anyway.
	signal chan struct{}

	// tries counts wakes sent since the unsettled set last changed, and seen is
	// the set they were sent for. Both are the sweep's own state: they are reset
	// by any progress and lost on a sidecar restart, which is correct — a
	// restarted sidecar has no evidence the lane is stuck.
	tries int
	seen  string
}

func newRelayWaker(inbox func(context.Context) (relayWakeSet, error), lane relayWakeTarget, logger *slog.Logger) *relayWaker {
	if logger == nil {
		logger = slog.Default()
	}
	return &relayWaker{
		inbox:  inbox,
		lane:   lane,
		logger: logger,
		sweep:  relayWakeSweep,
		signal: make(chan struct{}, 1),
	}
}

// notify is the nudge a committed relay_send gives. It never blocks and never
// fails: a nudge that could not be left is a sweep that happens a tick later,
// not a message that is lost.
func (w *relayWaker) notify() {
	if w == nil {
		return
	}
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

// run sweeps until the context ends.
func (w *relayWaker) run(ctx context.Context) {
	timer := time.NewTimer(w.sweep)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.signal:
		case <-timer.C:
		}
		next := w.sweepOnce(ctx)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(next)
	}
}

// sweepOnce asks the database, and wakes the lane if there is a reason to. It
// returns how long to wait before asking again.
func (w *relayWaker) sweepOnce(ctx context.Context) time.Duration {
	set, err := w.inbox(ctx)
	if err != nil {
		// A database that cannot be read yet is the sidecar's ordinary cold
		// start (session_db_mcp.go opens on first use), so this is not an error
		// the pod should act on — the next sweep asks again.
		w.logger.Debug("monitor relay wake: cannot read the inbox", "err", err)
		return w.sweep
	}
	if set.count == 0 {
		w.tries, w.seen = 0, ""
		return w.sweep
	}
	if set.fingerprint != w.seen {
		w.tries, w.seen = 0, set.fingerprint
	}
	if w.tries >= relayWakeMaxTries {
		// Said once per set rather than every sweep: the point is that a person
		// looking at this pod can see the lane is not settling its inbox.
		if w.tries == relayWakeMaxTries {
			w.logger.Warn("monitor relay wake: backing off; the monitor lane has not settled its inbox",
				"messages", set.count, "wakes", w.tries, "sweep", relayWakeBackoff)
			w.tries++
		}
		return relayWakeBackoff
	}
	idle, err := w.lane.idle(ctx)
	if err != nil {
		w.logger.Debug("monitor relay wake: cannot read the lane's queue", "err", err)
		return w.sweep
	}
	if !idle {
		// The lane is working. Waking it would queue a prompt behind the turn it
		// is already running, and that turn's own relay_list will see whatever
		// landed. A busy lane is not a failed wake, so it does not count a try.
		return w.sweep
	}
	callCtx, cancel := context.WithTimeout(ctx, relayWakeCall)
	defer cancel()
	if err := w.lane.write(callCtx, relayWakePrompt); err != nil {
		w.logger.Warn("monitor relay wake: could not queue the lane's turn", "err", err, "messages", set.count)
		return w.sweep
	}
	w.tries++
	w.logger.Info("monitor relay wake: queued a turn for the monitor lane",
		"messages", set.count, "wake", w.tries)
	return w.sweep
}

// httpRelayWakeTarget is the monitor lane over loopback.
type httpRelayWakeTarget struct {
	writeURL string
	queueURL string
	client   *http.Client
}

// monitorWakeURLFromEnv reads the lane's write address. Only a loopback address
// on this pod is one: the waker exists to reach the conversation next to it, and
// an address that pointed anywhere else would be this sidecar starting turns in
// another session.
func monitorWakeURLFromEnv(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(managerMonitorWakeURLEnv))
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "http://127.0.0.1:") || !strings.HasSuffix(raw, managerLanePathPrefix+"/write") {
		return "", fmt.Errorf("%s must be an http URL on 127.0.0.1 ending in %s",
			managerMonitorWakeURLEnv, managerLanePathPrefix+"/write")
	}
	return raw, nil
}

func newHTTPRelayWakeTarget(writeURL string) *httpRelayWakeTarget {
	return &httpRelayWakeTarget{
		writeURL: writeURL,
		queueURL: strings.TrimSuffix(writeURL, "/write") + agentQueuePath,
		client:   &http.Client{},
	}
}

// idle reports whether a prompt would start a turn right away. It is the lane's
// own GET /monitor/queue — the same answer the scheduler reads about the
// person's chat (scheduler_agent.go).
func (t *httpRelayWakeTarget) idle(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.queueURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("monitor lane queue: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var state agentQueueState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return false, fmt.Errorf("monitor lane queue: %w", err)
	}
	return state.Idle, nil
}

func (t *httpRelayWakeTarget) write(ctx context.Context, prompt string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.writeURL, strings.NewReader(prompt))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("monitor lane write: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return nil
}
