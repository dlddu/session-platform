package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The relay is exercised through the lane address, the way a lane actually
// reaches it (AC-N11): the caller's role comes from the path, so a test that
// passed a role as an argument would be testing something the model can write.

func laneCaller(lane, incarnation string) workCaller {
	return workCaller{lane: lane, incarnation: incarnation}
}

// laneCall posts one tools/call at a lane's own MCP address and returns the
// decoded payload, the work error code when the tool refused, and the JSON-RPC
// error when the guard in front of the arguments refused.
func laneCall(t *testing.T, db *sessionDB, caller workCaller, name string, args map[string]any) (map[string]any, string, *jsonRPCError) {
	t.Helper()
	fillWorkCASArgs(t, db, name, args)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := sessionDBMCPPath
	if caller.lane != "" {
		path = sessionDBLaneMCPPath(caller.lane, caller.incarnation)
	}
	rec := httptest.NewRecorder()
	sessionDBLaneHandler(db, caller).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s as %s: HTTP %d %s", name, caller.lane, rec.Code, rec.Body.String())
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		return nil, "", resp.Error
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &decoded); err != nil {
		t.Fatalf("%s as %s: %v", name, caller.lane, err)
	}
	if result["isError"] == true {
		code, _ := decoded["code"].(string)
		return decoded, code, nil
	}
	return decoded, "", nil
}

func mustLaneCall(t *testing.T, db *sessionDB, caller workCaller, name string, args map[string]any) map[string]any {
	t.Helper()
	decoded, code, rpcErr := laneCall(t, db, caller, name, args)
	if rpcErr != nil {
		t.Fatalf("%s as %s: JSON-RPC error %s", name, caller.lane, rpcErr.Message)
	}
	if code != "" {
		t.Fatalf("%s as %s: %s %v", name, caller.lane, code, decoded["error"])
	}
	return decoded
}

func wantLaneCode(t *testing.T, db *sessionDB, caller workCaller, name string, args map[string]any, want string) map[string]any {
	t.Helper()
	decoded, code, rpcErr := laneCall(t, db, caller, name, args)
	if rpcErr != nil {
		t.Fatalf("%s as %s: JSON-RPC error %s, want work code %s", name, caller.lane, rpcErr.Message, want)
	}
	if code != want {
		t.Fatalf("%s as %s = %q %v, want %s", name, caller.lane, code, decoded, want)
	}
	return decoded
}

func laneNames(t *testing.T, db *sessionDB, caller workCaller) []string {
	t.Helper()
	return sessionDBToolNames(t, sessionDBLaneHandler(db, caller))
}

func relayMessageArgs(kind, subject, body string) map[string]any {
	return map[string]any{"kind": kind, "subject": subject, "body": body}
}

// newRelayTestDB is a slot's database with both lanes' addresses available.
func newRelayTestDB(t *testing.T) (*sessionDB, workCaller, workCaller) {
	t.Helper()
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, laneCaller(relayLaneParent, "parent-run-1"), laneCaller(relayLaneMonitor, "monitor-run-1")
}

// TestLaneSurfaceSplitsReadsFromWrites is the permission separation as a lane
// actually meets it: the parent is not offered the record writes, and the guard
// behind the list refuses them anyway for a client holding an older list. The
// records stay directly readable by the parent — that is the whole point of one
// pod and one database.
func TestLaneSurfaceSplitsReadsFromWrites(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)

	parentTools := laneNames(t, db, parent)
	monitorTools := laneNames(t, db, monitor)

	writes := []string{"task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch",
		taskBookPutTool, monitorPutTool, monitorDeleteTool}
	for _, name := range writes {
		if containsString(parentTools, name) {
			t.Fatalf("the parent lane is offered %s; the monitor lane owns writing the records", name)
		}
		if !containsString(monitorTools, name) {
			t.Fatalf("the monitor lane is not offered %s; it is the lane that writes the records", name)
		}
	}
	reads := []string{"task_get", "task_list", "turn_list", "task_book_get", monitorGetTool, monitorListTool}
	for _, name := range reads {
		if !containsString(parentTools, name) {
			t.Fatalf("the parent lane cannot read %s; it reads the records directly", name)
		}
		if !containsString(monitorTools, name) {
			t.Fatalf("the monitor lane cannot read %s", name)
		}
	}
	for _, name := range relayTools {
		if !containsString(parentTools, name) || !containsString(monitorTools, name) {
			t.Fatalf("%s must be on both lanes; it is how they hand work over", name)
		}
	}

	// A manager with one lane keeps the whole surface: there is no other lane
	// for the records to belong to, so narrowing it would take writes away from
	// the only caller there is.
	single := laneNames(t, db, workCaller{})
	if strings.Join(single, ",") != workToolNames {
		t.Fatalf("an unpaired manager's surface = %v, want the whole one", single)
	}
	for _, name := range relayTools {
		if containsString(single, name) {
			t.Fatalf("an unpaired manager is offered %s; it has no other lane to hand anything to", name)
		}
	}
}

// laneToolDescription is one tool's description as that lane's tools/list
// actually serves it — what the model is given, not what the source says.
func laneToolDescription(t *testing.T, db *sessionDB, caller workCaller, name string) string {
	t.Helper()
	list := sessionDBCall(t, sessionDBLaneHandler(db, caller), "tools/list", map[string]any{})
	for _, tool := range list.Result.(map[string]any)["tools"].([]any) {
		entry := tool.(map[string]any)
		if entry["name"] == name {
			description, _ := entry["description"].(string)
			return description
		}
	}
	t.Fatalf("%s is not on this lane's surface", name)
	return ""
}

// TestRelaySendSaysWhatHandingOverDoesToTheRecipient is the role guidance as
// loaded rather than as intended. A lane gets no instructions file — its whole
// brief is its tool surface — so the two facts a sender acts on have to be in
// these descriptions or they are nowhere: that a parent cannot write the
// records, which is why a change it agreed with the person has to be handed
// over, and that handing over to the monitor starts that lane while handing
// over to the parent does not.
//
// The second one is why this test exists. Before the sidecar woke the lane,
// relay_send truthfully said no turn starts; it now does for the monitor, and a
// parent still reading the old sentence would believe its handover sits there
// until a person pokes the monitor — the exact gap the pair exists to close.
func TestRelaySendSaysWhatHandingOverDoesToTheRecipient(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)

	// In order: that the parent does not write the records, that handing over
	// runs the monitor, that it keeps being asked until settled, and where the
	// parent reads the outcome.
	fromParent := laneToolDescription(t, db, parent, relaySendTool)
	for _, want := range []string{
		"yours to read",
		"is then started",
		"until the message is settled",
		relayStatusTool,
	} {
		if !strings.Contains(fromParent, want) {
			t.Errorf("the parent's %s description does not say %q:\n%s", relaySendTool, want, fromParent)
		}
	}
	if strings.Contains(fromParent, "no turn starts") {
		t.Errorf("the parent's %s description still says no turn starts; the sidecar starts one:\n%s",
			relaySendTool, fromParent)
	}

	// The other direction says the opposite, and must: a turn started here
	// would put text in front of a person who did not speak.
	fromMonitor := laneToolDescription(t, db, monitor, relaySendTool)
	if !strings.Contains(fromMonitor, "No turn starts for the parent") {
		t.Errorf("the monitor's %s description does not say the parent is not started:\n%s",
			relaySendTool, fromMonitor)
	}
	if strings.Contains(fromMonitor, "is then started") {
		t.Errorf("the monitor's %s description claims it starts the parent:\n%s", relaySendTool, fromMonitor)
	}

	// An unpaired manager is shown neither sentence, because it is shown no
	// relay at all.
	if names := laneNames(t, db, workCaller{}); containsString(names, relaySendTool) {
		t.Errorf("an unpaired manager is offered %s", relaySendTool)
	}
}

// TestParentLaneCannotWriteTheRecords is the guard rather than the menu: a
// client holding a list from before the pair still cannot write, and it learns
// which lane owns the records and how to hand the change over.
func TestParentLaneCannotWriteTheRecords(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)

	for _, name := range []string{"task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch",
		taskBookPutTool, monitorPutTool, monitorDeleteTool} {
		_, _, rpcErr := laneCall(t, db, parent, name, map[string]any{"id": "t1", "request_id": "r1"})
		if rpcErr == nil {
			t.Fatalf("%s from the parent lane was not refused in front of the arguments", name)
		}
		for _, want := range []string{name, relayLaneMonitor, relaySendTool, relayStatusTool} {
			if !strings.Contains(rpcErr.Message, want) {
				t.Fatalf("%s refusal %q does not name %q", name, rpcErr.Message, want)
			}
		}
	}

	// The monitor writes, and the parent reads what it wrote — in the same
	// database, with no handover needed for the reading.
	mustLaneCall(t, db, monitor, "task_put", map[string]any{
		"id": "task-1", "request_id": "w1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 하나", "description": "목적",
			"completion_criteria": "머지되면 끝", "status": "planned",
		},
	})
	got := mustLaneCall(t, db, parent, "task_get", map[string]any{"id": "task-1"})
	task := got["task"].(map[string]any)
	if task["title"] != "작업 하나" {
		t.Fatalf("the parent lane read %v, want the task the monitor wrote", task)
	}
	page := mustLaneCall(t, db, parent, "task_list", map[string]any{})
	if len(page["tasks"].([]any)) != 1 {
		t.Fatalf("the parent lane's list = %v, want the one task the monitor wrote", page["tasks"])
	}
}

// TestRelayRoundTripBothWays walks a request, a decision and a result through
// the relay in both directions, which is the exchange the pair is for: the
// parent hands over what it learnt from the person and reads back what came of
// it; the monitor asks and reports.
func TestRelayRoundTripBothWays(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)
	mustLaneCall(t, db, monitor, "task_put", map[string]any{
		"id": "task-1", "request_id": "w1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업", "description": "목적",
			"completion_criteria": "끝", "status": "planned",
		},
	})

	// Parent → monitor: an instruction about a task that exists.
	sent := mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "s1",
		"message": map[string]any{
			"kind": relayKindInstruction, "subject": "이 작업 재개", "body": "사용자가 재개를 선택했다",
			"task_id": "task-1",
		},
	})
	if sent["sender"] != relayLaneParent || sent["recipient"] != relayLaneMonitor {
		t.Fatalf("sent = %v, want parent → monitor; the recipient is derived, not chosen", sent)
	}
	if sent["state"] != relayStatePending || sent["attempts"].(float64) != 0 {
		t.Fatalf("a new message = %v, want pending with no attempts", sent)
	}

	// The monitor sees it in its inbox, and listing is not taking.
	inbox := mustLaneCall(t, db, monitor, relayListTool, map[string]any{"box": "inbox", "open": true})
	messages := inbox["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["id"] != "m1" {
		t.Fatalf("monitor inbox = %v, want the one open message", inbox)
	}
	if messages[0].(map[string]any)["state"] != relayStatePending {
		t.Fatalf("listing moved the message: %v", messages[0])
	}
	// The parent's own outbox is the same message from the other side.
	outbox := mustLaneCall(t, db, parent, relayListTool, map[string]any{"box": "outbox"})
	if len(outbox["messages"].([]any)) != 1 {
		t.Fatalf("parent outbox = %v, want what it sent", outbox)
	}
	if empty := mustLaneCall(t, db, parent, relayListTool, map[string]any{"box": "inbox"}); len(empty["messages"].([]any)) != 0 {
		t.Fatalf("parent inbox = %v, want nothing: it sent this message", empty)
	}

	version := int64(messages[0].(map[string]any)["version"].(float64))
	claim := mustLaneCall(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c1", "expected_version": version,
	})
	token := claim["claim_token"].(string)
	if token == "" {
		t.Fatal("a claim must mint the token that proves the message is this lane's to settle")
	}
	if claim["recovered"] != false || claim["attempts"].(float64) != 1 {
		t.Fatalf("first claim = %v, want recovered=false and one attempt", claim)
	}

	// While it is claimed, the sender's question says "not settled yet".
	pending := mustLaneCall(t, db, parent, relayStatusTool, map[string]any{"id": "m1"})
	if pending["settled"] != false || pending["dealt_with"] != false {
		t.Fatalf("status while claimed = %v, want unsettled", pending)
	}

	mustLaneCall(t, db, monitor, relayAckTool, map[string]any{
		"id": "m1", "request_id": "a1", "claim_token": token,
		"outcome": relayStateProcessed, "result": "작업을 재개했고 turn 을 붙였다",
	})
	settled := mustLaneCall(t, db, parent, relayStatusTool, map[string]any{"id": "m1"})
	if settled["settled"] != true || settled["dealt_with"] != true {
		t.Fatalf("status after ack = %v, want settled and dealt with", settled)
	}
	if got := settled["message"].(map[string]any)["result"]; got != "작업을 재개했고 turn 을 붙였다" {
		t.Fatalf("the sender reads result %v, want what the monitor reported", got)
	}

	// Monitor → parent: a question, answered with a decision. The same
	// mechanism runs the other way, which is what makes it a round trip rather
	// than a one-way drop.
	asked := mustLaneCall(t, db, monitor, relaySendTool, map[string]any{
		"id": "m2", "request_id": "s2",
		"message": relayMessageArgs(relayKindQuestion, "되돌릴까", "CI 가 빨간데 되돌릴지 사용자에게 물어야 한다"),
	})
	if asked["recipient"] != relayLaneParent {
		t.Fatalf("the monitor's message went to %v, want the parent", asked["recipient"])
	}
	answerVersion := int64(asked["version"].(float64))
	answerClaim := mustLaneCall(t, db, parent, relayClaimTool, map[string]any{
		"id": "m2", "request_id": "c2", "expected_version": answerVersion,
	})
	mustLaneCall(t, db, parent, relayAckTool, map[string]any{
		"id": "m2", "request_id": "a2", "claim_token": answerClaim["claim_token"],
		"outcome": relayStateProcessed, "result": "사용자가 되돌리기를 선택했다",
	})
	answered := mustLaneCall(t, db, monitor, relayStatusTool, map[string]any{"id": "m2"})
	if answered["dealt_with"] != true {
		t.Fatalf("the monitor's question = %v, want an answer", answered)
	}

	// A failed outcome is an answer too, so a sender is never left guessing
	// between "refused" and "not looked at".
	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m3", "request_id": "s3",
		"message": relayMessageArgs(relayKindDecision, "중단", "사용자가 중단을 선택했다"),
	})
	status := mustLaneCall(t, db, monitor, relayStatusTool, map[string]any{"id": "m3"})
	claim3 := mustLaneCall(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m3", "request_id": "c3",
		"expected_version": int64(status["message"].(map[string]any)["version"].(float64)),
	})
	mustLaneCall(t, db, monitor, relayAckTool, map[string]any{
		"id": "m3", "request_id": "a3", "claim_token": claim3["claim_token"],
		"outcome": relayStateFailed, "result": "예약을 취소하지 못했다",
	})
	failed := mustLaneCall(t, db, parent, relayStatusTool, map[string]any{"id": "m3"})
	if failed["settled"] != true || failed["dealt_with"] != false {
		t.Fatalf("a failed message = %v, want settled but not dealt with", failed)
	}
}

// TestRelayResendIsNotASecondMessage: a lane whose answer was lost resends the
// same call and learns what it already did, rather than doing it twice.
func TestRelayResendIsNotASecondMessage(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)
	send := func(requestID, body string) map[string]any {
		return map[string]any{
			"id": "m1", "request_id": requestID,
			"message": relayMessageArgs(relayKindInstruction, "재개", body),
		}
	}
	first := mustLaneCall(t, db, parent, relaySendTool, send("s1", "사용자가 재개를 선택했다"))
	again := mustLaneCall(t, db, parent, relaySendTool, send("s1", "사용자가 재개를 선택했다"))
	if first["seq"] != again["seq"] || first["version"] != again["version"] {
		t.Fatalf("a resend returned a different message: %v then %v", first, again)
	}
	if inbox := mustLaneCall(t, db, monitor, relayListTool, map[string]any{}); len(inbox["messages"].([]any)) != 1 {
		t.Fatalf("a resend made a second message: %v", inbox)
	}

	// The same request id with different words is a conflict, not a silent
	// overwrite of something the recipient may already have read.
	wantLaneCode(t, db, parent, relaySendTool, send("s1", "사용자가 중단을 선택했다"), "conflict")
	// A new request id reusing the message id is a conflict too: the recipient's
	// claim would otherwise refer to words it never read.
	wantLaneCode(t, db, parent, relaySendTool, send("s2", "완전히 다른 내용"), "conflict")
	// And the other lane cannot take over an id by sending under it.
	wantLaneCode(t, db, monitor, relaySendTool, map[string]any{
		"id": "m1", "request_id": "s3",
		"message": relayMessageArgs(relayKindResult, "가로채기", "다른 레인의 id 로 보낸다"),
	}, "conflict")

	// A claim whose answer was lost replays the same token rather than taking
	// the message a second time.
	version := int64(first["version"].(float64))
	claimArgs := map[string]any{"id": "m1", "request_id": "c1", "expected_version": version}
	claim := mustLaneCall(t, db, monitor, relayClaimTool, claimArgs)
	replay := mustLaneCall(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c1", "expected_version": version,
	})
	if claim["claim_token"] != replay["claim_token"] || claim["attempts"] != replay["attempts"] {
		t.Fatalf("a replayed claim differs: %v then %v", claim, replay)
	}

	// An ack replayed under the same request id does not settle twice.
	ackArgs := map[string]any{
		"id": "m1", "request_id": "a1", "claim_token": claim["claim_token"],
		"outcome": relayStateProcessed, "result": "했다",
	}
	acked := mustLaneCall(t, db, monitor, relayAckTool, ackArgs)
	ackedAgain := mustLaneCall(t, db, monitor, relayAckTool, map[string]any{
		"id": "m1", "request_id": "a1", "claim_token": claim["claim_token"],
		"outcome": relayStateProcessed, "result": "했다",
	})
	if acked["version"] != ackedAgain["version"] || acked["settled_at"] != ackedAgain["settled_at"] {
		t.Fatalf("a replayed ack settled again: %v then %v", acked, ackedAgain)
	}
	// A settled message is never claimable again.
	status := mustLaneCall(t, db, monitor, relayStatusTool, map[string]any{"id": "m1"})
	wantLaneCode(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c9",
		"expected_version": int64(status["message"].(map[string]any)["version"].(float64)),
	}, "conflict")
}

// TestRelayClaimHasOneWinner: of two callers holding the same version exactly
// one wins, so one message is run once even when both lanes read it together.
func TestRelayClaimHasOneWinner(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)
	sent := mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "s1",
		"message": relayMessageArgs(relayKindInstruction, "재개", "사용자가 재개를 선택했다"),
	})
	version := int64(sent["version"].(float64))

	mustLaneCall(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c1", "expected_version": version,
	})
	// A second claim at the version the first caller read is told to read
	// again. It is a different request id, so this is the swap refusing rather
	// than a receipt replaying.
	wantLaneCode(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c2", "expected_version": version,
	}, "conflict")

	// The sender cannot claim what it sent: a message is the other lane's to
	// take, and there is nothing else to aim it at.
	status := mustLaneCall(t, db, parent, relayStatusTool, map[string]any{"id": "m1"})
	current := int64(status["message"].(map[string]any)["version"].(float64))
	refusal := wantLaneCode(t, db, parent, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c3", "expected_version": current,
	}, "conflict")
	if !strings.Contains(refusal["error"].(string), relayLaneMonitor) {
		t.Fatalf("the refusal %v must name the lane the message is addressed to", refusal)
	}
	// Nor can it settle it.
	wantLaneCode(t, db, parent, relayAckTool, map[string]any{
		"id": "m1", "request_id": "a3", "claim_token": "whatever",
		"outcome": relayStateProcessed,
	}, "conflict")
}

// TestRelayRestartRecoversAClaimAndSaysSo is the restart property: a claim a
// lane held when it died becomes claimable again exactly once, and the
// recovering run is *told* instead of being handed a clean slate it would read
// as a first attempt.
func TestRelayRestartRecoversAClaimAndSaysSo(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	before := laneCaller(relayLaneMonitor, "monitor-run-1")
	after := laneCaller(relayLaneMonitor, "monitor-run-2")

	sent := mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "s1",
		"message": relayMessageArgs(relayKindInstruction, "배포", "배포를 진행한다"),
	})
	lost := mustLaneCall(t, db, before, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c1", "expected_version": int64(sent["version"].(float64)),
	})

	// The same run asking again for a token it already holds is refused: one
	// lane reporting twice on one message is the thing this prevents.
	wantLaneCode(t, db, before, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c2",
		"expected_version": int64(lost["message"].(map[string]any)["version"].(float64)),
	}, "conflict")

	// The pod restarts. A call from a new incarnation of that role is proof the
	// earlier one is gone.
	open := mustLaneCall(t, db, after, relayListTool, map[string]any{"open": true})
	messages := open["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("after a restart the held message must still be open: %v", open)
	}
	recovered := mustLaneCall(t, db, after, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c3",
		"expected_version": int64(messages[0].(map[string]any)["version"].(float64)),
	})
	if recovered["recovered"] != true {
		t.Fatalf("the recovering run was not told it is recovering: %v", recovered)
	}
	if recovered["prior_incarnation"] != before.incarnation {
		t.Fatalf("prior_incarnation = %v, want %q", recovered["prior_incarnation"], before.incarnation)
	}
	if recovered["attempts"].(float64) != 2 {
		t.Fatalf("attempts = %v, want 2 — a recovered claim does not reset the count",
			recovered["attempts"])
	}

	// The run that came back late cannot report over the one working it now.
	wantLaneCode(t, db, before, relayAckTool, map[string]any{
		"id": "m1", "request_id": "a1", "claim_token": lost["claim_token"],
		"outcome": relayStateProcessed, "result": "이전 런이 늦게 보고한다",
	}, "conflict")
	mustLaneCall(t, db, after, relayAckTool, map[string]any{
		"id": "m1", "request_id": "a2", "claim_token": recovered["claim_token"],
		"outcome": relayStateProcessed, "result": "복구 후 확인하고 끝냈다",
	})
	if status := mustLaneCall(t, db, parent, relayStatusTool, map[string]any{"id": "m1"}); status["dealt_with"] != true {
		t.Fatalf("after recovery the sender's question = %v, want dealt with", status)
	}
}

// TestRelayRefusesWhatItCannotActOn pins the argument validation that keeps a
// message a message: a kind outside the closed set, an empty or oversized body,
// and a task link nobody can look up.
func TestRelayRefusesWhatItCannotActOn(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	base := func(message map[string]any) map[string]any {
		return map[string]any{"id": "m-bad", "request_id": "s-bad", "message": message}
	}
	wantLaneCode(t, db, parent, relaySendTool,
		base(relayMessageArgs("gossip", "주제", "내용")), "invalid_argument")
	wantLaneCode(t, db, parent, relaySendTool,
		base(relayMessageArgs(relayKindInstruction, "  ", "내용")), "invalid_argument")
	wantLaneCode(t, db, parent, relaySendTool,
		base(relayMessageArgs(relayKindInstruction, "주제", "")), "invalid_argument")
	wantLaneCode(t, db, parent, relaySendTool,
		base(relayMessageArgs(relayKindInstruction, "주제", strings.Repeat("x", relayBodyMax+1))), "invalid_argument")
	wantLaneCode(t, db, parent, relaySendTool,
		base(relayMessageArgs(relayKindInstruction, strings.Repeat("x", relaySubjectMax+1), "내용")), "invalid_argument")

	// A message about a task nobody can look up is one the recipient cannot
	// act on, and dropping the link silently would hide that the sender meant
	// a task at all.
	missing := map[string]any{
		"kind": relayKindInstruction, "subject": "주제", "body": "내용", "task_id": "task-missing",
	}
	wantLaneCode(t, db, parent, relaySendTool, base(missing), "not_found")

	// An unknown message is not found rather than created, and a message of
	// neither lane does not exist here.
	wantLaneCode(t, db, parent, relayStatusTool, map[string]any{"id": "nope"}, "not_found")
	wantLaneCode(t, db, parent, relayClaimTool,
		map[string]any{"id": "nope", "request_id": "c1", "expected_version": 1}, "not_found")
}

// TestRelayIsNotOfferedToAnUnpairedManager: a tool whose only possible answer
// is a refusal should not be shown, and calling it anyway says why rather than
// reading as a name this image does not have.
func TestRelayIsNotOfferedToAnUnpairedManager(t *testing.T) {
	db, _, _ := newRelayTestDB(t)
	for _, name := range relayTools {
		_, _, rpcErr := laneCall(t, db, workCaller{}, name, map[string]any{
			"id": "m1", "request_id": "s1",
			"message": relayMessageArgs(relayKindInstruction, "주제", "내용"),
		})
		if rpcErr == nil {
			t.Fatalf("%s on an unpaired manager was not refused in front of the arguments", name)
		}
		// The refusal has to say what is wrong — that this database serves one
		// lane — rather than that the arguments were malformed, which is what
		// the generic field check behind it would have said.
		for _, want := range []string{name, "lane"} {
			if !strings.Contains(rpcErr.Message, want) {
				t.Fatalf("%s refusal %q does not name %q", name, rpcErr.Message, want)
			}
		}
	}
}

// TestRelayRowsAreNotInTheWorkViews: the handover is management state between
// two lanes, so it must not leak into the reads the app and the person get.
func TestRelayRowsAreNotInTheWorkViews(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)
	mustLaneCall(t, db, monitor, "task_put", map[string]any{
		"id": "task-1", "request_id": "w1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업", "description": "목적",
			"completion_criteria": "끝", "status": "planned",
		},
	})
	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "s1",
		"message": map[string]any{
			"kind": relayKindInstruction, "subject": "재개", "body": "사용자가 재개를 선택했다",
			"task_id": "task-1",
		},
	})
	status := mustLaneCall(t, db, monitor, relayStatusTool, map[string]any{"id": "m1"})
	claim := mustLaneCall(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c1",
		"expected_version": int64(status["message"].(map[string]any)["version"].(float64)),
	})

	// The claim token is internal: it is minted into the claim's own reply and
	// never appears in a read a person or the app gets.
	page, err := db.workTasksView(context.Background(), workArgs{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{claim["claim_token"].(string), "사용자가 재개를 선택했다", "relay"} {
		if bytes.Contains(bytes.ToLower(raw), bytes.ToLower([]byte(leak))) {
			t.Fatalf("the work view carries %q: %s", leak, raw)
		}
	}
	// Reading a message is not the same as reading the task it names: the task
	// view still shows the task.
	if tasks := page.(map[string]any)["tasks"]; len(tasks.([]workTaskView)) != 1 {
		t.Fatalf("work tasks = %v, want the one task", tasks)
	}
}
