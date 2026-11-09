// Session scheduler (docs/prd/session-scheduler.md, AC-P1~P3): prompts a
// codex session's model asks to run later or repeatedly. The loopback
// k3s-mcp-proxy sidecar keeps the schedules and the clock and serves the
// `scheduler` MCP; a run is a prompt it puts on the workload agent's own
// queue through loopback POST /write (AC-P2), never through the control plane.
//
// Schedules live only in this process. A sidecar restart or a new pod (freeze
// and restore, delete) drops them, which the tool descriptions say.
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
}

// schedulerRun is the one run the scheduler has on the agent's queue. It is
// over when the agent reports idle with more finished turns than at the
// enqueue, or a new epoch (the agent restarted and lost its queue).
type schedulerRun struct {
	id       string
	planned  time.Time
	finished uint64
	epoch    string
	// nextSet records a loop_next during this run (AC-P2).
	nextSet bool
}

// agentQueueState is the workload agent's GET /queue (scheduler_agent.go).
type agentQueueState struct {
	Idle     bool   `json:"idle"`
	Finished uint64 `json:"finished"`
	Epoch    string `json:"epoch"`
}

type schedulerAgent interface {
	queueState(context.Context) (agentQueueState, error)
	write(context.Context, string) error
}

type scheduler struct {
	agent  schedulerAgent
	logger *slog.Logger
	now    func() time.Time
	newID  func() string

	mu        sync.Mutex
	schedules map[string]*schedule
	running   *schedulerRun
	retryAt   time.Time
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

// tick settles the outstanding run, or starts the earliest due schedule when
// the agent is idle (AC-P2). Agent calls happen outside the lock, so a tool
// call never waits on the agent.
func (s *scheduler) tick(ctx context.Context) {
	s.mu.Lock()
	now := s.now()
	s.expireLocked(now)
	run := s.running
	var due *schedule
	if run == nil && !now.Before(s.retryAt) {
		due = s.earliestLocked(now)
	}
	s.mu.Unlock()
	if run == nil && due == nil {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, schedulerAgentCall)
	defer cancel()
	state, err := s.agent.queueState(callCtx)
	if err != nil {
		s.logger.Debug("scheduler: agent queue unreadable", "err", err)
		return
	}

	if run != nil {
		if !state.Idle || (state.Epoch == run.epoch && state.Finished <= run.finished) {
			return
		}
		s.mu.Lock()
		if s.running == run {
			s.finishRunLocked(run, s.now())
		}
		s.mu.Unlock()
		return
	}
	if !state.Idle {
		return
	}

	s.mu.Lock()
	sched, ok := s.schedules[due.id]
	if !ok || s.running != nil || sched.nextRun.After(s.now()) {
		s.mu.Unlock()
		return
	}
	sched.runs++
	run = &schedulerRun{id: sched.id, planned: sched.nextRun, finished: state.Finished, epoch: state.Epoch}
	s.running = run
	prompt := schedulerRunHeader(sched) + "\n" + sched.prompt
	s.mu.Unlock()

	if err := s.agent.write(callCtx, prompt); err != nil {
		s.logger.Warn("scheduler: could not queue a run; retrying later", "schedule", sched.id, "err", err)
		s.mu.Lock()
		if s.running == run {
			s.running = nil
		}
		if current, ok := s.schedules[sched.id]; ok {
			current.runs--
		}
		s.retryAt = s.now().Add(schedulerRetryDelay)
		s.mu.Unlock()
		return
	}
	s.logger.Info("scheduler: queued a run", "schedule", sched.id, "kind", string(sched.kind), "run", sched.runs)
}

// expireLocked drops loops past their expiry that are not running.
func (s *scheduler) expireLocked(now time.Time) {
	for id, sched := range s.schedules {
		if sched.kind == scheduleOnce || now.Before(sched.expiresAt) {
			continue
		}
		if s.running != nil && s.running.id == id {
			continue
		}
		delete(s.schedules, id)
		s.logger.Info("scheduler: loop expired", "schedule", id)
	}
}

func (s *scheduler) earliestLocked(now time.Time) *schedule {
	var due *schedule
	for _, sched := range s.schedules {
		if sched.nextRun.After(now) {
			continue
		}
		if due == nil || sched.nextRun.Before(due.nextRun) {
			due = sched
		}
	}
	return due
}

// finishRunLocked sets the schedule's next run (AC-P2): a once schedule is
// gone, a fixed loop skips the slots its run overran, and a self-paced loop
// keeps the time loop_next gave or falls back to its interval.
func (s *scheduler) finishRunLocked(run *schedulerRun, now time.Time) {
	s.running = nil
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

func schedulerRunHeader(sched *schedule) string {
	const lead = "[session-platform scheduler] "
	switch sched.kind {
	case scheduleOnce:
		return lead + "one-time schedule " + sched.id + " — it will not run again."
	case scheduleFixed:
		return fmt.Sprintf("%sloop %s run %d — runs every %s until %s; schedule_cancel ends it.",
			lead, sched.id, sched.runs, isoDuration(sched.interval), formatScheduleTime(sched.expiresAt))
	default:
		return fmt.Sprintf("%sself-paced loop %s run %d — before you finish, call loop_next with id %s and when to run again, "+
			"or schedule_cancel to end it; otherwise it runs again %s after this run.",
			lead, sched.id, sched.runs, sched.id, isoDuration(sched.interval))
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

func (s *scheduler) viewLocked(sched *schedule) scheduleView {
	v := scheduleView{
		ID:        sched.id,
		Kind:      string(sched.kind),
		Prompt:    previewPrompt(sched.prompt),
		NextRunAt: formatScheduleTime(sched.nextRun),
		Runs:      sched.runs,
		Running:   s.running != nil && s.running.id == sched.id,
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

func (s *scheduler) scheduleOnce(a scheduleOnceArgs) (scheduleView, error) {
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
	return s.add(&schedule{kind: scheduleOnce, prompt: a.Prompt, nextRun: at})
}

func (s *scheduler) loopCreate(a loopCreateArgs) (scheduleView, error) {
	if err := validateSchedulePrompt(a.Prompt); err != nil {
		return scheduleView{}, err
	}
	now := s.now()
	sched := &schedule{prompt: a.Prompt, nextRun: now}
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
	if len(s.schedules) >= schedulerMaxSchedules {
		return scheduleView{}, fmt.Errorf("this session already has %d schedules; cancel one first", schedulerMaxSchedules)
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

func (s *scheduler) loopNext(a loopNextArgs) (scheduleView, error) {
	d, err := parseLoopDelay("delay", a.Delay)
	if err != nil {
		return scheduleView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[a.ID]
	if !ok {
		return scheduleView{}, fmt.Errorf("no schedule %q in this session", a.ID)
	}
	if sched.kind != scheduleSelfPaced {
		return scheduleView{}, fmt.Errorf("%s is a %s; loop_next is only for self-paced loops", a.ID, sched.kind)
	}
	next := s.now().Add(d)
	if !next.Before(sched.expiresAt) {
		return scheduleView{}, fmt.Errorf("%s expires at %s, before that run; use schedule_cancel to end it",
			a.ID, formatScheduleTime(sched.expiresAt))
	}
	sched.nextRun = next
	if s.running != nil && s.running.id == sched.id {
		s.running.nextSet = true
	}
	return s.viewLocked(sched), nil
}

func (s *scheduler) list() []scheduleView {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make([]scheduleView, 0, len(s.schedules))
	for _, sched := range s.schedules {
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

func (s *scheduler) cancel(a scheduleIDArgs) (scheduleView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sched, ok := s.schedules[a.ID]
	if !ok {
		return scheduleView{}, fmt.Errorf("no schedule %q in this session", a.ID)
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
				"as an RFC 3339 time with an offset; between 1 minute and 7 days ahead. Returns the id and the UTC time it will run — " +
				"tell the user that time." + schedulerVolatileNote,
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
				"the session is idle, one at a time. Tell the user the returned times." + schedulerVolatileNote,
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
func (s *scheduler) callTool(params json.RawMessage) (any, *jsonRPCError) {
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
		result, err = s.scheduleOnce(a)
	case "loop_create":
		var a loopCreateArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result, err = s.loopCreate(a)
	case "loop_next":
		var a loopNextArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result, err = s.loopNext(a)
	case "schedule_list":
		var a struct{}
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		result = map[string]any{"now": formatScheduleTime(s.now()), "schedules": s.list()}
	case "schedule_cancel":
		var a scheduleIDArgs
		if rpcErr := decode(&a); rpcErr != nil {
			return nil, rpcErr
		}
		var view scheduleView
		if view, err = s.cancel(a); err == nil {
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

// serveMCP has the session DB MCP's JSON-RPC-over-POST shape.
func (s *scheduler) serveMCP(w http.ResponseWriter, r *http.Request) {
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
		result, rpcErr := s.callTool(req.Params)
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

// withScheduler puts the scheduler's two paths in front of the sidecar's
// other handlers; both are matched exactly.
func withScheduler(next http.Handler, s *scheduler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case schedulerMCPPath:
			s.serveMCP(w, r)
		case schedulerStatePath:
			s.serveState(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// httpSchedulerAgent is the workload agent on loopback.
type httpSchedulerAgent struct {
	base   string
	client *http.Client
}

var errSchedulerAgentURL = errors.New(schedulerAgentURLEnv + " must be an http URL on 127.0.0.1")

func newHTTPSchedulerAgent(raw string) (*httpSchedulerAgent, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.HasPrefix(raw, "http://127.0.0.1:") {
		return nil, errSchedulerAgentURL
	}
	return &httpSchedulerAgent{base: raw, client: &http.Client{Timeout: schedulerAgentCall}}, nil
}

func (a *httpSchedulerAgent) queueState(ctx context.Context) (agentQueueState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+agentQueuePath, nil)
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

func (a *httpSchedulerAgent) write(ctx context.Context, prompt string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/write", strings.NewReader(prompt))
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agent write: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
