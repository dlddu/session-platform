// Session scheduler (docs/prd/session-scheduler.md, AC-P1~P4): prompts a
// codex session's model asks to run later or repeatedly. The loopback
// k3s-mcp-proxy sidecar keeps the schedules and the clock and serves the
// `scheduler` MCP; a run is a prompt it puts on the workload agent's own
// queue through loopback POST /write (AC-P2), never through the control plane.
//
// A manager pod runs two conversations (AC-N11), and a run has to land in one
// of them. It lands in the one that registered the schedule, and that is not an
// argument: the lane's address carries its role (<prefix>/lane/<role>/<run>,
// manager_lane.go), the address is in the managed config the platform rewrites
// every boot, and the lane that has no monitor address cannot ask for a run
// there. So the monitor's own watching never appears in the person's chat, and
// a session with one conversation — every codex-task, every manager that never
// turned the pair on — keeps the single destination it always had (AC-P4).
//
// What follows from two destinations is that everything about a run in flight
// is per lane: the idle gate is that lane's queue, the one-run-at-a-time rule
// is per lane (the parent spending a minute on a person cannot hold the
// monitor's clock), and so is the retry after a refusal. The run itself is
// owned by the receipt the lane issued for it (turn_ticket.go) — which is what
// makes "this round was delivered once" a fact rather than a count, and what
// lets a round's loop_next be told from a stale one's (AC-P4).
//
// Schedules live only in this process. A sidecar restart or a new pod (freeze
// and restore, delete) drops them, which the tool descriptions say. A lane
// restarting does not: the pod's monitor is still its monitor, so the schedule
// keeps its id and its next run, and only the round that was in flight is lost
// — with the receipt saying so rather than a number meaning a different turn.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Keep in sync with control-plane/internal/adapter/k8s
	// (SchedulerAgentURLEnvVar, SchedulerMCPURLEnvVar, schedulerMCPPath).
	schedulerAgentURLEnv = "SESSION_SCHEDULER_AGENT_URL"
	schedulerMCPURLEnv   = "SESSION_SCHEDULER_MCP_URL"
	schedulerMCPPath     = "/scheduler/mcp"
	schedulerStatePath   = "/scheduler/state"
	// schedulerLanePathPrefix is where a manager's lane registers the
	// scheduler: <prefix>/<role>/<incarnation>, the same shape the slot's
	// database uses (session_db_mcp.go). The role is in the address because
	// the address is in the lane's managed config, so a lane cannot register a
	// schedule that runs in the other lane's conversation (AC-P4).
	schedulerLanePathPrefix = schedulerMCPPath + "/lane"

	schedulerMCPServerName = "session-platform-scheduler"

	schedulerMaxSchedules    = 20
	schedulerMaxPromptBytes  = 16 << 10
	schedulerMinInterval     = time.Minute
	schedulerMaxInterval     = 24 * time.Hour
	schedulerMaxOnceDelay    = 7 * 24 * time.Hour
	schedulerMaxLifetime     = 7 * 24 * time.Hour
	schedulerDefaultFallback = 10 * time.Minute

	schedulerTick       = 5 * time.Second
	schedulerRetryDelay = 30 * time.Second
	schedulerAgentCall  = 10 * time.Second
	schedulerPreviewLen = 200
)

type scheduleKind string

const (
	scheduleOnce      scheduleKind = "once"
	scheduleFixed     scheduleKind = "loop_fixed"
	scheduleSelfPaced scheduleKind = "loop_self_paced"
)

type schedule struct {
	id     string
	kind   scheduleKind
	prompt string
	// interval is a fixed loop's interval, or a self-paced loop's fallback.
	interval  time.Duration
	nextRun   time.Time
	expiresAt time.Time
	runs      int
	// owner is the lane that registered this schedule, as the address the
	// registration arrived on said (AC-P4). Its role is where the runs go and
	// who may pace or cancel it; its incarnation is the run of that lane that
	// registered it, which only the round's own loop_next is checked against —
	// a lane that restarted is still this pod's parent or monitor, so its
	// schedules keep their ids and their next runs. The zero owner is a
	// session with one conversation, which is the destination it always had.
	owner workCaller
}

// dest is the lane a schedule's runs go to: the role of the lane that
// registered it. Empty is the agent's own conversation.
func (s *schedule) dest() string { return s.owner.lane }

// schedulerRun is the one run the scheduler has in flight on one lane. It is
// owned by the receipt that lane issued when the prompt was queued
// (turn_ticket.go): the run is over when that receipt is done, or when the
// receipt no longer names a turn of this run of the lane (unknown_run after a
// restart, expired, absent). A lane too old to issue receipts is settled the
// way this always did — idle with more finished turns, or a new epoch.
type schedulerRun struct {
	id      string
	lane    string
	planned time.Time
	// ticket is the receipt the lane issued for this round, empty if it issued
	// none. It is also what "delivered exactly once" means here: a round holds
	// one receipt, and a round that holds one is never written again.
	ticket   string
	finished uint64
	epoch    string
	// owner is the lane run that owns this round — the schedule's owner at the
	// moment the round was started. A loop_next from a different incarnation is
	// a stale run speaking about a round it no longer holds (AC-P4).
	owner workCaller
	// nextSet records a loop_next during this run (AC-P2).
	nextSet bool
}

// agentQueueState is the workload agent's GET /queue (scheduler_agent.go).
type agentQueueState struct {
	Idle     bool   `json:"idle"`
	Finished uint64 `json:"finished"`
	Epoch    string `json:"epoch"`
}

// schedulerAgent is the lanes of this pod as the sidecar reaches them. Every
// call names the lane it is about, because a destination that was implicit
// would be one the scheduler could get wrong exactly once per round.
type schedulerAgent interface {
	queueState(ctx context.Context, lane string) (agentQueueState, error)
	// write queues one prompt and returns the receipt the lane issued for it.
	// An error means the lane did not take it and issued no receipt, so there
	// is nothing in flight to settle and the round can be tried again.
	write(ctx context.Context, lane, prompt string) (ticket string, err error)
	// turnState resolves a receipt against the lane that issued it, as one of
	// turn_ticket.go's states.
	turnState(ctx context.Context, lane, ticket string) (string, error)
	// reaches reports whether this pod has that lane to run anything in. A
	// pod without the pair cannot be asked for a monitor's schedule.
	reaches(lane string) bool
}

type scheduler struct {
	agent  schedulerAgent
	logger *slog.Logger
	now    func() time.Time
	newID  func() string

	mu        sync.Mutex
	schedules map[string]*schedule
	// running and retryAt are per destination lane: one round at a time in
	// each conversation, and a refusal by one lane does not pause the other
	// (AC-P4). Keyed by lane role, empty for a single-conversation session.
	running map[string]*schedulerRun
	retryAt map[string]time.Time
}

func newScheduler(agent schedulerAgent, logger *slog.Logger) *scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &scheduler{
		agent:     agent,
		logger:    logger,
		now:       func() time.Time { return time.Now().UTC() },
		newID:     newScheduleID,
		schedules: map[string]*schedule{},
		running:   map[string]*schedulerRun{},
		retryAt:   map[string]time.Time{},
	}
}

func newScheduleID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "sch_" + hex.EncodeToString(b[:])
}

func (s *scheduler) run(ctx context.Context) {
	t := time.NewTicker(schedulerTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick services every lane that has something to do: settling the round it has
// in flight, or starting the earliest due schedule of that lane when that lane
// is idle (AC-P2, AC-P4). The lanes are serviced independently — a parent in
// the middle of a person's turn leaves the monitor's round due now, not later.
// Agent calls happen outside the lock, so a tool call never waits on a lane.
func (s *scheduler) tick(ctx context.Context) {
	s.mu.Lock()
	now := s.now()
	s.expireLocked(now)
	type laneWork struct {
		lane string
		run  *schedulerRun
		due  *schedule
	}
	var work []laneWork
	for _, lane := range s.destinationsLocked() {
		run := s.running[lane]
		var due *schedule
		if run == nil && !now.Before(s.retryAt[lane]) {
			due = s.earliestLocked(now, lane)
		}
		if run != nil || due != nil {
			work = append(work, laneWork{lane: lane, run: run, due: due})
		}
	}
	s.mu.Unlock()

	for _, w := range work {
		if w.run != nil {
			s.settle(ctx, w.run)
			continue
		}
		s.start(ctx, w.lane, w.due)
	}
}

// destinationsLocked is every lane this scheduler has work for, in a fixed
// order so that two lanes due at the same moment are serviced predictably.
func (s *scheduler) destinationsLocked() []string {
	seen := map[string]bool{}
	for _, sched := range s.schedules {
		seen[sched.dest()] = true
	}
	for lane := range s.running {
		seen[lane] = true
	}
	lanes := make([]string, 0, len(seen))
	for lane := range seen {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	return lanes
}

// settle asks whether the round in flight on one lane is over. The receipt is
// the answer when there is one: done means the turn ran and ended, and the
// three refusals (unknown_run after the lane restarted, expired, absent) mean
// the turn this round was is no longer one this lane can account for — either
// way there is nothing more to wait for, and the schedule may have its next
// run. Only queued and running keep the round open.
func (s *scheduler) settle(ctx context.Context, run *schedulerRun) {
	callCtx, cancel := context.WithTimeout(ctx, schedulerAgentCall)
	defer cancel()

	over := false
	if run.ticket != "" {
		state, err := s.agent.turnState(callCtx, run.lane, run.ticket)
		if err != nil {
			s.logger.Debug("scheduler: receipt unreadable", "lane", run.lane, "schedule", run.id, "err", err)
			return
		}
		switch state {
		case turnTicketQueued, turnTicketRunning:
			return
		default:
			over = true
			if state != turnTicketDone {
				s.logger.Info("scheduler: a round's receipt no longer names a turn of this run",
					"lane", run.lane, "schedule", run.id, "state", state)
			}
		}
	} else {
		// A lane that issued no receipt: idle again with a turn more finished,
		// or a run of the lane that is not the one the round was queued on.
		state, err := s.agent.queueState(callCtx, run.lane)
		if err != nil {
			s.logger.Debug("scheduler: lane queue unreadable", "lane", run.lane, "err", err)
			return
		}
		over = state.Idle && (state.Epoch != run.epoch || state.Finished > run.finished)
	}
	if !over {
		return
	}
	s.mu.Lock()
	if s.running[run.lane] == run {
		s.finishRunLocked(run, s.now())
	}
	s.mu.Unlock()
}

// start puts one due schedule's round on its own lane, once that lane is idle
// — nothing running, nothing queued (AC-P2). The round is claimed before the
// write and released if the lane refuses it, so two ticks cannot both deliver
// it and a refusal leaves no round behind.
func (s *scheduler) start(ctx context.Context, lane string, due *schedule) {
	callCtx, cancel := context.WithTimeout(ctx, schedulerAgentCall)
	defer cancel()
	state, err := s.agent.queueState(callCtx, lane)
	if err != nil {
		s.logger.Debug("scheduler: lane queue unreadable", "lane", lane, "err", err)
		return
	}
	if !state.Idle {
		return
	}

	s.mu.Lock()
	sched, ok := s.schedules[due.id]
	if !ok || s.running[lane] != nil || sched.dest() != lane || sched.nextRun.After(s.now()) {
		s.mu.Unlock()
		return
	}
	sched.runs++
	run := &schedulerRun{
		id: sched.id, lane: lane, planned: sched.nextRun,
		finished: state.Finished, epoch: state.Epoch, owner: sched.owner,
	}
	s.running[lane] = run
	prompt := sched.prompt
	s.mu.Unlock()

	ticket, err := s.agent.write(callCtx, lane, prompt)
	if err != nil {
		// The lane issues no receipt for a prompt it did not queue (AC-N11), so
		// a refusal leaves nothing in flight: the round is given back and tried
		// again after the delay — on this lane only.
		s.logger.Warn("scheduler: could not queue a run; retrying later",
			"lane", lane, "schedule", sched.id, "err", err)
		s.mu.Lock()
		if s.running[lane] == run {
			delete(s.running, lane)
		}
		if current, ok := s.schedules[sched.id]; ok {
			current.runs--
		}
		s.retryAt[lane] = s.now().Add(schedulerRetryDelay)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	if s.running[lane] == run {
		run.ticket = ticket
	}
	s.mu.Unlock()
	s.logger.Info("scheduler: queued a run", "lane", lane, "schedule", sched.id,
		"kind", string(sched.kind), "run", sched.runs, "ticket", ticket)
}

// expireLocked drops loops past their expiry that are not running.
func (s *scheduler) expireLocked(now time.Time) {
	for id, sched := range s.schedules {
		if sched.kind == scheduleOnce || now.Before(sched.expiresAt) {
			continue
		}
		if run := s.running[sched.dest()]; run != nil && run.id == id {
			continue
		}
		delete(s.schedules, id)
		s.logger.Info("scheduler: loop expired", "schedule", id)
	}
}

// earliestLocked is the schedule of one lane that is due soonest.
func (s *scheduler) earliestLocked(now time.Time, lane string) *schedule {
	var due *schedule
	for _, sched := range s.schedules {
		if sched.dest() != lane || sched.nextRun.After(now) {
			continue
		}
		if due == nil || sched.nextRun.Before(due.nextRun) ||
			(sched.nextRun.Equal(due.nextRun) && sched.id < due.id) {
			due = sched
		}
	}
	return due
}

// finishRunLocked sets the schedule's next run (AC-P2): a once schedule is
// gone, a fixed loop skips the slots its run overran, and a self-paced loop
// keeps the time loop_next gave or falls back to its interval.
func (s *scheduler) finishRunLocked(run *schedulerRun, now time.Time) {
	delete(s.running, run.lane)
	sched, ok := s.schedules[run.id]
	if !ok {
		return
	}
	switch sched.kind {
	case scheduleOnce:
		delete(s.schedules, sched.id)
		return
	case scheduleFixed:
		next := run.planned.Add(sched.interval)
		if !next.After(now) {
			missed := now.Sub(run.planned) / sched.interval
			next = run.planned.Add((missed + 1) * sched.interval)
		}
		sched.nextRun = next
	case scheduleSelfPaced:
		if !run.nextSet {
			sched.nextRun = now.Add(sched.interval)
		}
	}
	if !sched.nextRun.Before(sched.expiresAt) {
		delete(s.schedules, sched.id)
		s.logger.Info("scheduler: loop ended at its expiry", "schedule", sched.id)
	}
}

// state is what the agent's GET /scheduler reports (AC-P3).
func (s *scheduler) state() (int, *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next *time.Time
	for _, sched := range s.schedules {
		if next == nil || sched.nextRun.Before(*next) {
			t := sched.nextRun
			next = &t
		}
	}
	return len(s.schedules), next
}

// --- tools (AC-P1) ---

type scheduleView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Prompt    string `json:"prompt"`
	NextRunAt string `json:"nextRunAt"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Interval  string `json:"interval,omitempty"`
	Fallback  string `json:"fallbackInterval,omitempty"`
	Runs      int    `json:"runs"`
	Running   bool   `json:"running"`
}

// runLocked is this schedule's round if it has one in flight. A lane runs one
// round at a time, so that is the round on its destination when the round is
// this schedule's.
func (s *scheduler) runLocked(sched *schedule) *schedulerRun {
	if run := s.running[sched.dest()]; run != nil && run.id == sched.id {
		return run
	}
	return nil
}

func (s *scheduler) viewLocked(sched *schedule) scheduleView {
	v := scheduleView{
		ID:        sched.id,
		Kind:      string(sched.kind),
		Prompt:    previewPrompt(sched.prompt),
		NextRunAt: formatScheduleTime(sched.nextRun),
		Runs:      sched.runs,
		Running:   s.runLocked(sched) != nil,
	}
	switch sched.kind {
	case scheduleFixed:
		v.Interval = isoDuration(sched.interval)
		v.ExpiresAt = formatScheduleTime(sched.expiresAt)
	case scheduleSelfPaced:
		v.Fallback = isoDuration(sched.interval)
		v.ExpiresAt = formatScheduleTime(sched.expiresAt)
	}
	return v
}

func previewPrompt(p string) string {
	if utf8.RuneCountInString(p) <= schedulerPreviewLen {
		return p
	}
	return string([]rune(p)[:schedulerPreviewLen]) + "…"
}

type scheduleOnceArgs struct {
	Prompt string `json:"prompt"`
	Delay  string `json:"delay"`
	At     string `json:"at"`
}

type loopCreateArgs struct {
	Prompt           string `json:"prompt"`
	Interval         string `json:"interval"`
	FallbackInterval string `json:"fallback_interval"`
	ExpiresIn        string `json:"expires_in"`
}

type loopNextArgs struct {
	ID    string `json:"id"`
	Delay string `json:"delay"`
}

type scheduleIDArgs struct {
	ID string `json:"id"`
}

// errLaneNotHere refuses a registration aimed at a conversation this pod does
// not run. It is named rather than quietly falling back to the one lane there
// is: a schedule that ran in the person's chat because the monitor was not
// there is exactly the mix-up the address is meant to prevent (AC-P4).
func (s *scheduler) reachable(caller workCaller) error {
	if s.agent.reaches(caller.lane) {
		return nil
	}
	return fmt.Errorf("this session has no %s conversation to run a schedule in", caller.lane)
}

func (s *scheduler) scheduleOnce(caller workCaller, a scheduleOnceArgs) (scheduleView, error) {
	if err := s.reachable(caller); err != nil {
		return scheduleView{}, err
	}
	if err := validateSchedulePrompt(a.Prompt); err != nil {
		return scheduleView{}, err
	}
	now := s.now()
	var at time.Time
	switch {
	case a.Delay != "" && a.At != "":
		return scheduleView{}, errors.New("give delay or at, not both")
	case a.Delay != "":
		d, err := parseISODuration(a.Delay)
		if err != nil {
			return scheduleView{}, err
		}
		at = now.Add(d)
	case a.At != "":
		t, err := time.Parse(time.RFC3339, a.At)
		if err != nil {
			return scheduleView{}, fmt.Errorf("at must be an RFC 3339 time with an offset, e.g. 2026-11-09T09:00:00+09:00: %w", err)
		}
		at = t.UTC()
	default:
		return scheduleView{}, errors.New("give delay (ISO 8601 duration) or at (RFC 3339 time)")
	}
	if d := at.Sub(now); d < schedulerMinInterval || d > schedulerMaxOnceDelay {
		return scheduleView{}, fmt.Errorf("a one-time schedule must run between %s and %s from now",
			isoDuration(schedulerMinInterval), isoDuration(schedulerMaxOnceDelay))
	}
	return s.add(&schedule{kind: scheduleOnce, prompt: a.Prompt, nextRun: at, owner: caller})
}

func (s *scheduler) loopCreate(caller workCaller, a loopCreateArgs) (scheduleView, error) {
	if err := s.reachable(caller); err != nil {
		return scheduleView{}, err
	}
	if err := validateSchedulePrompt(a.Prompt); err != nil {
		return scheduleView{}, err
	}
	now := s.now()
	sched := &schedule{prompt: a.Prompt, nextRun: now, owner: caller}
	switch {
	case a.Interval != "" && a.FallbackInterval != "":
		return scheduleView{}, errors.New("give interval (fixed loop) or fallback_interval (self-paced loop), not both")
	case a.Interval != "":
		d, err := parseLoopDelay("interval", a.Interval)
		if err != nil {
			return scheduleView{}, err
		}
		sched.kind, sched.interval = scheduleFixed, d
	default:
		sched.kind, sched.interval = scheduleSelfPaced, schedulerDefaultFallback
		if a.FallbackInterval != "" {
			d, err := parseLoopDelay("fallback_interval", a.FallbackInterval)
			if err != nil {
				return scheduleView{}, err
			}
			sched.interval = d
		}
	}
	lifetime := schedulerMaxLifetime
	if a.ExpiresIn != "" {
		d, err := parseISODuration(a.ExpiresIn)
		if err != nil {
			return scheduleView{}, err
		}
		if d < schedulerMinInterval || d > schedulerMaxLifetime {
			return scheduleView{}, fmt.Errorf("expires_in must be between %s and %s",
				isoDuration(schedulerMinInterval), isoDuration(schedulerMaxLifetime))
		}
		lifetime = d
	}
	sched.expiresAt = now.Add(lifetime)
	return s.add(sched)
}

func (s *scheduler) add(sched *schedule) (scheduleView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The limit is the session's, not the lane's (AC-P1), so a pod running two
	// conversations shares it — and says so, because a lane that lists only
	// its own schedules would otherwise be told to cancel one it cannot see.
	if len(s.schedules) >= schedulerMaxSchedules {
		return scheduleView{}, fmt.Errorf("this session already has %d schedules across its conversations; cancel one first",
			schedulerMaxSchedules)
	}
	for {
		sched.id = s.newID()
		if _, taken := s.schedules[sched.id]; !taken {
			break
		}
	}
	s.schedules[sched.id] = sched
	s.logger.Info("scheduler: schedule added", "schedule", sched.id, "kind", string(sched.kind),
		"next_run", formatScheduleTime(sched.nextRun))
	return s.viewLocked(sched), nil
}

// ownedLocked finds one of the caller's own schedules. A schedule belongs to
// the lane that registered it, so for the other lane it is refused by name: a
// lane that could pace or cancel the other's schedules would be acting as that
// lane, which is the one thing the lane address exists to prevent. The role
// alone decides, not the incarnation — a restarted monitor is still this pod's
// monitor and has to be able to end the loops it left running (AC-P4).
func (s *scheduler) ownedLocked(caller workCaller, id string) (*schedule, error) {
	sched, ok := s.schedules[id]
	if !ok {
		return nil, fmt.Errorf("no schedule %q in this session", id)
	}
	if sched.owner.lane != caller.lane {
		return nil, fmt.Errorf("%s belongs to this manager's %s conversation; a lane lists, paces and cancels its own schedules",
			id, laneDescription(sched.owner.lane))
	}
	return sched, nil
}

// laneDescription names a lane for a refusal a model reads.
func laneDescription(lane string) string {
	if lane == "" {
		return "chat"
	}
	return lane
}

func (s *scheduler) loopNext(caller workCaller, a loopNextArgs) (scheduleView, error) {
	d, err := parseLoopDelay("delay", a.Delay)
	if err != nil {
		return scheduleView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, err := s.ownedLocked(caller, a.ID)
	if err != nil {
		return scheduleView{}, err
	}
	if sched.kind != scheduleSelfPaced {
		return scheduleView{}, fmt.Errorf("%s is a %s; loop_next is only for self-paced loops", a.ID, sched.kind)
	}
	// Who owns the round decides this round's next time. A call from a run of
	// the lane that is not the run the round was started for is a leftover of
	// a conversation that is gone, and letting it move the next run would let
	// a dead round pace a live loop.
	run := s.runLocked(sched)
	if run != nil && run.owner.incarnation != caller.incarnation {
		return scheduleView{}, fmt.Errorf("%s is running in an earlier run of this lane; that round sets its own next time", a.ID)
	}
	next := s.now().Add(d)
	if !next.Before(sched.expiresAt) {
		return scheduleView{}, fmt.Errorf("%s expires at %s, before that run",
			a.ID, formatScheduleTime(sched.expiresAt))
	}
	sched.nextRun = next
	if run != nil {
		run.nextSet = true
	}
	return s.viewLocked(sched), nil
}

// list is the caller's own schedules. The other lane's are not in it for the
// reason its conversation is not readable: a schedule is a prompt that becomes
// a turn of that conversation (AC-N11, AC-P4).
func (s *scheduler) list(caller workCaller) []scheduleView {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make([]scheduleView, 0, len(s.schedules))
	for _, sched := range s.schedules {
		if sched.owner.lane != caller.lane {
			continue
		}
		views = append(views, s.viewLocked(sched))
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].NextRunAt != views[j].NextRunAt {
			return views[i].NextRunAt < views[j].NextRunAt
		}
		return views[i].ID < views[j].ID
	})
	return views
}

func (s *scheduler) cancel(caller workCaller, a scheduleIDArgs) (scheduleView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, err := s.ownedLocked(caller, a.ID)
	if err != nil {
		return scheduleView{}, err
	}
	view := s.viewLocked(sched)
	delete(s.schedules, a.ID)
	s.logger.Info("scheduler: schedule cancelled", "schedule", a.ID)
	return view, nil
}

func validateSchedulePrompt(p string) error {
	switch {
	case strings.TrimSpace(p) == "":
		return errors.New("prompt must not be empty")
	case len(p) > schedulerMaxPromptBytes:
		return fmt.Errorf("prompt must be at most %d bytes", schedulerMaxPromptBytes)
	case !utf8.ValidString(p):
		return errors.New("prompt must be valid UTF-8")
	}
	return nil
}

func parseLoopDelay(name, raw string) (time.Duration, error) {
	d, err := parseISODuration(raw)
	if err != nil {
		return 0, err
	}
	if d < schedulerMinInterval || d > schedulerMaxInterval {
		return 0, fmt.Errorf("%s must be between %s and %s", name, isoDuration(schedulerMinInterval), isoDuration(schedulerMaxInterval))
	}
	return d, nil
}

var isoDurationPattern = regexp.MustCompile(`^P(?:(\d{1,6})D)?(?:T(?:(\d{1,6})H)?(?:(\d{1,6})M)?(?:(\d{1,6})S)?)?$`)

// parseISODuration takes the days-and-time subset of ISO 8601 durations
// (PnDTnHnMnS, whole numbers). Anything else — natural language included — is
// refused rather than interpreted (AC-P1).
func parseISODuration(raw string) (time.Duration, error) {
	m := isoDurationPattern.FindStringSubmatch(raw)
	if m == nil || raw == "P" || strings.HasSuffix(raw, "T") {
		return 0, fmt.Errorf("%q is not an ISO 8601 duration such as PT30M, PT2H or P1D", raw)
	}
	units := []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second}
	var total time.Duration
	for i, unit := range units {
		if m[i+1] == "" {
			continue
		}
		n, _ := strconv.Atoi(m[i+1])
		total += time.Duration(n) * unit
	}
	return total, nil
}

func isoDuration(d time.Duration) string {
	if d <= 0 {
		return "PT0S"
	}
	var b strings.Builder
	b.WriteString("P")
	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dD", days)
		d -= days * 24 * time.Hour
	}
	if d > 0 {
		b.WriteString("T")
		if h := d / time.Hour; h > 0 {
			fmt.Fprintf(&b, "%dH", h)
			d -= h * time.Hour
		}
		if m := d / time.Minute; m > 0 {
			fmt.Fprintf(&b, "%dM", m)
			d -= m * time.Minute
		}
		if sec := d / time.Second; sec > 0 {
			fmt.Fprintf(&b, "%dS", sec)
		}
	}
	return b.String()
}

func formatScheduleTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

const schedulerVolatileNote = " Schedules live only in this session's running pod: freezing, restoring or deleting the session drops them."

func schedulerToolDefinitions() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	object := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	prompt := str("The prompt to run in this session when the schedule fires (at most 16 KiB).")
	return []map[string]any{
		{
			"name": "schedule_once",
			"description": "Run a prompt in this session once, later. Give delay as an ISO 8601 duration (PT30M, PT2H, P1D) or at " +
				"as an RFC 3339 time with an offset; between 1 minute and 7 days ahead. Returns the id and the UTC time it will run." +
				schedulerVolatileNote,
			"inputSchema": object(map[string]any{
				"prompt": prompt,
				"delay":  str("How long from now, as an ISO 8601 duration, e.g. PT30M."),
				"at":     str("When, as an RFC 3339 time with an offset, e.g. 2026-11-10T09:00:00+09:00."),
			}, "prompt"),
		},
		{
			"name": "loop_create",
			"description": "Run a prompt in this session repeatedly; the first run starts as soon as the session is free. " +
				"With interval (ISO 8601, 1 minute to 24 hours) it runs at that fixed interval. Without interval it is self-paced: " +
				"each run decides the next with loop_next, or ends the loop with schedule_cancel; a run that does neither runs again " +
				"after fallback_interval (default PT10M). Loops end after expires_in (default and maximum P7D). Runs only start while " +
				"the session is idle, one at a time. Returns the id and the next run time (UTC)." + schedulerVolatileNote,
			"inputSchema": object(map[string]any{
				"prompt":            prompt,
				"interval":          str("Fixed interval between runs, e.g. PT5M. Omit for a self-paced loop."),
				"fallback_interval": str("Self-paced loops only: when to run again if a run calls neither loop_next nor schedule_cancel."),
				"expires_in":        str("How long the loop lives, e.g. PT6H. Default and maximum P7D."),
			}, "prompt"),
		},
		{
			"name":        "loop_next",
			"description": "Set when a self-paced loop runs next: delay is an ISO 8601 duration between 1 minute and 24 hours from now.",
			"inputSchema": object(map[string]any{
				"id":    str("The loop's id, e.g. sch_1a2b3c4d."),
				"delay": str("How long from now, e.g. PT2M."),
			}, "id", "delay"),
		},
		{
			"name":        "schedule_list",
			"description": "List this session's schedules with their next run (UTC), runs so far and expiry.",
			"inputSchema": object(map[string]any{}),
		},
		{
			"name":        "schedule_cancel",
			"description": "Delete a schedule; this is also how a loop ends. A run already started finishes.",
			"inputSchema": object(map[string]any{"id": str("The schedule's id.")}, "id"),
		},
	}
}

// callTool runs one tool. Arguments the tool refuses are a result with
// isError, so the model reads why; only a malformed call is a JSON-RPC error.
func (s *scheduler) callTool(caller workCaller, params json.RawMessage) (any, *jsonRPCError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "tools/call needs a name and arguments"}
	}
	decode := func(dst any) *jsonRPCError {
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			return nil
		}
		decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(dst); err != nil {
			return &jsonRPCError{Code: jsonRPCInvalidParams, Message: call.Name + " arguments: " + err.Error()}
		}
		return nil
	}
	var (
		result any
		err    error
	)
	switch call.Name {
	case "schedule_once":
		var a scheduleOnceArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result, err = s.scheduleOnce(caller, a)
	case "loop_create":
		var a loopCreateArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result, err = s.loopCreate(caller, a)
	case "loop_next":
		var a loopNextArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result, err = s.loopNext(caller, a)
	case "schedule_list":
		var a struct{}
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result = map[string]any{"now": formatScheduleTime(s.now()), "schedules": s.list(caller)}
	case "schedule_cancel":
		var a scheduleIDArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		var view scheduleView
		if view, err = s.cancel(caller, a); err == nil {
			result = map[string]any{"cancelled": view}
		}
	default:
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "unknown tool " + call.Name}
	}
	if err != nil {
		return sessionDBToolResult(map[string]string{"error": err.Error()}, true), nil
	}
	return sessionDBToolResult(result, false), nil
}

// serveMCP has the session DB MCP's JSON-RPC-over-POST shape. The caller is
// the lane whose address this arrived on, never an argument (AC-P4).
func (s *scheduler) serveMCP(w http.ResponseWriter, r *http.Request, caller workCaller) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "scheduler MCP accepts POST", http.StatusMethodNotAllowed)
		return
	}
	var req jsonRPCRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionMCPRequestBytes)).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, jsonRPCParseError, "malformed JSON-RPC request")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSONRPCError(w, req.ID, jsonRPCInvalidRequest, "not a JSON-RPC 2.0 request")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": sessionMCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": schedulerMCPServerName, "version": sessionMCPServerVersion},
		})
	case "tools/list":
		writeJSONRPCResult(w, req.ID, map[string]any{"tools": schedulerToolDefinitions()})
	case "tools/call":
		result, rpcErr := s.callTool(caller, req.Params)
		if rpcErr != nil {
			writeJSONRPCError(w, req.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		writeJSONRPCResult(w, req.ID, result)
	case "ping":
		writeJSONRPCResult(w, req.ID, map[string]any{})
	default:
		writeJSONRPCError(w, req.ID, jsonRPCMethodNotFound, "scheduler MCP does not implement "+req.Method)
	}
}

func (s *scheduler) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "scheduler state accepts GET", http.StatusMethodNotAllowed)
		return
	}
	active, next := s.state()
	out := map[string]any{"active": active}
	if next != nil {
		out["nextRunAt"] = formatScheduleTime(*next)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// withScheduler puts the scheduler's paths in front of the sidecar's other
// handlers: the state path and the one address a single-conversation session
// registers, both matched exactly, and the lane addresses of a manager's two
// (AC-P4). A malformed path under the lane prefix is refused by name rather
// than falling through to the K3s MCP proxy behind this.
func withScheduler(next http.Handler, s *scheduler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if caller, underPrefix, refusal := laneCallerFromPath(schedulerLanePathPrefix, r.URL.Path); underPrefix {
			if refusal != "" {
				http.Error(w, refusal, http.StatusNotFound)
				return
			}
			s.serveMCP(w, r, caller)
			return
		}
		switch r.URL.Path {
		case schedulerMCPPath:
			// The address a session with one conversation registers: its
			// schedules run there, which is where they always ran.
			s.serveMCP(w, r, workCaller{})
		case schedulerStatePath:
			s.serveState(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// httpSchedulerAgent is this pod's lanes on loopback. One base address serves
// both, because the lanes are one listener and a path prefix (manager_lane.go)
// — so there is no second port to get wrong and nothing new reachable from
// outside the pod.
type httpSchedulerAgent struct {
	base string
	// monitor is whether this pod runs the pair. Without it there is one
	// conversation to reach, and a monitor's schedule is refused rather than
	// quietly run in the person's chat.
	monitor bool
	client  *http.Client
}

var errSchedulerAgentURL = errors.New(schedulerAgentURLEnv + " must be an http URL on 127.0.0.1")

func newHTTPSchedulerAgent(raw string, monitor bool) (*httpSchedulerAgent, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.HasPrefix(raw, "http://127.0.0.1:") {
		return nil, errSchedulerAgentURL
	}
	return &httpSchedulerAgent{base: raw, monitor: monitor, client: &http.Client{Timeout: schedulerAgentCall}}, nil
}

func (a *httpSchedulerAgent) reaches(lane string) bool {
	switch lane {
	case "", laneParent:
		return true
	case laneMonitor:
		return a.monitor
	default:
		return false
	}
}

// laneBase is where one lane answers. The parent lane keeps the agent's own
// paths, so a schedule of a session that never heard of lanes reaches exactly
// what it reached before.
func (a *httpSchedulerAgent) laneBase(lane string) string {
	if lane == laneMonitor {
		return a.base + managerLanePathPrefix
	}
	return a.base
}

func (a *httpSchedulerAgent) queueState(ctx context.Context, lane string) (agentQueueState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.laneBase(lane)+agentQueuePath, nil)
	if err != nil {
		return agentQueueState{}, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return agentQueueState{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return agentQueueState{}, fmt.Errorf("agent queue: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var state agentQueueState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return agentQueueState{}, fmt.Errorf("decode agent queue: %w", err)
	}
	return state, nil
}

func (a *httpSchedulerAgent) write(ctx context.Context, lane, prompt string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.laneBase(lane)+"/write", strings.NewReader(prompt))
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("agent write: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// The receipt the lane issued for this prompt (turn_ticket.go). A lane too
	// old to issue one answers without it, and the round is then settled from
	// the queue state the way it always was.
	var queued struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&queued); err != nil {
		return "", nil
	}
	return strings.TrimSpace(queued.Ticket), nil
}

func (a *httpSchedulerAgent) turnState(ctx context.Context, lane, ticket string) (string, error) {
	address := a.laneBase(lane) + agentTurnPath + "?ticket=" + url.QueryEscape(ticket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("agent turn: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Only the state is read: the round's own output belongs to the lane's
	// conversation, and the sidecar has no business holding a copy of it.
	var answer struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&answer); err != nil {
		return "", fmt.Errorf("decode agent turn: %w", err)
	}
	if answer.State == "" {
		return "", errors.New("agent turn: no state")
	}
	return answer.State, nil
}

// schedulerLaneMCPURL is the address one lane registers the scheduler at: the
// sidecar's own, with the lane and this run in the path (AC-P4). It mirrors
// the slot database's derivation (sessionDBLaneMCPURL) and refuses an address
// whose path is not the scheduler MCP's, because a lane address derived from
// some other path would be this code guessing.
func schedulerLaneMCPURL(raw, lane, incarnation string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Path != schedulerMCPPath {
		return "", errors.New(schedulerMCPURLEnv + " must end in " + schedulerMCPPath +
			" for a lane address to be derived from it")
	}
	if !isLaneRole(lane) {
		return "", fmt.Errorf("a manager lane is %s", strings.Join(laneRoles, " or "))
	}
	if !workID.MatchString(incarnation) {
		return "", errors.New("an incarnation is 1..128 letters, digits, _ or -")
	}
	parsed.Path = laneMCPPath(schedulerLanePathPrefix, lane, incarnation)
	return parsed.String(), nil
}
