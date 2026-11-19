package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A request as the tools answer it, decoded for assertions.
type requestReply struct {
	Request struct {
		ID              string `json:"id"`
		TaskID          string `json:"task_id"`
		Seq             int64  `json:"seq"`
		Version         int64  `json:"version"`
		Text            string `json:"text"`
		Author          string `json:"author"`
		Status          string `json:"status"`
		ClaimOwner      string `json:"claim_owner"`
		OperationKey    string `json:"operation_key"`
		OperationTarget string `json:"operation_target"`
		DispatchState   string `json:"dispatch_state"`
		RelatedSeq      *int64 `json:"related_seq"`
		Result          string `json:"result"`
	} `json:"request"`
	ClaimToken string `json:"claim_token"`
	Reconcile  bool   `json:"reconcile"`
}

func decodeRequestReply(t *testing.T, raw any) requestReply {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out requestReply
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type requestList struct {
	Requests []struct {
		ID            string `json:"id"`
		TaskID        string `json:"task_id"`
		Seq           int64  `json:"seq"`
		Status        string `json:"status"`
		Text          string `json:"text"`
		DispatchState string `json:"dispatch_state"`
	} `json:"requests"`
}

func listRequests(t *testing.T, s *sessionDB, a ledgerArgs) requestList {
	t.Helper()
	raw, err := s.ledger(context.Background(), "ledger_requests_list", a)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out requestList
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// freezeLedgerClock pins the lifecycle's clock so a claim's lapse can be
// reached by moving time instead of waiting for it.
func freezeLedgerClock(t *testing.T) func(time.Duration) {
	t.Helper()
	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	offset := time.Duration(0)
	ledgerNow = func() time.Time { return base.Add(offset) }
	t.Cleanup(func() { ledgerNow = time.Now })
	return func(d time.Duration) { offset += d }
}

func putTask(t *testing.T, s *sessionDB, id, requestID string, version int64) {
	t.Helper()
	if _, err := s.ledger(context.Background(), "ledger_put", taskArgs(id, requestID, version)); err != nil {
		t.Fatal(err)
	}
}

// A request is stored only against a task this ledger actually has, and the
// request's own id is its idempotency key: the app's retry of an answer it
// never saw stores nothing twice, and a used id with different words is a
// mistake rather than a second request.
func TestLedgerUserRequestsAppendIsIdempotentAndTaskLinked(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)

	if _, _, err := s.appendLedgerRequest(ctx, "r1", "absent", "이 작업 진행해 줘", "user-7", "", ledgerNow()); err == nil {
		t.Fatal("a request about a task the ledger does not have must be refused")
	} else {
		assertLedgerCode(t, err, "not_found")
	}

	putTask(t, s, "t1", "create-t1", 0)
	stored, replayed, err := s.appendLedgerRequest(ctx, "r1", "t1", "이 작업 진행해 줘", "user-7", "", ledgerNow())
	if err != nil {
		t.Fatal(err)
	}
	if replayed || stored.Status != requestPending || stored.Seq != 1 || stored.Version != 1 || stored.Author != "user-7" {
		t.Fatalf("stored = %+v, want a fresh pending request", stored)
	}

	again, replayed, err := s.appendLedgerRequest(ctx, "r1", "t1", "이 작업 진행해 줘", "user-7", "", ledgerNow())
	if err != nil || !replayed || again.Seq != stored.Seq {
		t.Fatalf("replay = %+v, %v, %v — want the stored row", again, replayed, err)
	}
	if got := listRequests(t, s, ledgerArgs{}); len(got.Requests) != 1 {
		t.Fatalf("requests = %d, want the retry to have stored nothing twice", len(got.Requests))
	}

	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "다른 말", "user-7", "", ledgerNow()); err == nil {
		t.Fatal("the same id with different words must conflict")
	} else {
		assertLedgerCode(t, err, "conflict")
	}

	for _, bad := range []struct{ id, task, text, author string }{
		{"r2", "t1", "   ", "user-7"},
		{"r2", "t1", "말", "열린 문자열"},
		{"공백 아이디", "t1", "말", "user-7"},
	} {
		if _, _, err := s.appendLedgerRequest(ctx, bad.id, bad.task, bad.text, bad.author, "", ledgerNow()); err == nil {
			t.Fatalf("%+v must be refused", bad)
		} else {
			assertLedgerCode(t, err, "invalid_argument")
		}
	}
}

// Storing a request leaves the task exactly as the parent wrote it: the task's
// own version does not move, so an update the parent had already prepared still
// lands. This is why requests live in their own table.
func TestLedgerUserRequestsDoNotDisturbTaskUpdates(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)

	// The parent reads version 1 and is about to write version 2.
	before, err := s.ledger(ctx, "ledger_get", ledgerArgs{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := before.(ledgerRecord).Version; got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}

	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "요청", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}

	// The prepared update still applies — the append did not take the version
	// out from under it.
	update := taskArgs("t1", "update-t1", 1)
	update.Task.Details = map[string]any{"approval": "PR only", "observation": "parent's own note"}
	after, err := s.ledger(ctx, "ledger_put", update)
	if err != nil {
		t.Fatalf("the parent's update must still land: %v", err)
	}
	record := after.(ledgerRecord)
	if record.Version != 2 || record.Task.Details["observation"] != "parent's own note" {
		t.Fatalf("task = %+v, want the parent's own details at version 2", record)
	}
	if got := listRequests(t, s, ledgerArgs{TaskID: "t1"}); len(got.Requests) != 1 || got.Requests[0].Status != requestPending {
		t.Fatalf("requests = %+v, want the request untouched by the task update", got.Requests)
	}
}

// Several requests on one task are consumed in arrival order, and a plain
// ledger_get or ledger_list already shows that something is waiting — a parent
// that only ever reads its task list still finds out.
func TestLedgerUserRequestsOrderAndDiscovery(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	putTask(t, s, "t2", "create-t2", 0)
	for _, r := range []struct{ id, task, text string }{
		{"r1", "t1", "첫 번째"}, {"r2", "t2", "다른 작업"}, {"r3", "t1", "두 번째"},
	} {
		if _, _, err := s.appendLedgerRequest(ctx, r.id, r.task, r.text, "user-7", "", ledgerNow()); err != nil {
			t.Fatal(err)
		}
	}

	all := listRequests(t, s, ledgerArgs{})
	if len(all.Requests) != 3 || all.Requests[0].ID != "r1" || all.Requests[1].ID != "r2" || all.Requests[2].ID != "r3" {
		t.Fatalf("order = %+v, want arrival order by seq", all.Requests)
	}
	one := listRequests(t, s, ledgerArgs{TaskID: "t1"})
	if len(one.Requests) != 2 || one.Requests[0].ID != "r1" || one.Requests[1].ID != "r3" {
		t.Fatalf("task filter = %+v, want r1 then r3", one.Requests)
	}
	pending := listRequests(t, s, ledgerArgs{Status: requestPending})
	if len(pending.Requests) != 3 {
		t.Fatalf("pending = %d, want 3", len(pending.Requests))
	}

	got, err := s.ledger(ctx, "ledger_get", ledgerArgs{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	summary := got.(ledgerRecord).Requests
	if summary == nil || summary.Pending != 2 || summary.LastSeq != 3 {
		t.Fatalf("ledger_get summary = %+v, want 2 pending and last seq 3", summary)
	}
	page, err := s.ledger(ctx, "ledger_list", ledgerArgs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.(map[string]any)["tasks"].([]ledgerRecord) {
		if record.Requests == nil {
			t.Fatalf("task %s carries no request summary", record.ID)
		}
	}
}

// One request goes to one parent. Two parents claiming the same version is a
// race with exactly one winner, and reading the list is not taking anything.
func TestLedgerUserRequestsClaimIsExclusive(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "요청", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}

	// Listing it twice settles nothing.
	listRequests(t, s, ledgerArgs{})
	if got := listRequests(t, s, ledgerArgs{}); got.Requests[0].Status != requestPending {
		t.Fatalf("status after listing = %s, want it still pending", got.Requests[0].Status)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for _, owner := range []string{"parent-a", "parent-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			_, err := s.ledger(ctx, "ledger_requests_claim", ledgerArgs{
				ID: "r1", RequestID: "claim-" + owner, ExpectedVersion: lv(1), ClaimOwner: owner,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				return
			}
			var le ledgerError
			if jsonAs(err, &le) && le.Code == "conflict" {
				conflicts++
			}
		}(owner)
	}
	wg.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d, want exactly one winner", wins, conflicts)
	}
}

// Only an ack closes a request, it needs the claim that is actually held, and
// it records what happened — the result and the turn that carries it.
func TestLedgerUserRequestsAckNeedsTheHeldClaim(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "요청", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ledger(ctx, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim", ExpectedVersion: lv(1), ClaimOwner: "parent-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := decodeRequestReply(t, claimed)
	if claim.Request.Status != requestClaimed || claim.ClaimToken == "" || claim.Reconcile {
		t.Fatalf("claim = %+v, want a plain claim with a token", claim)
	}

	if _, err := s.ledger(ctx, "ledger_requests_ack", ledgerArgs{
		ID: "r1", RequestID: "ack-wrong", ExpectedVersion: lv(claim.Request.Version),
		ClaimToken: "0123456789abcdef0123456789abcdef", Outcome: requestProcessed, Result: "했다",
	}); err == nil {
		t.Fatal("an ack without the held claim must be refused")
	} else {
		assertLedgerCode(t, err, "conflict")
	}

	acked, err := s.ledger(ctx, "ledger_requests_ack", ledgerArgs{
		ID: "r1", RequestID: "ack", ExpectedVersion: lv(claim.Request.Version), ClaimToken: claim.ClaimToken,
		Outcome: requestProcessed, Result: "원장에 적어 두고 진행했다", RelatedSeq: lv(42),
	})
	if err != nil {
		t.Fatal(err)
	}
	settled := decodeRequestReply(t, acked)
	if settled.Request.Status != requestProcessed || settled.Request.Result == "" ||
		settled.Request.RelatedSeq == nil || *settled.Request.RelatedSeq != 42 ||
		settled.Request.DispatchState != dispatchSettled {
		t.Fatalf("ack = %+v, want processed with result and related seq", settled.Request)
	}

	// A settled request cannot be claimed again: it is answered, not waiting.
	if _, err := s.ledger(ctx, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim-again", ExpectedVersion: lv(settled.Request.Version), ClaimOwner: "parent-b",
	}); err == nil {
		t.Fatal("a processed request must not be claimable")
	} else {
		assertLedgerCode(t, err, "conflict")
	}
}

// The case the record exists for: the parent dispatched something and died
// before acking. The lapsed claim does NOT become pending — it becomes a
// reconciliation that names what was dispatched, and sending something new on
// it requires having established that the first attempt never landed.
func TestLedgerUserRequestsCrashAfterDispatchBecomesReconciliation(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	advance := freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "PR 올려 줘", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}
	first := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim-a", ExpectedVersion: lv(1), ClaimOwner: "parent-a", TTLSeconds: lv(60),
	}))

	// Intent first, then the hand-over — both before the ack that never comes.
	intended := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_dispatch", ledgerArgs{
		ID: "r1", RequestID: "intend", ExpectedVersion: lv(first.Request.Version), ClaimToken: first.ClaimToken,
		OperationKey: "pr-413-comment", OperationTarget: "pr/413", DispatchState: dispatchIntended,
	}))
	dispatched := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_dispatch", ledgerArgs{
		ID: "r1", RequestID: "dispatch", ExpectedVersion: lv(intended.Request.Version), ClaimToken: first.ClaimToken,
		OperationKey: "pr-413-comment", OperationTarget: "pr/413", DispatchState: dispatchDispatched,
	}))
	if dispatched.Request.DispatchState != dispatchDispatched || dispatched.Request.Status != requestClaimed {
		t.Fatalf("dispatch = %+v, want a dispatched claim", dispatched.Request)
	}

	// ...the parent dies here. The claim lapses.
	advance(2 * time.Minute)

	// Its own token is now worthless: a late survivor cannot report over
	// whoever picks the request up next.
	if _, err := s.ledger(ctx, "ledger_requests_ack", ledgerArgs{
		ID: "r1", RequestID: "late-ack", ExpectedVersion: lv(dispatched.Request.Version), ClaimToken: first.ClaimToken,
		Outcome: requestProcessed, Result: "늦게 돌아와서 적는다",
	}); err == nil {
		t.Fatal("a lapsed claim must not be able to ack")
	} else {
		assertLedgerCode(t, err, "conflict")
	}

	second := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim-b", ExpectedVersion: lv(dispatched.Request.Version), ClaimOwner: "parent-b",
	}))
	if !second.Reconcile || second.Request.Status != requestReconciling ||
		second.Request.OperationKey != "pr-413-comment" || second.Request.OperationTarget != "pr/413" {
		t.Fatalf("re-claim = %+v, want a reconciliation naming the dispatch", second)
	}

	// A different operation on a reconciling request is refused: the lapse is
	// not evidence that nothing landed.
	if _, err := s.ledger(ctx, "ledger_requests_dispatch", ledgerArgs{
		ID: "r1", RequestID: "redo", ExpectedVersion: lv(second.Request.Version), ClaimToken: second.ClaimToken,
		OperationKey: "pr-413-comment-again", OperationTarget: "pr/413", DispatchState: dispatchDispatched,
	}); err == nil {
		t.Fatal("a new operation on a reconciling request must be refused without evidence")
	} else {
		assertLedgerCode(t, err, "conflict")
	}

	// Re-sending under the same operation key is always allowed: that is the
	// key the outside world deduplicates on.
	same := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_dispatch", ledgerArgs{
		ID: "r1", RequestID: "retry-same-key", ExpectedVersion: lv(second.Request.Version), ClaimToken: second.ClaimToken,
		OperationKey: "pr-413-comment", OperationTarget: "pr/413", DispatchState: dispatchDispatched,
	}))

	// And a new operation is allowed once the parent says it established the
	// first one never landed.
	fresh := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_dispatch", ledgerArgs{
		ID: "r1", RequestID: "redo-observed", ExpectedVersion: lv(same.Request.Version), ClaimToken: second.ClaimToken,
		OperationKey: "pr-413-comment-2", OperationTarget: "pr/413", DispatchState: dispatchDispatched,
		Observed: "not_accepted",
	}))
	if fresh.Request.OperationKey != "pr-413-comment-2" {
		t.Fatalf("observed dispatch = %+v, want the new operation recorded", fresh.Request)
	}

	// The reconciliation closes like any other claim — with an outcome.
	settled := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_ack", ledgerArgs{
		ID: "r1", RequestID: "ack-b", ExpectedVersion: lv(fresh.Request.Version), ClaimToken: second.ClaimToken,
		Outcome: requestFailed, Result: "첫 시도는 닿지 않았고 다시 보낸 뒤 실패로 닫았다",
	}))
	if settled.Request.Status != requestFailed {
		t.Fatalf("ack = %+v, want failed", settled.Request)
	}
}

// A claim that lapses before anything was dispatched is simply claimable
// again: nothing can have happened outside, so there is nothing to reconcile.
func TestLedgerUserRequestsLapsedClaimWithoutDispatchIsPlain(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	advance := freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "요청", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}
	first := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim-a", ExpectedVersion: lv(1), ClaimOwner: "parent-a", TTLSeconds: lv(60),
	}))
	advance(2 * time.Minute)
	second := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim-b", ExpectedVersion: lv(first.Request.Version), ClaimOwner: "parent-b",
	}))
	if second.Reconcile || second.Request.Status != requestClaimed {
		t.Fatalf("re-claim = %+v, want a plain claim", second)
	}
	// A claim still in force is not takeable.
	if _, err := s.ledger(ctx, "ledger_requests_claim", ledgerArgs{
		ID: "r1", RequestID: "claim-c", ExpectedVersion: lv(second.Request.Version), ClaimOwner: "parent-c",
	}); err == nil {
		t.Fatal("a live claim must not be takeable")
	} else {
		assertLedgerCode(t, err, "conflict")
	}
}

// Deleting the task does not delete what a person asked about it: the request
// stays readable, with its words, and a new request about the deleted task is
// refused rather than stored where nobody would look for it.
func TestLedgerUserRequestsSurviveTaskDeletion(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "지워지기 전에 물어본 말", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ledger(ctx, "ledger_delete", ledgerArgs{ID: "t1", RequestID: "delete-t1", ExpectedVersion: lv(1)}); err != nil {
		t.Fatal(err)
	}
	kept := listRequests(t, s, ledgerArgs{TaskID: "t1"})
	if len(kept.Requests) != 1 || kept.Requests[0].Text != "지워지기 전에 물어본 말" {
		t.Fatalf("requests after deletion = %+v, want the request kept", kept.Requests)
	}
	if _, _, err := s.appendLedgerRequest(ctx, "r2", "t1", "지워진 뒤에 보낸 말", "user-7", "", ledgerNow()); err == nil {
		t.Fatal("a request about a deleted task must be refused")
	} else {
		assertLedgerCode(t, err, "conflict")
	}
}

// A mutation that is retried returns its first committed answer, so a parent
// that lost the reply does not claim, dispatch or ack twice.
func TestLedgerUserRequestsMutationsReplay(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	if _, _, err := s.appendLedgerRequest(ctx, "r1", "t1", "요청", "user-7", "", ledgerNow()); err != nil {
		t.Fatal(err)
	}
	args := ledgerArgs{ID: "r1", RequestID: "claim", ExpectedVersion: lv(1), ClaimOwner: "parent-a"}
	first, err := s.ledger(ctx, "ledger_requests_claim", args)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ledger(ctx, "ledger_requests_claim", args)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(replay)
	if string(a) != string(b) {
		t.Fatalf("replay differs:\n%s\n%s", a, b)
	}
	args.ClaimOwner = "parent-b"
	if _, err := s.ledger(ctx, "ledger_requests_claim", args); err == nil {
		t.Fatal("the same request_id with different arguments must conflict")
	} else {
		assertLedgerCode(t, err, "conflict")
	}
}

// The sidecar's write view is the only way in from outside, it is loopback
// only, and it answers in the sidecar's own words: created, replayed, or why
// not.
func TestLedgerRequestsViewStoresFromLoopbackOnly(t *testing.T) {
	db, h := ledgerViewHandler(t)
	freezeLedgerClock(t)
	putTask(t, db, "t1", "create-t1", 0)

	post := func(remote string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, sessionDBLedgerRequestsPath, strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	stored := `{"id":"r1","task_id":"t1","text":"요청","author":"user-7"}`

	if rec := post("10.0.0.5:4000", stored); rec.Code != http.StatusForbidden {
		t.Fatalf("off-pod POST = %d, want 403", rec.Code)
	}
	rec := post("127.0.0.1:54321", stored)
	if rec.Code != http.StatusCreated {
		t.Fatalf("store = %d %s, want 201", rec.Code, rec.Body.String())
	}
	var created struct {
		Request  ledgerRequest `json:"request"`
		Replayed bool          `json:"replayed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Replayed || created.Request.Status != requestPending || created.Request.Author != "user-7" {
		t.Fatalf("created = %+v", created)
	}
	if rec := post("127.0.0.1:54321", stored); rec.Code != http.StatusOK {
		t.Fatalf("replay = %d, want 200", rec.Code)
	}
	if rec := post("127.0.0.1:54321", `{"id":"r9","task_id":"absent","text":"요청","author":"user-7"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task = %d, want 404", rec.Code)
	}
	if rec := post("127.0.0.1:54321", `{"id":"r1","task_id":"t1","text":"다른 말","author":"user-7"}`); rec.Code != http.StatusConflict {
		t.Fatalf("reused id = %d, want 409", rec.Code)
	}
	if rec := post("127.0.0.1:54321", `{"id":"r8","task_id":"t1","text":"요청","author":"user-7","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", rec.Code)
	}
	// GET is the task view's verb, not this one's.
	req := httptest.NewRequest(http.MethodGet, sessionDBLedgerRequestsPath, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}

// The parent's surface grew by exactly the four request tools.
func TestLedgerRequestsToolsAreOffered(t *testing.T) {
	_, h := newTestSessionDB(t)
	names := strings.Join(sessionDBToolNames(t, h), ",")
	want := ledgerToolNames
	if names != want {
		t.Fatalf("tools = %s, want %s", names, want)
	}
}

func mustLedger(t *testing.T, s *sessionDB, name string, a ledgerArgs) any {
	t.Helper()
	out, err := s.ledger(context.Background(), name, a)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// jsonAs is errors.As for the ledger's own error type, kept local so the
// concurrency assertions read as one line.
func jsonAs(err error, target *ledgerError) bool {
	if e, ok := err.(ledgerError); ok {
		*target = e
		return true
	}
	return false
}
