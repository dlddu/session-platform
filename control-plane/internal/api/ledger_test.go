package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
