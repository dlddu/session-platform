// A claude-code turn's tool calls, decided one by one by the session owner
// while the turn waits (docs/prd/claude-code-workload.md AC-E2, 「턴 중 도구
// 승인」).
//
// The runner's canUseTool callback posts one call here on loopback and polls
// for the decision; the control plane lists what is waiting and relays the
// owner's decision on the pod IP. Only calls the runner's own policy already
// allows arrive here — a policy denial is never put to a person — and the desk
// refuses to relay the tools that policy always denies, so a decision cannot
// become a way around it.
//
// A decision names the exact call it was taken on: the request's id, the hash
// of the arguments the model asked for, and the epoch of the run that put it
// up. The desk binds the decision to all three, so an approval of one call
// cannot let another call through, and one taken on what an earlier run showed
// is refused rather than applied here. One call lives only as long as its turn:
// the turn's end, a checkpoint that waits on the turn, the runner giving up, or
// the request's own expiry closes it.
//
// This is not the approval gate of docs/prd/approval-gate.md (approval_wait.go,
// /approval): that one is another type's session MCP asking its gateway, and
// nothing here touches it.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// toolApprovalAskPath is the runner's loopback route: POST asks, GET {id}
	// polls, DELETE {id} withdraws.
	toolApprovalAskPath = "/approvals/ask"
	// toolApprovalsPath is the control plane's: GET lists what is waiting,
	// POST {id}/decide carries the owner's decision.
	toolApprovalsPath = "/approvals"

	maxToolApprovalBodyBytes = 64 << 10
	maxDecidedByRunes        = 200
	// maxPendingToolApprovals bounds what one turn can leave open at once. A
	// turn may run several tool calls at the same time, so this sits above the
	// question desk's bound.
	maxPendingToolApprovals = 8
	maxToolNameRunes        = 200
	maxToolApprovalIDRunes  = 200
	maxDecisionReasonRunes  = 2000
	// maxToolInputBytes is how much of the call's arguments the desk keeps for
	// the owner to read. The runner hashes the whole input either way, so a
	// call with a larger body is refused rather than shown in part — the owner
	// decides on the call as it is, or not at all.
	maxToolInputBytes = 16 << 10
	// toolApprovalTTL is how long one request can stay open. The runner's own
	// wait is shorter, so this bounds a request whose runner went away without
	// withdrawing it, and tells the owner how long the window is.
	toolApprovalTTL = 30 * time.Minute

	toolApprovalStatePending   = "pending"
	toolApprovalStateApproved  = "approved"
	toolApprovalStateDenied    = "denied"
	toolApprovalStateCancelled = "cancelled"

	// toolApprovalApprove and toolApprovalDeny are the decisions the control
	// plane may relay. Anything else is not a decision.
	toolApprovalApprove = "approve"
	toolApprovalDeny    = "deny"

	toolApprovalExpiredReason = "nobody decided before the request expired"
)

var (
	errToolApprovalNoTurn      = errors.New("no turn is running; a tool call belongs to the running turn")
	errToolApprovalClosed      = errors.New("the session is being frozen; nobody can decide now")
	errToolApprovalTooMany     = errors.New("too many tool calls are already waiting in this turn")
	errToolApprovalNotFound    = errors.New("no such pending tool call")
	errToolApprovalInvalid     = errors.New("invalid tool call")
	errToolApprovalBadDecision = errors.New("invalid decision")
	errToolApprovalOtherCall   = errors.New("the decision names another call")
	errToolApprovalUnsupported = errors.New("this workload does not put tool calls to the session owner")
	errToolApprovalRefused     = errors.New("the platform policy decides this tool itself; it is not put to a person")
	errToolApprovalStaleEpoch  = errors.New("the decision was taken on an earlier run of this session")
)

// toolApprovalRelay is an agentCLI whose turns can put one tool call to the
// session owner and wait for the decision inside the turn (claude-code with
// somewhere to relay it). A write asking any other workload for it is refused
// rather than quietly running the turn without it.
type toolApprovalRelay interface {
	relaysToolApprovals() bool
	argvWithToolApprovals(binary, model, prompt string, resume bool) []string
}

// toolApprovalNeverRelayed are the tools the platform policy decides on its
// own in every turn: the plan-mode pair it denies headless, and
// AskUserQuestion, which has its own desk (claude_questions.go). The runner
// never asks about them; the desk refuses them anyway, so no runner can turn a
// standing denial into a decision for the owner.
var toolApprovalNeverRelayed = map[string]bool{
	"AskUserQuestion": true,
	"EnterPlanMode":   true,
	"ExitPlanMode":    true,
}

// toolArgsHashPattern is the shape of the hash the runner takes over the tool
// and its arguments (claude-runner/lib.mjs toolArgsHash).
var toolArgsHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// pendingToolApproval is one tool call waiting on the session owner. The
// owner-facing fields are tagged; the rest is the desk's own bookkeeping.
type pendingToolApproval struct {
	ID string `json:"id"`
	// Turn is the invocation this call belongs to.
	Turn int `json:"-"`
	// Tool is the tool the model asked to use and Input the arguments it asked
	// for — what the owner reads to decide.
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	// ArgsHash names this exact call; a decision carries it back.
	ArgsHash string `json:"argsHash"`
	// ToolUseID and RequestID are the SDK's own ids for the call and for the
	// permission request. RequestID is what a redelivered request rejoins on.
	ToolUseID string `json:"toolUseId"`
	RequestID string `json:"-"`
	// Epoch names the run of this session the call was put up in. A decision
	// carries it back, so one taken on what an earlier run showed the owner is
	// refused instead of landing on this run's desk.
	Epoch string `json:"epoch"`

	AskedAt   time.Time `json:"askedAt"`
	ExpiresAt time.Time `json:"expiresAt"`

	State string `json:"-"`
	// DecidedBy is who the control plane says took the decision, kept for the
	// session's record. The agent cannot authenticate the owner itself: the
	// decide route only accepts a request from outside this pod, and the
	// control plane is what checks that the caller owns the session.
	DecidedBy string    `json:"-"`
	Reason    string    `json:"-"`
	DecidedAt time.Time `json:"-"`
}

// open reports whether the request can still take a decision at now: it is
// pending and its window has not passed.
func (c *pendingToolApproval) open(now time.Time) bool {
	return c.State == toolApprovalStatePending && now.Before(c.ExpiresAt)
}

// toolApprovalDesk holds the running turn's tool calls. It has its own lock so
// the HTTP handlers never wait on the workload's. Expiry is read from the
// caller's clock rather than a timer, so a request that nobody decided is
// closed by the next poll either side makes.
type toolApprovalDesk struct {
	mu    sync.Mutex
	calls map[string]*pendingToolApproval
	// epoch is this run's name for the desk. It is generated on first use and
	// rotated by a restore, so the ids and hashes one run showed the owner
	// cannot be decided on a desk that is no longer the same one.
	epoch string
}

// currentEpoch names this run of the desk, generating it on first use.
func (d *toolApprovalDesk) currentEpoch() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.epochLocked()
}

func (d *toolApprovalDesk) epochLocked() string {
	if d.epoch == "" {
		d.epoch = newRandomID("ep_")
	}
	return d.epoch
}

// rotateEpoch gives the desk a new name and forgets every call of the old one:
// a restore puts a different session state under the same pod, so nothing from
// before it is still the call the owner read.
func (d *toolApprovalDesk) rotateEpoch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.epoch = newRandomID("ep_")
	d.calls = nil
}

// toolApprovalState is the call's state with its expiry applied: a request
// nobody decided in time reads as closed, not pending.
func toolApprovalState(call *pendingToolApproval, now time.Time) string {
	if call.State == toolApprovalStatePending && !now.Before(call.ExpiresAt) {
		return toolApprovalStateCancelled
	}
	return call.State
}

// add opens one request, or rejoins the one already waiting for the same
// control request: the SDK redelivers a permission request with the same id
// after a transport gap, and the owner's single decision has to serve the
// redelivery rather than being asked for twice. A rejoin is only the same call
// — same turn, same tool, same arguments.
func (d *toolApprovalDesk) add(call *pendingToolApproval, now time.Time) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	open := 0
	for _, existing := range d.calls {
		if existing.Turn != call.Turn || !existing.open(now) {
			continue
		}
		if existing.RequestID != "" && existing.RequestID == call.RequestID {
			if existing.Tool != call.Tool || existing.ArgsHash != call.ArgsHash {
				return "", false, fmt.Errorf("%w: this request id is open on another call", errToolApprovalInvalid)
			}
			return existing.ID, true, nil
		}
		open++
	}
	if open >= maxPendingToolApprovals {
		return "", false, errToolApprovalTooMany
	}
	if d.calls == nil {
		d.calls = map[string]*pendingToolApproval{}
	}
	d.calls[call.ID] = call
	return call.ID, false, nil
}

// expire closes every request whose window has passed, as of now, and returns
// what it closed so the session's record can say so.
func (d *toolApprovalDesk) expire(now time.Time) []pendingToolApproval {
	d.mu.Lock()
	defer d.mu.Unlock()
	var closed []pendingToolApproval
	for _, call := range d.calls {
		if call.State == toolApprovalStatePending && !call.open(now) {
			call.State = toolApprovalStateCancelled
			call.Reason = toolApprovalExpiredReason
			call.DecidedAt = now
			closed = append(closed, *call)
		}
	}
	return sortedByAsk(closed)
}

// status is what the runner's poll reads.
func (d *toolApprovalDesk) status(id string, now time.Time) (pendingToolApproval, string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	call, ok := d.calls[id]
	if !ok {
		return pendingToolApproval{}, "", false
	}
	copied := *call
	current := toolApprovalState(call, now)
	if current == toolApprovalStateCancelled && copied.Reason == "" {
		copied.Reason = toolApprovalExpiredReason
	}
	return copied, current, true
}

// pending is what the owner has to decide, oldest first.
func (d *toolApprovalDesk) pending(now time.Time) []pendingToolApproval {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []pendingToolApproval{}
	for _, call := range d.calls {
		if call.open(now) {
			out = append(out, *call)
		}
	}
	return sortedByAsk(out)
}

// decide records the owner's decision on one call. Three things bind it to the
// call the owner actually read: the id, the hash of the arguments — the model
// having changed the call since then is not this call any more — and the epoch
// of the run that put it up. A call that is already decided or closed takes no
// second decision, so a replayed decision changes nothing.
func (d *toolApprovalDesk) decide(id, decision, argsHash, epoch, reason, decidedBy string, now time.Time) (pendingToolApproval, error) {
	if decision != toolApprovalApprove && decision != toolApprovalDeny {
		return pendingToolApproval{}, fmt.Errorf("%w: decide approve or deny", errToolApprovalBadDecision)
	}
	if !toolArgsHashPattern.MatchString(argsHash) {
		return pendingToolApproval{}, fmt.Errorf("%w: name the call with its argsHash", errToolApprovalBadDecision)
	}
	if utf8.RuneCountInString(reason) > maxDecisionReasonRunes || !utf8.ValidString(reason) {
		return pendingToolApproval{}, fmt.Errorf("%w: the reason is too long", errToolApprovalBadDecision)
	}
	if decidedBy != "" && (utf8.RuneCountInString(decidedBy) > maxDecidedByRunes || !utf8.ValidString(decidedBy)) {
		return pendingToolApproval{}, fmt.Errorf("%w: an oversized decider", errToolApprovalBadDecision)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if epoch == "" {
		return pendingToolApproval{}, fmt.Errorf("%w: name the run with its epoch", errToolApprovalBadDecision)
	}
	if epoch != d.epochLocked() {
		return pendingToolApproval{}, errToolApprovalStaleEpoch
	}
	call, ok := d.calls[id]
	if !ok || !call.open(now) {
		return pendingToolApproval{}, errToolApprovalNotFound
	}
	if argsHash != call.ArgsHash {
		return pendingToolApproval{}, errToolApprovalOtherCall
	}
	call.State = toolApprovalStateApproved
	if decision == toolApprovalDeny {
		call.State = toolApprovalStateDenied
	}
	call.Reason = reason
	call.DecidedBy = decidedBy
	call.DecidedAt = now
	return *call, nil
}

// cancel closes one waiting call, reporting whether it was waiting.
func (d *toolApprovalDesk) cancel(id, reason string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	call, ok := d.calls[id]
	if !ok || !call.open(now) {
		return false
	}
	call.State = toolApprovalStateCancelled
	call.Reason = reason
	call.DecidedAt = now
	return true
}

// cancelAll closes every waiting call with reason; the runner reads it on its
// next poll and denies the call.
func (d *toolApprovalDesk) cancelAll(reason string, now time.Time) []pendingToolApproval {
	d.mu.Lock()
	defer d.mu.Unlock()
	var closed []pendingToolApproval
	for _, call := range d.calls {
		if call.open(now) {
			call.State = toolApprovalStateCancelled
			call.Reason = reason
			call.DecidedAt = now
			closed = append(closed, *call)
		}
	}
	return sortedByAsk(closed)
}

// dropTurn forgets a finished turn's calls: nothing polls them any more, and
// no decision can land on a turn that has ended.
func (d *toolApprovalDesk) dropTurn(turn int, now time.Time) []pendingToolApproval {
	d.mu.Lock()
	defer d.mu.Unlock()
	var open []pendingToolApproval
	for id, call := range d.calls {
		if call.Turn == turn {
			if call.open(now) {
				open = append(open, *call)
			}
			delete(d.calls, id)
		}
	}
	return sortedByAsk(open)
}

func sortedByAsk(calls []pendingToolApproval) []pendingToolApproval {
	sort.Slice(calls, func(i, j int) bool { return calls[i].AskedAt.Before(calls[j].AskedAt) })
	return calls
}

// validateToolApprovalAsk checks what the runner posted before it becomes a
// request the owner sees.
func validateToolApprovalAsk(tool, argsHash, toolUseID, requestID string, input json.RawMessage) error {
	if !boundedText(tool, maxToolNameRunes) {
		return fmt.Errorf("%w: name the tool", errToolApprovalInvalid)
	}
	if toolApprovalNeverRelayed[tool] {
		return fmt.Errorf("%w: %s", errToolApprovalRefused, tool)
	}
	if !toolArgsHashPattern.MatchString(argsHash) {
		return fmt.Errorf("%w: argsHash is a sha256 over the tool and its arguments", errToolApprovalInvalid)
	}
	for _, id := range []string{toolUseID, requestID} {
		if id != "" && (utf8.RuneCountInString(id) > maxToolApprovalIDRunes || !utf8.ValidString(id)) {
			return fmt.Errorf("%w: an oversized id", errToolApprovalInvalid)
		}
	}
	switch {
	case len(input) == 0:
		return fmt.Errorf("%w: send the arguments the model asked for", errToolApprovalInvalid)
	case len(input) > maxToolInputBytes:
		return fmt.Errorf("%w: the arguments exceed %d bytes", errToolApprovalInvalid, maxToolInputBytes)
	case !json.Valid(input):
		return fmt.Errorf("%w: the arguments are not JSON", errToolApprovalInvalid)
	}
	return nil
}

// newRandomID is an id nothing outside the desk can guess. crypto/rand.Read
// fills the buffer entirely or panics, so an id is always available.
func newRandomID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func (c *claudeWorkload) relaysToolApprovals() bool {
	relay, ok := c.cli.(toolApprovalRelay)
	return ok && relay.relaysToolApprovals()
}

// askToolApproval opens one call for the running turn and leaves a line in the
// transcript, so the session's record says what the turn waits on and on which
// arguments.
func (c *claudeWorkload) askToolApproval(tool, argsHash, toolUseID, requestID string, input json.RawMessage) (pendingToolApproval, bool, error) {
	if !c.relaysToolApprovals() {
		return pendingToolApproval{}, false, errToolApprovalUnsupported
	}
	if err := validateToolApprovalAsk(tool, argsHash, toolUseID, requestID, input); err != nil {
		return pendingToolApproval{}, false, err
	}
	c.mu.Lock()
	switch {
	case !c.active:
		c.mu.Unlock()
		return pendingToolApproval{}, false, errToolApprovalNoTurn
	case c.checkpointing:
		c.mu.Unlock()
		return pendingToolApproval{}, false, errToolApprovalClosed
	}
	turn := c.turns
	c.mu.Unlock()
	now := c.now().UTC()
	call := &pendingToolApproval{
		ID: newRandomID("ap_"), Turn: turn, Tool: tool, Input: json.RawMessage(c.redactString(string(input))), ArgsHash: argsHash,
		ToolUseID: toolUseID, RequestID: requestID, Epoch: c.toolApprovals.currentEpoch(),
		AskedAt: now, ExpiresAt: now.Add(toolApprovalTTL), State: toolApprovalStatePending,
	}
	id, rejoined, err := c.toolApprovals.add(call, now)
	if err != nil {
		return pendingToolApproval{}, false, err
	}
	if rejoined {
		joined, _, _ := c.toolApprovals.status(id, now)
		return joined, true, nil
	}
	c.appendPlatformNotice(c.redactString(renderToolApprovalNotice(*call, string(input))))
	return *call, false, nil
}

// decideToolApproval records the owner's decision and says so in the session's
// record. The turn reads it on its next poll.
func (c *claudeWorkload) decideToolApproval(id, decision, argsHash, epoch, reason, decidedBy string) (string, error) {
	if !c.relaysToolApprovals() {
		return "", errToolApprovalUnsupported
	}
	call, err := c.toolApprovals.decide(id, decision, argsHash, epoch, reason, decidedBy, c.now().UTC())
	if err != nil {
		return "", err
	}
	c.appendPlatformNotice(c.redactString(renderToolApprovalDecision(call)))
	return call.State, nil
}

// giveUpToolApproval is the runner's own timeout: nobody decided in time.
func (c *claudeWorkload) giveUpToolApproval(id string) {
	if c.toolApprovals.cancel(id, "nobody decided in time", c.now().UTC()) {
		c.appendPlatformNotice(fmt.Sprintf("\n[session-platform: tool call %s was not decided; the call was denied]\n", id))
	}
}

// closeToolApprovalsForCheckpoint lets a turn blocked on the owner finish, so
// the checkpoint that waits on it is not held for the runner's whole wait.
func (c *claudeWorkload) closeToolApprovalsForCheckpoint() {
	for _, call := range c.toolApprovals.cancelAll(errToolApprovalClosed.Error(), c.now().UTC()) {
		c.appendPlatformNotice(fmt.Sprintf("\n[session-platform: tool call %s closed — the session is being frozen]\n", call.ID))
	}
}

// expireToolApprovals closes what nobody decided in time. Both sides read the
// same expiry on every poll, so this only adds the line in the session's
// record.
func (c *claudeWorkload) expireToolApprovals() {
	for _, call := range c.toolApprovals.expire(c.now().UTC()) {
		c.appendPlatformNotice(fmt.Sprintf("\n[session-platform: tool call %s expired undecided; the call was denied]\n", call.ID))
	}
}

func (c *claudeWorkload) dropTurnToolApprovals(turn int) {
	for _, call := range c.toolApprovals.dropTurn(turn, c.now().UTC()) {
		c.appendPlatformNotice(fmt.Sprintf("\n[session-platform: tool call %s closed with its turn]\n", call.ID))
	}
}

func renderToolApprovalNotice(call pendingToolApproval, input string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[session-platform: tool call %s — the turn waits for the session owner to decide]\n", call.ID)
	fmt.Fprintf(&b, "  ? %s %s\n", questionLine(call.Tool), call.ArgsHash)
	fmt.Fprintf(&b, "  ? %s\n", questionLine(input))
	return b.String()
}

func renderToolApprovalDecision(call pendingToolApproval) string {
	owner := "the session owner"
	if call.DecidedBy != "" {
		owner = questionLine(call.DecidedBy)
	}
	line := fmt.Sprintf("\n[session-platform: tool call %s %s by %s]\n", call.ID, call.State, owner)
	if call.Reason != "" {
		line += fmt.Sprintf("  → %s\n", questionLine(call.Reason))
	}
	return line
}

func toolApprovalStatus(err error) int {
	switch {
	case errors.Is(err, errToolApprovalInvalid), errors.Is(err, errToolApprovalBadDecision):
		return http.StatusBadRequest
	case errors.Is(err, errToolApprovalRefused):
		return http.StatusForbidden
	case errors.Is(err, errToolApprovalNotFound), errors.Is(err, errToolApprovalUnsupported):
		return http.StatusNotFound
	case errors.Is(err, errToolApprovalOtherCall), errors.Is(err, errToolApprovalStaleEpoch):
		return http.StatusConflict
	case errors.Is(err, errToolApprovalTooMany):
		return http.StatusTooManyRequests
	case errors.Is(err, errToolApprovalNoTurn), errors.Is(err, errToolApprovalClosed):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func decodeToolApprovalBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxToolApprovalBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", errToolApprovalInvalid, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the JSON body", errToolApprovalInvalid)
	}
	return nil
}

// registerToolApprovalRoutes adds the tool approval routes. The runner's are
// loopback only; the decision refuses anything from inside the pod, so the
// model cannot approve its own tool call with a shell command — only the
// control plane, carrying the owner's decision, can.
func registerToolApprovalRoutes(mux *http.ServeMux, c *claudeWorkload) {
	mux.HandleFunc("POST "+toolApprovalAskPath, func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "tool calls are put up by the session's own runner", http.StatusForbidden)
			return
		}
		var body struct {
			Tool      string          `json:"tool"`
			ToolUseID string          `json:"toolUseId"`
			RequestID string          `json:"requestId"`
			ArgsHash  string          `json:"argsHash"`
			Input     json.RawMessage `json:"input"`
		}
		if err := decodeToolApprovalBody(w, r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		call, rejoined, err := c.askToolApproval(body.Tool, body.ArgsHash, body.ToolUseID, body.RequestID, body.Input)
		if err != nil {
			http.Error(w, err.Error(), toolApprovalStatus(err))
			return
		}
		status := http.StatusCreated
		if rejoined {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": call.ID, "expiresAt": call.ExpiresAt})
	})
	mux.HandleFunc("GET "+toolApprovalAskPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "tool calls are polled by the session's own runner", http.StatusForbidden)
			return
		}
		call, current, ok := c.toolApprovals.status(r.PathValue("id"), c.now().UTC())
		if !ok {
			http.Error(w, errToolApprovalNotFound.Error(), http.StatusNotFound)
			return
		}
		// The runner checks the tool and the hash against the call it asked
		// about before it allows anything, so both ride back with the state.
		out := map[string]any{"state": current, "tool": call.Tool, "argsHash": call.ArgsHash}
		if current != toolApprovalStatePending && call.Reason != "" {
			out["reason"] = call.Reason
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("DELETE "+toolApprovalAskPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "tool calls are withdrawn by the session's own runner", http.StatusForbidden)
			return
		}
		c.giveUpToolApproval(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+toolApprovalsPath, func(w http.ResponseWriter, _ *http.Request) {
		c.expireToolApprovals()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch":     c.toolApprovals.currentEpoch(),
			"approvals": c.toolApprovals.pending(c.now().UTC()),
		})
	})
	mux.HandleFunc("POST "+toolApprovalsPath+"/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		if fromThisPod(r) {
			http.Error(w, "a tool call is decided by the session owner through the control plane", http.StatusForbidden)
			return
		}
		var body struct {
			Decision  string `json:"decision"`
			ArgsHash  string `json:"argsHash"`
			Epoch     string `json:"epoch"`
			Reason    string `json:"reason"`
			DecidedBy string `json:"decidedBy"`
		}
		if err := decodeToolApprovalBody(w, r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		recorded, err := c.decideToolApproval(r.PathValue("id"), body.Decision, body.ArgsHash, body.Epoch, body.Reason, body.DecidedBy)
		if err != nil {
			http.Error(w, err.Error(), toolApprovalStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": recorded})
	})
}
