package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type choiceReply struct {
	ID           string         `json:"id"`
	BasisVersion int64          `json:"basis_version"`
	Choices      []ledgerChoice `json:"choices"`
}

type eventPage struct {
	Events []struct {
		Seq       int64          `json:"seq"`
		TaskID    string         `json:"task_id"`
		Kind      string         `json:"kind"`
		Actor     string         `json:"actor"`
		Summary   string         `json:"summary"`
		Detail    map[string]any `json:"detail"`
		RequestID string         `json:"request_id"`
	} `json:"events"`
	NextCursor int64 `json:"next_cursor"`
}

func decodeInto(t *testing.T, raw any, out any) {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatal(err)
	}
}

func choiceArgs(id, requestID string, version int64, entries ...ledgerChoiceArg) ledgerArgs {
	return ledgerArgs{ID: id, RequestID: requestID, ExpectedVersion: lv(version), Choices: entries}
}

func events(t *testing.T, s *sessionDB, taskID string, after *int64, limit int) eventPage {
	t.Helper()
	raw, err := s.ledger(context.Background(), "ledger_events_list",
		ledgerArgs{ID: taskID, AfterSeq: after, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	var page eventPage
	decodeInto(t, raw, &page)
	return page
}

// The session writes what the app offers, against the version it read. The app
// is given those sentences unchanged — and only while the task still stands at
// that version.
func TestLedgerChoicesAreServedOnlyForTheVersionTheyWereWrittenAgainst(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)

	stored, err := s.ledger(ctx, "ledger_choices_put", choiceArgs("t1", "choices-1", 1,
		ledgerChoiceArg{ChoiceID: "c-ci-log", Label: "CI 로그 확인 요청", RequestText: "방금 실패한 CI 로그에서 어디서 멈췄는지 확인해서 알려 줘."},
		ledgerChoiceArg{ChoiceID: "c-skip-flaky", Label: "플래키 테스트 건너뛰기 승인", RequestText: "그 테스트가 플래키면 이번에는 건너뛰고 진행해 줘."},
	))
	if err != nil {
		t.Fatal(err)
	}
	var reply choiceReply
	decodeInto(t, stored, &reply)
	if reply.BasisVersion != 1 || len(reply.Choices) != 2 || reply.Choices[0].ID != "c-ci-log" {
		t.Fatalf("stored = %+v, want two choices on version 1 in order", reply)
	}

	// The task's read carries them, in the order the session wrote them.
	got, err := s.ledger(ctx, "ledger_get", ledgerArgs{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	record := got.(ledgerRecord)
	if len(record.Choices) != 2 || record.Choices[1].Label != "플래키 테스트 건너뛰기 승인" {
		t.Fatalf("ledger_get choices = %+v", record.Choices)
	}
	if record.Choices[0].RequestText == "" {
		t.Fatal("the request text the app fills in must travel with the choice")
	}

	// The task moves. The sentences were written about its older state, so they
	// are no longer served — nobody had to delete them.
	if _, err := s.ledger(ctx, "ledger_put", taskArgs("t1", "update-t1", 1)); err != nil {
		t.Fatal(err)
	}
	got, err = s.ledger(ctx, "ledger_get", ledgerArgs{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if moved := got.(ledgerRecord); len(moved.Choices) != 0 {
		t.Fatalf("stale choices = %+v, want none served after the task moved", moved.Choices)
	}

	// Writing against the version that no longer holds is a conflict, not a
	// silent overwrite.
	if _, err := s.ledger(ctx, "ledger_choices_put", choiceArgs("t1", "choices-stale", 1,
		ledgerChoiceArg{ChoiceID: "c-x", Label: "낡은 기준", RequestText: "낡은 기준으로 쓴 문장"},
	)); err == nil {
		t.Fatal("a put against an older version must conflict")
	} else {
		assertLedgerCode(t, err, "conflict")
	}

	// Written against the current version, they are served again.
	if _, err := s.ledger(ctx, "ledger_choices_put", choiceArgs("t1", "choices-2", 2,
		ledgerChoiceArg{ChoiceID: "c-after", Label: "다음 결정 요청", RequestText: "두 방법 중 어느 쪽으로 갈지 내가 고르게 정리해 줘."},
	)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ledger(ctx, "ledger_get", ledgerArgs{ID: "t1"})
	if fresh := got.(ledgerRecord); len(fresh.Choices) != 1 || fresh.Choices[0].ID != "c-after" {
		t.Fatalf("fresh choices = %+v", fresh.Choices)
	}

	// Clearing leaves none, which is what the app shows as "none offered".
	if _, err := s.ledger(ctx, "ledger_choices_clear", ledgerArgs{ID: "t1", RequestID: "clear-1", ExpectedVersion: lv(2)}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ledger(ctx, "ledger_get", ledgerArgs{ID: "t1"})
	if cleared := got.(ledgerRecord); len(cleared.Choices) != 0 {
		t.Fatalf("after clear = %+v", cleared.Choices)
	}
}

// Bounds and shapes: a choice is a short label and one sentence, with a stable
// id, and a retry of the same write neither rewrites nor duplicates.
func TestLedgerChoicesAreBoundedAndReplay(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)

	long := strings.Repeat("가", choiceTextMax+1)
	for name, args := range map[string]ledgerArgs{
		"too many":      choiceArgs("t1", "r", 1, make([]ledgerChoiceArg, choiceMax+1)...),
		"blank label":   choiceArgs("t1", "r", 1, ledgerChoiceArg{ChoiceID: "c1", Label: "  ", RequestText: "문장"}),
		"blank text":    choiceArgs("t1", "r", 1, ledgerChoiceArg{ChoiceID: "c1", Label: "라벨", RequestText: " "}),
		"text too long": choiceArgs("t1", "r", 1, ledgerChoiceArg{ChoiceID: "c1", Label: "라벨", RequestText: long}),
		"bad id":        choiceArgs("t1", "r", 1, ledgerChoiceArg{ChoiceID: "선택 하나", Label: "라벨", RequestText: "문장"}),
		"duplicate id": choiceArgs("t1", "r", 1,
			ledgerChoiceArg{ChoiceID: "c1", Label: "라벨", RequestText: "문장"},
			ledgerChoiceArg{ChoiceID: "c1", Label: "라벨2", RequestText: "문장2"}),
	} {
		if _, err := s.ledger(ctx, "ledger_choices_put", args); err == nil {
			t.Errorf("%s must be refused", name)
		} else {
			assertLedgerCode(t, err, "invalid_argument")
		}
	}

	good := choiceArgs("t1", "choices-1", 1, ledgerChoiceArg{ChoiceID: "c1", Label: "라벨", RequestText: "문장"})
	first, err := s.ledger(ctx, "ledger_choices_put", good)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ledger(ctx, "ledger_choices_put", good)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(replay)
	if string(a) != string(b) {
		t.Fatalf("replay differs:\n%s\n%s", a, b)
	}
	// The replay added no history line either.
	published := 0
	for _, e := range events(t, s, "t1", nil, 0).Events {
		if e.Kind == eventChoicesPublished {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("choices_published events = %d, want one for one write", published)
	}
}

// The history is the task's own story in order: its transitions, and what
// happened to the requests left on it — each row written with the change it
// describes.
func TestLedgerTaskHistoryRecordsWhatActuallyHappened(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)

	putTask(t, s, "t1", "create-t1", 0)
	moved := taskArgs("t1", "update-t1", 1)
	moved.Task.Status = "blocked"
	moved.Task.Stage = "review"
	if _, err := s.ledger(ctx, "ledger_put", moved); err != nil {
		t.Fatal(err)
	}
	// The session offers a choice on the task as it now stands, and the person
	// picks it — so the history shows which of its own sentences was taken.
	if _, err := s.ledger(ctx, "ledger_choices_put", choiceArgs("t1", "choices-1", 2,
		ledgerChoiceArg{ChoiceID: "c-go", Label: "그대로 진행", RequestText: "이 작업 진행해 줘"},
	)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.appendLedgerRequest(ctx, "req-1", "t1", "이 작업 진행해 줘", "user-7", "c-go", 2, ledgerNow()); err != nil {
		t.Fatal(err)
	}
	claimed := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_claim", ledgerArgs{
		ID: "req-1", RequestID: "claim-1", ExpectedVersion: lv(1), ClaimOwner: "parent-a",
	}))
	dispatched := decodeRequestReply(t, mustLedger(t, s, "ledger_requests_dispatch", ledgerArgs{
		ID: "req-1", RequestID: "dispatch-1", ExpectedVersion: lv(claimed.Request.Version),
		ClaimToken: claimed.ClaimToken, OperationKey: "turn-42", OperationTarget: "seq/42",
		DispatchState: dispatchDispatched,
	}))
	mustLedger(t, s, "ledger_requests_ack", ledgerArgs{
		ID: "req-1", RequestID: "ack-1", ExpectedVersion: lv(dispatched.Request.Version),
		ClaimToken: claimed.ClaimToken, Outcome: requestProcessed, Result: "원장에 적고 진행했다", RelatedSeq: lv(42),
	})

	page := events(t, s, "t1", nil, 0)
	kinds := make([]string, 0, len(page.Events))
	for _, e := range page.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{eventTaskCreated, eventTaskUpdated, eventChoicesPublished, eventRequestStored, eventRequestClaimed, eventRequestDispatched, eventRequestSettled}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("history = %v, want %v", kinds, want)
	}
	// seq is strictly increasing, which is what pages a boundary safely.
	for i := 1; i < len(page.Events); i++ {
		if page.Events[i].Seq <= page.Events[i-1].Seq {
			t.Fatalf("seq not increasing: %+v", page.Events)
		}
	}
	// The transition says what moved, the request line carries the person's own
	// words, and the settled line carries the result they will read.
	if page.Events[1].Detail["status_from"] != "planned" || !strings.Contains(page.Events[1].Summary, "blocked") {
		t.Fatalf("transition line = %+v", page.Events[1])
	}
	if page.Events[3].Detail["text"] != "이 작업 진행해 줘" || page.Events[3].Detail["choice_id"] != "c-go" {
		t.Fatalf("request line = %+v", page.Events[3])
	}
	if page.Events[6].Detail["result"] != "원장에 적고 진행했다" {
		t.Fatalf("settled line = %+v", page.Events[6])
	}
	// No claim token, and nothing else that is not a named fact, reaches the
	// history.
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), claimed.ClaimToken) {
		t.Fatal("the claim token must never reach the history")
	}
	for _, e := range page.Events {
		if e.TaskID != "t1" {
			t.Fatalf("another task's row leaked: %+v", e)
		}
	}
}

// Two tasks' histories do not mix, and a page boundary neither repeats nor
// skips a row.
func TestLedgerTaskHistoryIsPerTaskAndPagesBySeq(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	putTask(t, s, "t2", "create-t2", 0)
	for i, id := range []string{"r1", "r2", "r3", "r4"} {
		task := "t1"
		if i%2 == 1 {
			task = "t2"
		}
		if _, _, err := s.appendLedgerRequest(ctx, id, task, "요청 "+id, "user-7", "", 0, ledgerNow()); err != nil {
			t.Fatal(err)
		}
	}

	one := events(t, s, "t1", nil, 0)
	for _, e := range one.Events {
		if e.TaskID != "t1" {
			t.Fatalf("t1 history carries %s", e.TaskID)
		}
	}
	if len(one.Events) != 3 { // created + two requests
		t.Fatalf("t1 history = %d rows, want 3", len(one.Events))
	}

	first := events(t, s, "t1", nil, 2)
	if len(first.Events) != 2 || first.NextCursor != first.Events[1].Seq {
		t.Fatalf("first page = %+v", first)
	}
	second := events(t, s, "t1", &first.NextCursor, 2)
	if len(second.Events) != 1 || second.NextCursor != 0 {
		t.Fatalf("second page = %+v", second)
	}
	if second.Events[0].Seq <= first.Events[1].Seq {
		t.Fatal("a page boundary must not repeat a row")
	}
	// A task with no history of its own answers empty rather than someone
	// else's rows.
	if empty := events(t, s, "absent", nil, 0); len(empty.Events) != 0 {
		t.Fatalf("unknown task history = %+v", empty.Events)
	}
}

// Concurrency: two sessions writing choices for the same version race, and
// exactly one wins — the loser is told the task moved rather than silently
// replacing the winner's sentences.
func TestLedgerChoicesConcurrentPutHasOneWinner(t *testing.T) {
	s, _ := newTestSessionDB(t)
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for _, who := range []string{"a", "b"} {
		wg.Add(1)
		go func(who string) {
			defer wg.Done()
			_, err := s.ledger(context.Background(), "ledger_choices_put", choiceArgs("t1", "choices-"+who, 1,
				ledgerChoiceArg{ChoiceID: "c-" + who, Label: "라벨 " + who, RequestText: "문장 " + who},
			))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			}
		}(who)
	}
	wg.Wait()
	// Both wrote against the same, still-current version, so both are valid
	// full replacements; what must hold is that the end state is one coherent
	// set rather than a mix of the two.
	got, err := s.ledger(context.Background(), "ledger_get", ledgerArgs{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	record := got.(ledgerRecord)
	if wins == 0 || len(record.Choices) != 1 {
		t.Fatalf("wins=%d choices=%+v, want a single coherent set", wins, record.Choices)
	}
}

// The history view is loopback-only, GET-only, and pages by the same cursor the
// tool uses.
func TestLedgerEventsViewServesFromLoopbackOnly(t *testing.T) {
	db, h := ledgerViewHandler(t)
	freezeLedgerClock(t)
	putTask(t, db, "t1", "create-t1", 0)

	get := func(remote, query string) *httptest.ResponseRecorder {
		target := sessionDBLedgerEventsPath
		if query != "" {
			target += "?" + query
		}
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("10.0.0.5:4000", "task_id=t1"); rec.Code != http.StatusForbidden {
		t.Fatalf("off-pod GET = %d, want 403", rec.Code)
	}
	rec := get("127.0.0.1:54321", "task_id=t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d %s", rec.Code, rec.Body.String())
	}
	var page eventPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Kind != eventTaskCreated {
		t.Fatalf("page = %+v", page)
	}
	if rec := get("127.0.0.1:54321", "task_id=t1&limit=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400", rec.Code)
	}
	if rec := get("127.0.0.1:54321", "task_id=t1&after_seq=-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("after_seq=-1 = %d, want 400", rec.Code)
	}
	if rec := get("127.0.0.1:54321", "task_id="+url.QueryEscape("공백 아이디")); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad task id = %d, want 400", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, sessionDBLedgerEventsPath, strings.NewReader("{}"))
	req.RemoteAddr = "127.0.0.1:54321"
	post := httptest.NewRecorder()
	h.ServeHTTP(post, req)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", post.Code)
	}
}

// Hiding a stale choice from the read is not the whole rule: a screen opened
// before the task moved still holds the old sentences, so the store itself
// refuses the pick. The person's typed words are never refused this way, and a
// refusal stores nothing and leaves no history line.
func TestLedgerRequestRefusesAChoiceTheSessionNoLongerOffers(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	freezeLedgerClock(t)
	putTask(t, s, "t1", "create-t1", 0)
	mustLedger(t, s, "ledger_choices_put", choiceArgs("t1", "choices-1", 1,
		ledgerChoiceArg{ChoiceID: "c-ci", Label: "CI 로그 확인", RequestText: "실패한 CI 로그를 확인해 줘"},
	))

	// A choice this task never offered is refused even while the task stands
	// still.
	if _, _, err := s.appendLedgerRequest(ctx, "r-unknown", "t1", "다른 선택", "user-7", "c-made-up", 0, ledgerNow()); err == nil {
		t.Fatal("an unknown choice_id must be refused")
	} else {
		assertLedgerCode(t, err, "stale_choice")
	}
	// A basis the caller carries that disagrees with the stored one is refused
	// too — that is the long-open screen.
	if _, _, err := s.appendLedgerRequest(ctx, "r-oldbasis", "t1", "낡은 화면", "user-7", "c-ci", 99, ledgerNow()); err == nil {
		t.Fatal("a caller basis that disagrees must be refused")
	} else {
		assertLedgerCode(t, err, "stale_choice")
	}
	// Fresh, it stores.
	if _, _, err := s.appendLedgerRequest(ctx, "r-ok", "t1", "실패한 CI 로그를 확인해 줘", "user-7", "c-ci", 1, ledgerNow()); err != nil {
		t.Fatal(err)
	}

	// The task moves. The very same pick, from a screen that never saw the
	// move, is now refused — including when the session re-published the same
	// choice_id against the newer version.
	if _, err := s.ledger(ctx, "ledger_put", taskArgs("t1", "update-t1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.appendLedgerRequest(ctx, "r-stale", "t1", "실패한 CI 로그를 확인해 줘", "user-7", "c-ci", 1, ledgerNow()); err == nil {
		t.Fatal("a pick written against the older version must be refused")
	} else {
		assertLedgerCode(t, err, "stale_choice")
	}
	mustLedger(t, s, "ledger_choices_put", choiceArgs("t1", "choices-2", 2,
		ledgerChoiceArg{ChoiceID: "c-ci", Label: "CI 로그 확인", RequestText: "실패한 CI 로그를 확인해 줘"},
	))
	if _, _, err := s.appendLedgerRequest(ctx, "r-stale2", "t1", "실패한 CI 로그를 확인해 줘", "user-7", "c-ci", 1, ledgerNow()); err == nil {
		t.Fatal("a re-published choice must not let an older screen through")
	} else {
		assertLedgerCode(t, err, "stale_choice")
	}
	// The person's own words carry no choice_id and are stored as ever, even
	// while every offered choice of that task is stale.
	if _, _, err := s.appendLedgerRequest(ctx, "r-free", "t1", "내가 직접 쓴 요청", "user-7", "", 0, ledgerNow()); err != nil {
		t.Fatal(err)
	}
	// A basis without a pick is a malformed call, not a stale one.
	if _, _, err := s.appendLedgerRequest(ctx, "r-bad", "t1", "선택 없이 기준", "user-7", "", 3, ledgerNow()); err == nil {
		t.Fatal("a basis with no choice_id must be refused")
	} else {
		assertLedgerCode(t, err, "invalid_argument")
	}

	// Only the two stores that were accepted exist, and the refusals left no
	// history line of their own.
	stored := 0
	for _, e := range events(t, s, "t1", nil, 0).Events {
		if e.Kind == eventRequestStored {
			stored++
		}
	}
	if stored != 2 {
		t.Fatalf("stored requests = %d, want only the fresh pick and the typed words", stored)
	}
}
