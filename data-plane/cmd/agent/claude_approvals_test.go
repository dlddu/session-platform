package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	testApprovalTool  = "Bash"
	testApprovalInput = `{"command":"rm -rf /tmp/scratch"}`
)

// approvalHash is the hash the runner takes over the tool and its arguments
// (claude-runner/lib.mjs toolArgsHash); the desk only checks its shape, so the
// tests use fixed hashes that have it.
func approvalHash(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64/len(seed))
}

var (
	testApprovalArgsHash  = approvalHash("ab")
	testApprovalOtherHash = approvalHash("cd")
)

// newApprovalWorkload is a claude-code workload whose runner has somewhere to
// relay a tool call, with the control plane on an address that is not this
// pod's — the same harness the question desk is tested on
// (claude_questions_test.go), since both ride the same agent routes.
func newApprovalWorkload(t *testing.T, runner *fakeClaudeRunner) (*claudeWorkload, questionClient) {
	t.Helper()
	return newQuestionWorkload(t, runner)
}

// approvalTranscript is the session's record as a person reads it.
func approvalTranscript(t *testing.T, c *claudeWorkload) string {
	t.Helper()
	payload, _ := c.out.Since(0)
	return string(payload)
}

// startApprovalTurn queues a write that asked for the owner's decision and
// waits for the turn to start, so a call can be put up inside it.
func startApprovalTurn(t *testing.T, runner *fakeClaudeRunner, q questionClient) recordedClaudeRun {
	t.Helper()
	if code, body := q.do(http.MethodPost, "/write?"+claudeToolApprovalsParam+"=true", "do the thing", false); code != http.StatusOK {
		t.Fatalf("write = %d %s", code, body)
	}
	return receiveClaudeRun(t, runner.started)
}

func askApprovalBody(tool, argsHash, requestID, input string) string {
	body, _ := json.Marshal(map[string]any{
		"tool":      tool,
		"toolUseId": "toolu_1",
		"requestId": requestID,
		"argsHash":  argsHash,
		"input":     json.RawMessage(input),
	})
	return string(body)
}

// askApprovalID puts one call up from loopback, as the runner does.
func askApprovalID(t *testing.T, q questionClient, body string) (string, int) {
	t.Helper()
	code, resp := q.do(http.MethodPost, toolApprovalAskPath, body, true)
	if code != http.StatusCreated && code != http.StatusOK {
		return "", code
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || !strings.HasPrefix(out.ID, "ap_") {
		t.Fatalf("ask answer = %q", resp)
	}
	return out.ID, code
}

// listApprovals is what the control plane reads: the run's epoch and what is
// waiting in it.
func listApprovals(t *testing.T, q questionClient) (string, []map[string]any, string) {
	t.Helper()
	code, resp := q.do(http.MethodGet, toolApprovalsPath, "", false)
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, resp)
	}
	var out struct {
		Epoch     string           `json:"epoch"`
		Approvals []map[string]any `json:"approvals"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("list body = %q: %v", resp, err)
	}
	return out.Epoch, out.Approvals, resp
}

func decideBody(decision, argsHash, epoch, reason, decidedBy string) string {
	body, _ := json.Marshal(map[string]any{
		"decision":  decision,
		"argsHash":  argsHash,
		"epoch":     epoch,
		"reason":    reason,
		"decidedBy": decidedBy,
	})
	return string(body)
}

func pollApproval(t *testing.T, q questionClient, id string) (int, map[string]any) {
	t.Helper()
	code, resp := q.do(http.MethodGet, toolApprovalAskPath+"/"+id, "", true)
	var out map[string]any
	_ = json.Unmarshal([]byte(resp), &out)
	return code, out
}

// AC-E2 「턴 중 도구 승인」: one call waits for the owner, the control plane
// reads exactly what the model asked for, the decision is relayed on the pod
// IP, the runner's poll reads it with the tool and hash it asked about, and
// the session's record says both what was asked and who decided.
func TestClaudeToolApprovalRoundTrip(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)

	run := startApprovalTurn(t, runner, q)
	argv := strings.Join(run.argv, " ")
	if !strings.Contains(argv, "--approvals-url http://127.0.0.1:8090") {
		t.Fatalf("argv = %v, want the agent's loopback URL for approvals", run.argv)
	}
	if !strings.Contains(argv, "--questions-url http://127.0.0.1:8090") {
		t.Fatalf("argv = %v, want the questions URL kept alongside", run.argv)
	}

	id, code := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	if code != http.StatusCreated {
		t.Fatalf("ask = %d", code)
	}

	epoch, pending, raw := listApprovals(t, q)
	if epoch == "" || len(pending) != 1 {
		t.Fatalf("list = %s", raw)
	}
	call := pending[0]
	if call["id"] != id || call["tool"] != testApprovalTool || call["argsHash"] != testApprovalArgsHash || call["epoch"] != epoch {
		t.Fatalf("pending call = %v, want the call as asked in this run", call)
	}
	if !strings.Contains(raw, `"command":"rm -rf /tmp/scratch"`) {
		t.Fatalf("the owner must read the arguments the model asked for: %s", raw)
	}
	if _, state := pollApproval(t, q, id); state["state"] != toolApprovalStatePending {
		t.Fatalf("poll before a decision = %v", state)
	}

	// The model must not approve its own call: a decision from inside the pod
	// is refused however it is dressed up.
	approve := decideBody(toolApprovalApprove, testApprovalArgsHash, epoch, "go ahead", "owner@example")
	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", approve, true); code != http.StatusForbidden {
		t.Fatalf("a decision from inside the pod = %d %s, want 403", code, body)
	}
	podIP := &net.IPNet{IP: net.ParseIP("10.42.0.9"), Mask: net.CIDRMask(24, 32)}
	saved := localInterfaceAddrs
	localInterfaceAddrs = func() ([]net.Addr, error) { return []net.Addr{podIP}, nil }
	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", approve, false); code != http.StatusForbidden {
		t.Fatalf("a decision from the pod's own IP = %d %s, want 403", code, body)
	}
	localInterfaceAddrs = saved

	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", approve, false); code != http.StatusOK {
		t.Fatalf("the owner's decision = %d %s", code, body)
	}
	code, state := pollApproval(t, q, id)
	if code != http.StatusOK || state["state"] != toolApprovalStateApproved {
		t.Fatalf("poll after the decision = %d %v", code, state)
	}
	if state["tool"] != testApprovalTool || state["argsHash"] != testApprovalArgsHash {
		t.Fatalf("the poll must carry the call back for the runner to check: %v", state)
	}

	// A decided call takes no second decision, so a replay changes nothing.
	deny := decideBody(toolApprovalDeny, testApprovalArgsHash, epoch, "", "")
	if code, _ := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", deny, false); code != http.StatusNotFound {
		t.Fatalf("a second decision = %d, want 404", code)
	}
	if _, pending, _ := listApprovals(t, q); len(pending) != 0 {
		t.Fatalf("a decided call must leave the owner's list: %v", pending)
	}

	close(runner.release)
	waitClaudeIdle(t, c)
	transcript := approvalTranscript(t, c)
	for _, want := range []string{
		"tool call " + id,
		testApprovalArgsHash,
		"approved by owner@example",
		"go ahead",
	} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript must record %q:\n%s", want, transcript)
		}
	}
}

// A denial is the owner's decision too: the call is refused with the reason,
// and the turn goes on.
func TestClaudeToolApprovalDenial(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	id, _ := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	epoch, _, _ := listApprovals(t, q)
	deny := decideBody(toolApprovalDeny, testApprovalArgsHash, epoch, "not on production data", "owner@example")
	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", deny, false); code != http.StatusOK {
		t.Fatalf("deny = %d %s", code, body)
	}
	_, state := pollApproval(t, q, id)
	if state["state"] != toolApprovalStateDenied || state["reason"] != "not on production data" {
		t.Fatalf("poll after the denial = %v", state)
	}
	if !strings.Contains(approvalTranscript(t, c), "denied by owner@example") {
		t.Fatal("the session's record must say the owner denied the call")
	}
}

// Nobody decided in time: the request closes itself on the next poll either
// side makes, and a late decision has nothing to land on.
func TestClaudeToolApprovalExpires(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	id, _ := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	epoch, _, _ := listApprovals(t, q)

	c.now = func() time.Time { return testTurnTime.Add(toolApprovalTTL + time.Second) }
	if _, pending, _ := listApprovals(t, q); len(pending) != 0 {
		t.Fatalf("an expired call must not be offered for a decision: %v", pending)
	}
	_, state := pollApproval(t, q, id)
	if state["state"] != toolApprovalStateCancelled || state["reason"] != toolApprovalExpiredReason {
		t.Fatalf("poll after the window = %v", state)
	}
	late := decideBody(toolApprovalApprove, testApprovalArgsHash, epoch, "", "")
	if code, _ := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", late, false); code != http.StatusNotFound {
		t.Fatalf("a decision after the window = %d, want 404", code)
	}
	if !strings.Contains(approvalTranscript(t, c), "expired undecided") {
		t.Fatal("the session's record must say the call expired")
	}
}

// The decision is bound to the call the owner read: the same id with another
// argument hash is not this call, and the desk refuses rather than approving
// what the model changed underneath.
func TestClaudeToolApprovalBindsToTheArguments(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	_, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	id, _ := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	epoch, _, _ := listApprovals(t, q)

	other := decideBody(toolApprovalApprove, testApprovalOtherHash, epoch, "", "")
	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", other, false); code != http.StatusConflict {
		t.Fatalf("a decision on other arguments = %d %s, want 409", code, body)
	}
	if _, state := pollApproval(t, q, id); state["state"] != toolApprovalStatePending {
		t.Fatalf("the call must still be waiting: %v", state)
	}
	for _, bad := range []string{
		decideBody("allow", testApprovalArgsHash, epoch, "", ""),
		decideBody(toolApprovalApprove, "not-a-hash", epoch, "", ""),
		decideBody(toolApprovalApprove, testApprovalArgsHash, epoch, strings.Repeat("x", maxDecisionReasonRunes+1), ""),
		decideBody(toolApprovalApprove, testApprovalArgsHash, epoch, "", strings.Repeat("y", maxDecidedByRunes+1)),
	} {
		if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", bad, false); code != http.StatusBadRequest {
			t.Fatalf("decision %s = %d %s, want 400", bad, code, body)
		}
	}
}

// The decision is bound to the run that put the call up: what an earlier run
// showed the owner cannot be decided on this one, and a restore starts a new
// run under the same pod.
func TestClaudeToolApprovalBindsToTheRun(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	id, _ := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	epoch, _, _ := listApprovals(t, q)

	stale := decideBody(toolApprovalApprove, testApprovalArgsHash, "ep_0000000000000000", "", "")
	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", stale, false); code != http.StatusConflict {
		t.Fatalf("a decision from another run = %d %s, want 409", code, body)
	}
	unnamed := decideBody(toolApprovalApprove, testApprovalArgsHash, "", "", "")
	if code, body := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", unnamed, false); code != http.StatusBadRequest {
		t.Fatalf("a decision naming no run = %d %s, want 400", code, body)
	}

	c.toolApprovals.rotateEpoch()
	if after, pending, _ := listApprovals(t, q); after == epoch || len(pending) != 0 {
		t.Fatalf("a restore must start a new run with an empty desk: %s %v", after, pending)
	}
	replay := decideBody(toolApprovalApprove, testApprovalArgsHash, epoch, "", "")
	if code, _ := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", replay, false); code != http.StatusConflict {
		t.Fatalf("a decision from before the restore = %d, want 409", code)
	}
}

// The SDK redelivers a permission request with the same id after a transport
// gap. The redelivery joins the one request already waiting, so the owner is
// asked once; the same id on another call is a mistake, not a rejoin.
func TestClaudeToolApprovalRedeliveryJoinsOneRequest(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	_, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	body := askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput)
	id, code := askApprovalID(t, q, body)
	if code != http.StatusCreated {
		t.Fatalf("first ask = %d", code)
	}
	rejoined, code := askApprovalID(t, q, body)
	if code != http.StatusOK || rejoined != id {
		t.Fatalf("redelivery = %d %s, want 200 and %s", code, rejoined, id)
	}
	if _, pending, _ := listApprovals(t, q); len(pending) != 1 {
		t.Fatalf("the owner must be asked once: %v", pending)
	}
	changed := askApprovalBody(testApprovalTool, testApprovalOtherHash, "req_1", `{"command":"ls"}`)
	if code, resp := q.do(http.MethodPost, toolApprovalAskPath, changed, true); code != http.StatusBadRequest {
		t.Fatalf("the same request id on another call = %d %s, want 400", code, resp)
	}
}

// Several calls of one turn can wait at once, and the bound is what one turn
// may leave open rather than a limit on the session.
func TestClaudeToolApprovalBoundsOneTurn(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	_, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	for i := 0; i < maxPendingToolApprovals; i++ {
		body := askApprovalBody(testApprovalTool, approvalHash(fmt.Sprintf("%02x", i)), fmt.Sprintf("req_%d", i), testApprovalInput)
		if _, code := askApprovalID(t, q, body); code != http.StatusCreated {
			t.Fatalf("ask %d = %d", i, code)
		}
	}
	over := askApprovalBody(testApprovalTool, approvalHash("ff"), "req_over", testApprovalInput)
	if code, resp := q.do(http.MethodPost, toolApprovalAskPath, over, true); code != http.StatusTooManyRequests {
		t.Fatalf("one call over the bound = %d %s, want 429", code, resp)
	}
	if _, pending, _ := listApprovals(t, q); len(pending) != maxPendingToolApprovals {
		t.Fatalf("pending = %d, want %d", len(pending), maxPendingToolApprovals)
	}
}

// The relay stays inside the platform's policy: it never carries the tools the
// policy decides itself, it only runs where the runner has somewhere to relay,
// a call belongs to a running turn, and only the runner may put one up.
func TestClaudeToolApprovalPermissionBoundary(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)

	// No turn is running yet: a call has no turn to belong to.
	early := askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_0", testApprovalInput)
	if code, body := q.do(http.MethodPost, toolApprovalAskPath, early, true); code != http.StatusConflict {
		t.Fatalf("a call outside a turn = %d %s, want 409", code, body)
	}

	startApprovalTurn(t, runner, q)
	for tool := range toolApprovalNeverRelayed {
		body := askApprovalBody(tool, testApprovalArgsHash, "req_"+tool, `{}`)
		if code, resp := q.do(http.MethodPost, toolApprovalAskPath, body, true); code != http.StatusForbidden {
			t.Fatalf("%s = %d %s, want 403: the policy decides it itself", tool, code, resp)
		}
	}
	for _, body := range []string{
		askApprovalBody("", testApprovalArgsHash, "req_1", testApprovalInput),
		askApprovalBody(testApprovalTool, "sha256:nothex", "req_1", testApprovalInput),
		askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", ""),
		askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", `{"big":"`+strings.Repeat("x", maxToolInputBytes)+`"}`),
	} {
		if code, resp := q.do(http.MethodPost, toolApprovalAskPath, body, true); code != http.StatusBadRequest {
			t.Fatalf("invalid ask = %d %s, want 400", code, resp)
		}
	}
	// Only the session's own runner puts a call up or polls it.
	valid := askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_2", testApprovalInput)
	if code, _ := q.do(http.MethodPost, toolApprovalAskPath, valid, false); code != http.StatusForbidden {
		t.Fatalf("an ask from outside the pod = %d, want 403", code)
	}
	id, _ := askApprovalID(t, q, valid)
	if code, _ := q.do(http.MethodGet, toolApprovalAskPath+"/"+id, "", false); code != http.StatusForbidden {
		t.Fatalf("a poll from outside the pod = %d, want 403", code)
	}

	// A write asking for the owner's decision where the runner has nowhere to
	// relay it is refused, rather than running the turn without it.
	close(runner.release)
	waitClaudeIdle(t, c)
	c.cli = claudeCLI{}
	code, body := q.do(http.MethodPost, "/write?"+claudeToolApprovalsParam+"=true", "do the thing", false)
	if code != http.StatusBadRequest || !strings.Contains(body, "cannot put its tool calls") {
		t.Fatalf("write without a relay = %d %s, want 400", code, body)
	}
	// The flag is a flag: a turn never quietly runs without what it asked for.
	if code, body := q.do(http.MethodPost, "/write?"+claudeToolApprovalsParam+"=1", "do the thing", false); code != http.StatusBadRequest {
		t.Fatalf("write with a misspelled flag = %d %s, want 400", code, body)
	}
}

// A write that did not ask for it runs with no approvals URL at all, so the
// turn's calls are decided by the policy as before.
func TestClaudeTurnWithoutApprovalsHasNoRelay(t *testing.T) {
	runner := newFakeClaudeRunner()
	_, q := newApprovalWorkload(t, runner)

	if code, body := q.do(http.MethodPost, "/write", "do the thing", false); code != http.StatusOK {
		t.Fatalf("write = %d %s", code, body)
	}
	run := receiveClaudeRun(t, runner.started)
	if strings.Contains(strings.Join(run.argv, " "), "--approvals-url") {
		t.Fatalf("argv = %v, want no approvals URL", run.argv)
	}
}

// A session being frozen cannot wait for a person: the pending calls close so
// the checkpoint is not held for the runner's whole wait, and the turn goes on
// with the denial.
func TestClaudeToolApprovalsCloseForACheckpoint(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)
	t.Cleanup(func() { close(runner.release) })
	startApprovalTurn(t, runner, q)

	id, _ := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	c.closeToolApprovalsForCheckpoint()

	_, state := pollApproval(t, q, id)
	if state["state"] != toolApprovalStateCancelled || !strings.Contains(fmt.Sprint(state["reason"]), "frozen") {
		t.Fatalf("poll after the freeze = %v", state)
	}
	// While the checkpoint holds the session, a new call is refused rather
	// than opened for a decision nobody can take.
	c.mu.Lock()
	c.checkpointing = true
	c.mu.Unlock()
	if code, _ := q.do(http.MethodPost, toolApprovalAskPath, askApprovalBody(testApprovalTool, testApprovalOtherHash, "req_2", testApprovalInput), true); code != http.StatusConflict {
		t.Fatalf("a call while the session is being frozen = %d, want 409", code)
	}
	c.mu.Lock()
	c.checkpointing = false
	c.mu.Unlock()
}

// A call lives only as long as its turn: when the turn ends, the desk forgets
// it and no decision can land on it afterwards.
func TestClaudeToolApprovalEndsWithItsTurn(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newApprovalWorkload(t, runner)
	startApprovalTurn(t, runner, q)

	id, _ := askApprovalID(t, q, askApprovalBody(testApprovalTool, testApprovalArgsHash, "req_1", testApprovalInput))
	epoch, _, _ := listApprovals(t, q)
	close(runner.release)
	waitClaudeIdle(t, c)

	if code, _ := pollApproval(t, q, id); code != http.StatusNotFound {
		t.Fatalf("poll after the turn = %d, want 404", code)
	}
	late := decideBody(toolApprovalApprove, testApprovalArgsHash, epoch, "", "")
	if code, _ := q.do(http.MethodPost, toolApprovalsPath+"/"+id+"/decide", late, false); code != http.StatusNotFound {
		t.Fatalf("a decision after the turn = %d, want 404", code)
	}
	if !strings.Contains(approvalTranscript(t, c), "closed with its turn") {
		t.Fatal("the session's record must say the call closed with its turn")
	}
}
