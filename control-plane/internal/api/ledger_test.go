package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"k8s.io/client-go/kubernetes/fake"
)

// ledgerServer is the API with a stub agent holding a slot's task ledger.
func ledgerServer(t *testing.T) (*httptest.Server, *agent.StubClient) {
	t.Helper()
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := archive.NewStubCheckpointer(true)
	stub := agent.NewStubClient()
	mgr := service.New(orch, configmap.NewStore(fake.NewSimpleClientset(), "sessions"), ckpt, stub,
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexChat, ckpt))
	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, stub
}

func ledgerRow(id, status, stage, reason, next string) session.LedgerTaskRecord {
	return session.LedgerTaskRecord{
		ID:      id,
		Version: 1,
		Task: session.LedgerTask{
			Title:  "task " + id,
			Status: status,
			Stage:  stage,
			Owner:  "parent",
			Sessions: []session.LedgerTaskSession{
				{ID: "first", Ownership: "owned", Purpose: "earlier work", Cleanup: "deleted"},
				{ID: "now", Ownership: "owned", Purpose: "current work", Cleanup: "retained", Reason: reason, NextAction: next},
			},
			Details: map[string]any{"kind": "work"},
		},
	}
}

func getLedger(t *testing.T, srv *httptest.Server, id, query string) (int, session.LedgerTaskPage) {
	t.Helper()
	target := srv.URL + "/api/v1/sessions/" + id + "/ledger/tasks"
	if query != "" {
		target += "?" + query
	}
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("get ledger: %v", err)
	}
	defer resp.Body.Close()
	var page session.LedgerTaskPage
	_ = json.NewDecoder(resp.Body).Decode(&page)
	return resp.StatusCode, page
}

// AC-N6 read side: the route pages the chat slot's ledger, carries the cursor
// forward, and ends with an empty cursor — so a reader can tell a finished walk
// from a truncated one. Status, stage, reason and next action all reach it.
func TestLedgerTasks_PagesTheSlotLedger(t *testing.T) {
	srv, stub := ledgerServer(t)
	stub.LedgerRows = []session.LedgerTaskRecord{
		ledgerRow("t1", "running", "implementation", "", ""),
		ledgerRow("t2", "blocked", "review", "CI 가 막혔다", "로그를 본다"),
		ledgerRow("t3", "review", "verification", "", ""),
	}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "ledger", "workloadType": "codex-chat"})

	status, first := getLedger(t, srv, s.ID, "limit=2")
	if status != http.StatusOK || len(first.Tasks) != 2 || first.NextCursor != "t2" {
		t.Fatalf("first page = %d %+v", status, first)
	}
	if first.Tasks[1].Task.Status != "blocked" || first.Tasks[1].Task.Stage != "review" {
		t.Fatalf("status/stage lost: %+v", first.Tasks[1].Task)
	}
	// The reason a task is stuck and what comes next are the point of the
	// blocked row, so they must survive the whole path.
	last := first.Tasks[1].Task.Sessions[len(first.Tasks[1].Task.Sessions)-1]
	if last.Reason != "CI 가 막혔다" || last.NextAction != "로그를 본다" {
		t.Fatalf("reason/next action lost: %+v", last)
	}

	status, second := getLedger(t, srv, s.ID, "limit=2&after_id="+first.NextCursor)
	if status != http.StatusOK || len(second.Tasks) != 1 || second.Tasks[0].ID != "t3" {
		t.Fatalf("second page = %d %+v", status, second)
	}
	if second.NextCursor != "" {
		t.Fatalf("a finished walk must end with an empty cursor, got %q", second.NextCursor)
	}
}

// An empty ledger is an empty page: the session has no tasks yet.
func TestLedgerTasks_EmptyLedgerIsAnEmptyPage(t *testing.T) {
	srv, _ := ledgerServer(t)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "empty", "workloadType": "codex-chat"})
	status, page := getLedger(t, srv, s.ID, "")
	if status != http.StatusOK || len(page.Tasks) != 0 || page.NextCursor != "" {
		t.Fatalf("empty = %d %+v", status, page)
	}
}

// A page size outside the ledger's own range is refused rather than clamped.
func TestLedgerTasks_RefusesAnInvalidPageSize(t *testing.T) {
	srv, _ := ledgerServer(t)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "limits", "workloadType": "codex-chat"})
	for _, query := range []string{"limit=0", "limit=101", "limit=-5", "limit=lots"} {
		if status, _ := getLedger(t, srv, s.ID, query); status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, status)
		}
	}
}

// A type that keeps no session database says so (501) rather than answering an
// empty ledger, which would claim the session has no work.
func TestLedgerTasks_UnsupportedWorkloadIsNotAnEmptyLedger(t *testing.T) {
	srv, _ := ledgerServer(t)
	_, shell := createRaw(t, srv.URL, map[string]any{"name": "sh"})
	if status, _ := getLedger(t, srv, shell.ID, ""); status != http.StatusNotImplemented {
		t.Fatalf("shell session = %d, want 501", status)
	}
}

// A snapshot session keeps its ledger on the slot's volume but has no pod to
// read it through. That is a distinct answer (409) — and reading must not
// restore the snapshot, or opening a panel would wake a session.
func TestLedgerTasks_SnapshotIsUnavailableAndIsNotWoken(t *testing.T) {
	srv, stub := ledgerServer(t)
	stub.LedgerRows = []session.LedgerTaskRecord{ledgerRow("t1", "running", "implementation", "", "")}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "snap", "workloadType": "codex-chat"})

	resp, err := http.Post(srv.URL+"/api/v1/sessions/"+s.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot = %d", resp.StatusCode)
	}

	before := stub.LedgerReads
	status, page := getLedger(t, srv, s.ID, "")
	if status != http.StatusConflict {
		t.Fatalf("snapshot read = %d %+v, want 409", status, page)
	}
	if stub.LedgerReads != before {
		t.Fatalf("a snapshot read reached the pod %d times, want 0", stub.LedgerReads-before)
	}

	// The session is still a snapshot: the read neither restored it nor
	// counted as an access.
	resp, err = http.Get(srv.URL + "/api/v1/sessions/" + s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var after session.Session
	_ = json.NewDecoder(resp.Body).Decode(&after)
	if after.State != session.StateSnapshot {
		t.Fatalf("reading the ledger moved the session to %q; it must stay a snapshot", after.State)
	}
}

// AC-K2: the ledger route is scoped like every other session-id route. Another
// tenant's session answers exactly like a missing one, so one user's chat can
// never read another's task panel.
func TestLedgerTasks_IsScopedToTheOwner(t *testing.T) {
	srv := newTenantServer(t)
	resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "alpha",
		map[string]string{"name": "mine", "workloadType": "codex-chat"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var mine session.Session
	if err := json.Unmarshal(body, &mine); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/sessions/" + mine.ID + "/ledger/tasks"
	for _, tc := range []struct {
		name, tenant string
		want         int
	}{
		{"the owner reads its own ledger", "alpha", http.StatusOK},
		{"another tenant sees a missing session", "beta", http.StatusNotFound},
		{"an unknown caller is refused", "nobody", http.StatusForbidden},
	} {
		resp, body := tenantCall(t, srv, http.MethodGet, path, tc.tenant, nil)
		if resp.StatusCode != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
		}
	}
}

// The route is read-only: nothing but GET is mounted on it.
func TestLedgerTasks_IsGETOnly(t *testing.T) {
	srv, _ := ledgerServer(t)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "ro", "workloadType": "codex-chat"})
	path := srv.URL + "/api/v1/sessions/" + s.ID + "/ledger/tasks"
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s reached the ledger route; it must be GET only", method)
		}
	}
}

// postLedgerRequest stores one user request the way an app's backend does.
func postLedgerRequest(t *testing.T, srv *httptest.Server, sessionID, taskID string, body map[string]any) (int, session.LedgerRequestStored) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	target := srv.URL + "/api/v1/sessions/" + sessionID + "/ledger/tasks/" + taskID + "/requests"
	resp, err := http.Post(target, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("store request: %v", err)
	}
	defer resp.Body.Close()
	var stored session.LedgerRequestStored
	_ = json.NewDecoder(resp.Body).Decode(&stored)
	return resp.StatusCode, stored
}

// AC-N6 write side: a request lands on the slot as pending, and the answer
// says that and nothing more — stored and waiting, never dealt with. The task
// comes from the path, so the body cannot aim it at another task.
func TestLedgerRequests_StoresOnePendingRequest(t *testing.T) {
	srv, stub := ledgerServer(t)
	stub.LedgerRows = []session.LedgerTaskRecord{ledgerRow("t1", "blocked", "review", "CI 가 막혔다", "로그를 본다")}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "requests", "workloadType": "codex-chat"})

	status, stored := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-1", "text": "이 작업 진행해 줘", "author": "user-7",
	})
	if status != http.StatusCreated || stored.Replayed {
		t.Fatalf("store = %d %+v, want 201 and a first store", status, stored)
	}
	if stored.Request.TaskID != "t1" || stored.Request.Status != session.LedgerRequestPending ||
		stored.Request.Text != "이 작업 진행해 줘" || stored.Request.Author != "user-7" {
		t.Fatalf("stored = %+v, want a pending request on t1", stored.Request)
	}
	// Nothing was read or run to store it: the session was not woken.
	if stub.LedgerReads != 0 {
		t.Fatalf("ledger reads = %d, want storing a request to read nothing", stub.LedgerReads)
	}

	// The same id with the same words is the caller's retry, not a second
	// request.
	status, replay := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-1", "text": "이 작업 진행해 줘", "author": "user-7",
	})
	if status != http.StatusOK || !replay.Replayed || replay.Request.Seq != stored.Request.Seq {
		t.Fatalf("replay = %d %+v, want 200 and the stored row", status, replay)
	}
	if len(stub.LedgerRequests) != 1 {
		t.Fatalf("stored rows = %d, want the retry to have stored nothing twice", len(stub.LedgerRequests))
	}

	// The same id with different words is a mistake, not a second request.
	if status, _ := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-1", "text": "다른 말", "author": "user-7",
	}); status != http.StatusConflict {
		t.Fatalf("reused id = %d, want 409", status)
	}
}

// A request about a task this ledger does not have is not stored: nobody would
// ever find it. A session that keeps no ledger, and one with no live pod to
// write through, each say so in their own status.
func TestLedgerRequests_RefusesWhatCannotBeStored(t *testing.T) {
	srv, stub := ledgerServer(t)
	stub.LedgerRows = []session.LedgerTaskRecord{ledgerRow("t1", "running", "implementation", "", "")}
	_, chat := createRaw(t, srv.URL, map[string]any{"name": "requests", "workloadType": "codex-chat"})

	if status, _ := postLedgerRequest(t, srv, chat.ID, "absent", map[string]any{
		"id": "req-1", "text": "요청", "author": "user-7",
	}); status != http.StatusNotFound {
		t.Fatalf("unknown task = %d, want 404", status)
	}

	// Another tenant's session answers exactly like a missing one.
	if status, _ := postLedgerRequest(t, srv, "sess-nope", "t1", map[string]any{
		"id": "req-2", "text": "요청", "author": "user-7",
	}); status != http.StatusNotFound {
		t.Fatalf("unknown session = %d, want 404", status)
	}

	// A type that keeps no ledger has nowhere to put it.
	_, shell := createRaw(t, srv.URL, map[string]any{"name": "shell", "workloadType": "shell"})
	if status, _ := postLedgerRequest(t, srv, shell.ID, "t1", map[string]any{
		"id": "req-3", "text": "요청", "author": "user-7",
	}); status != http.StatusNotImplemented {
		t.Fatalf("shell session = %d, want 501", status)
	}
}

// The fields are bounded before the hop into the pod, and the message is about
// the field rather than the hop.
func TestLedgerRequests_RefusesMalformedFields(t *testing.T) {
	srv, stub := ledgerServer(t)
	stub.LedgerRows = []session.LedgerTaskRecord{ledgerRow("t1", "running", "implementation", "", "")}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "requests", "workloadType": "codex-chat"})
	for _, body := range []map[string]any{
		{"text": "요청", "author": "user-7"},
		{"id": "한글 아이디", "text": "요청", "author": "user-7"},
		{"id": "req-1", "text": "   ", "author": "user-7"},
		{"id": "req-1", "text": "요청", "author": ""},
		{"id": "req-1", "text": strings.Repeat("가", session.LedgerRequestTextMax+1), "author": "user-7"},
	} {
		if status, _ := postLedgerRequest(t, srv, s.ID, "t1", body); status != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, status)
		}
	}
	if len(stub.LedgerRequests) != 0 {
		t.Fatalf("stored = %d, want nothing stored for a malformed request", len(stub.LedgerRequests))
	}
}

// AC-N8: the history route pages one task's story by seq, carries the cursor
// forward, and is scoped like every other session-id route.
func TestLedgerEvents_PagesOneTasksHistory(t *testing.T) {
	srv, stub := ledgerServer(t)
	stub.LedgerRows = []session.LedgerTaskRecord{ledgerRow("t1", "blocked", "review", "CI 가 막혔다", "로그를 본다")}
	stub.LedgerEventRows = map[string][]session.LedgerTaskEvent{
		"t1": {
			{Seq: 1, TaskID: "t1", At: "2026-11-19T00:00:00Z", Kind: "task_created", Actor: "parent", Summary: "작업을 원장에 등록했다"},
			{Seq: 2, TaskID: "t1", At: "2026-11-19T00:01:00Z", Kind: "request_stored", Actor: "user-1", Summary: "요청을 남겼다",
				Detail: map[string]any{"text": "이 작업 진행해 줘"}, RequestID: "req-1"},
			{Seq: 3, TaskID: "t1", At: "2026-11-19T00:02:00Z", Kind: "request_settled", Actor: "parent", Summary: "요청을 processed 로 닫았다"},
		},
		"t2": {{Seq: 9, TaskID: "t2", Kind: "task_created", Actor: "parent", Summary: "다른 작업"}},
	}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "history", "workloadType": "codex-chat"})

	get := func(query string) (int, session.LedgerTaskEventPage) {
		target := srv.URL + "/api/v1/sessions/" + s.ID + "/ledger/tasks/t1/events"
		if query != "" {
			target += "?" + query
		}
		resp, err := http.Get(target)
		if err != nil {
			t.Fatalf("get history: %v", err)
		}
		defer resp.Body.Close()
		var page session.LedgerTaskEventPage
		_ = json.NewDecoder(resp.Body).Decode(&page)
		return resp.StatusCode, page
	}

	status, first := get("limit=2")
	if status != http.StatusOK || len(first.Events) != 2 || first.NextCursor != 2 {
		t.Fatalf("first page = %d %+v", status, first)
	}
	if first.Events[1].Detail["text"] != "이 작업 진행해 줘" || first.Events[1].RequestID != "req-1" {
		t.Fatalf("the person's own words must survive the path: %+v", first.Events[1])
	}
	status, second := get("limit=2&after_seq=2")
	if status != http.StatusOK || len(second.Events) != 1 || second.Events[0].Seq != 3 || second.NextCursor != 0 {
		t.Fatalf("second page = %d %+v", status, second)
	}
	// Another task's rows never appear under this one.
	for _, e := range append(first.Events, second.Events...) {
		if e.TaskID != "t1" {
			t.Fatalf("leaked %+v", e)
		}
	}
	for _, bad := range []string{"limit=0", "limit=101", "after_seq=-1", "after_seq=lots"} {
		if status, _ := get(bad); status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, status)
		}
	}
	// A task with no history is an empty page, not someone else's rows.
	resp, err := http.Get(srv.URL + "/api/v1/sessions/" + s.ID + "/ledger/tasks/absent/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var empty session.LedgerTaskEventPage
	_ = json.NewDecoder(resp.Body).Decode(&empty)
	if resp.StatusCode != http.StatusOK || len(empty.Events) != 0 {
		t.Fatalf("unknown task = %d %+v", resp.StatusCode, empty)
	}
	// Another tenant's session answers exactly like a missing one.
	other, err := http.Get(srv.URL + "/api/v1/sessions/sess-nope/ledger/tasks/t1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session = %d, want 404", other.StatusCode)
	}
}

// The session's offered choices reach the app through the task page, with the
// version they were written against.
func TestLedgerTasks_CarryTheSessionsOfferedChoices(t *testing.T) {
	srv, stub := ledgerServer(t)
	row := ledgerRow("t1", "blocked", "review", "CI 가 막혔다", "로그를 본다")
	row.Choices = []session.LedgerChoice{
		{ID: "c-ci-log", Label: "CI 로그 확인 요청", RequestText: "방금 실패한 CI 로그에서 어디서 멈췄는지 알려 줘.", BasisVersion: 1, CreatedAt: "2026-11-19T00:00:00Z"},
	}
	stub.LedgerRows = []session.LedgerTaskRecord{row}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "choices", "workloadType": "codex-chat"})

	status, page := getLedger(t, srv, s.ID, "")
	if status != http.StatusOK || len(page.Tasks) != 1 {
		t.Fatalf("page = %d %+v", status, page)
	}
	got := page.Tasks[0].Choices
	if len(got) != 1 || got[0].ID != "c-ci-log" || got[0].RequestText == "" || got[0].BasisVersion != 1 {
		t.Fatalf("choices = %+v, want the session's own sentence with its basis version", got)
	}
}

// A pick is checked when it is stored, not only when it is shown: once the task
// moves, the same choice from a screen that never saw the move is refused, and
// the person's own words still store.
func TestLedgerRequests_RefusesAPickTheSessionNoLongerOffers(t *testing.T) {
	srv, stub := ledgerServer(t)
	row := ledgerRow("t1", "blocked", "review", "CI 가 막혔다", "로그를 본다")
	row.Choices = []session.LedgerChoice{
		{ID: "c-ci-log", Label: "CI 로그 확인 요청", RequestText: "실패한 CI 로그를 확인해 줘", BasisVersion: 1, CreatedAt: "2026-11-19T00:00:00Z"},
	}
	stub.LedgerRows = []session.LedgerTaskRecord{row}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "requests", "workloadType": "codex-chat"})

	if status, _ := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-1", "text": "실패한 CI 로그를 확인해 줘", "author": "user-7",
		"choice_id": "c-ci-log", "choice_basis_version": 1,
	}); status != http.StatusCreated {
		t.Fatalf("fresh pick = %d, want 201", status)
	}

	// The task moves; its choices were written about the state before.
	moved := stub.LedgerRows[0]
	moved.Version = 2
	stub.LedgerRows = []session.LedgerTaskRecord{moved}
	if status, _ := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-2", "text": "실패한 CI 로그를 확인해 줘", "author": "user-7",
		"choice_id": "c-ci-log", "choice_basis_version": 1,
	}); status != http.StatusConflict {
		t.Fatalf("stale pick = %d, want 409", status)
	}
	// A choice the task never offered is the same answer.
	if status, _ := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-3", "text": "없는 선택", "author": "user-7", "choice_id": "c-made-up",
	}); status != http.StatusConflict {
		t.Fatalf("unknown pick = %d, want 409", status)
	}
	// A basis with no pick is a malformed body, and never reaches the slot.
	if status, _ := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-4", "text": "선택 없이 기준", "author": "user-7", "choice_basis_version": 2,
	}); status != http.StatusBadRequest {
		t.Fatalf("basis without a pick = %d, want 400", status)
	}
	// Free text is unaffected by any of it.
	if status, _ := postLedgerRequest(t, srv, s.ID, "t1", map[string]any{
		"id": "req-5", "text": "내가 직접 쓴 요청", "author": "user-7",
	}); status != http.StatusCreated {
		t.Fatalf("free text = %d, want 201", status)
	}
	if len(stub.LedgerRequests) != 2 {
		t.Fatalf("stored = %d, want only the fresh pick and the typed words", len(stub.LedgerRequests))
	}
}
