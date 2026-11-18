package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ledgerViewHandler is the sidecar as the pod sees it: the ledger view mounted
// in front of the K3s MCP proxy, so the test also covers the routing.
func ledgerViewHandler(t *testing.T) (*sessionDB, http.Handler) {
	t.Helper()
	db, _ := newTestSessionDB(t)
	proxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // "this did not reach the ledger view"
	})
	return db, withSessionDBMCP(proxy, db)
}

// ledgerViewGet calls the view the way the agent does — from loopback.
func ledgerViewGet(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := sessionDBLedgerPath
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type ledgerViewPage struct {
	Tasks []struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Task    struct {
			Title    string `json:"title"`
			Status   string `json:"status"`
			Stage    string `json:"stage"`
			Sessions []struct {
				Reason     string `json:"reason"`
				NextAction string `json:"next_action"`
			} `json:"sessions"`
			Details map[string]any `json:"details"`
		} `json:"task"`
	} `json:"tasks"`
	NextCursor string `json:"next_cursor"`
}

func decodeLedgerView(t *testing.T, rec *httptest.ResponseRecorder) ledgerViewPage {
	t.Helper()
	var page ledgerViewPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return page
}

// AC-N6 read side: the view pages the same rows the session's own ledger_list
// returns, carries the cursor forward, and ends with an empty cursor — so a
// reader can tell a last page from a truncated one.
func TestLedgerViewPagesTheSameLedger(t *testing.T) {
	db, h := ledgerViewHandler(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.ledger(ctx, "ledger_put", taskArgs(id, id, 0)); err != nil {
			t.Fatal(err)
		}
	}

	rec := ledgerViewGet(t, h, "limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("first page = %d %s", rec.Code, rec.Body)
	}
	first := decodeLedgerView(t, rec)
	if len(first.Tasks) != 2 || first.NextCursor != "b" {
		t.Fatalf("first page = %+v", first)
	}
	if first.Tasks[0].ID != "a" || first.Tasks[0].Task.Status != "planned" {
		t.Fatalf("first row = %+v", first.Tasks[0])
	}
	// The task's own bookkeeping reaches the reader: the panel reads a kind out
	// of details, which the ledger schema has no column for.
	if first.Tasks[0].Task.Details["approval"] != "PR only" {
		t.Fatalf("details = %+v", first.Tasks[0].Task.Details)
	}

	rec = ledgerViewGet(t, h, "limit=2&after_id="+url.QueryEscape(first.NextCursor))
	last := decodeLedgerView(t, rec)
	if rec.Code != http.StatusOK || len(last.Tasks) != 1 || last.Tasks[0].ID != "c" {
		t.Fatalf("last page = %d %+v", rec.Code, last)
	}
	if last.NextCursor != "" {
		t.Fatalf("a last page must end the walk, got cursor %q", last.NextCursor)
	}
}

// An empty ledger is an empty page, not an error: the session simply has no
// tasks yet.
func TestLedgerViewEmptyLedgerIsAnEmptyPage(t *testing.T) {
	_, h := ledgerViewHandler(t)
	rec := ledgerViewGet(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty = %d %s", rec.Code, rec.Body)
	}
	if page := decodeLedgerView(t, rec); len(page.Tasks) != 0 || page.NextCursor != "" {
		t.Fatalf("empty page = %+v", page)
	}
}

// The view is read-only and ledger-only: no method but GET, and no input but a
// page size and a cursor. In particular it takes no SQL, so it cannot be used
// to read the rest of the session database.
func TestLedgerViewIsReadOnlyAndTakesNoSQL(t *testing.T) {
	db, h := ledgerViewHandler(t)
	ctx := context.Background()
	if _, err := db.exec(ctx, sessionDBStatement{SQL: "CREATE TABLE secrets(token)"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.exec(ctx, sessionDBStatement{SQL: "INSERT INTO secrets VALUES('shhh')"}); err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, sessionDBLedgerPath, strings.NewReader(`{"sql":"SELECT * FROM secrets"}`))
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}

	// A SQL-shaped query parameter is simply not a parameter the view reads;
	// the answer is an ordinary page and never the other table's rows.
	rec := ledgerViewGet(t, h, "sql="+url.QueryEscape("SELECT * FROM secrets"))
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown parameter = %d %s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); strings.Contains(body, "shhh") || strings.Contains(body, "secrets") {
		t.Fatalf("the ledger view answered with something outside the ledger: %s", body)
	}
}

// A page size the ledger does not allow is refused rather than clamped, so a
// caller never believes it read a full page when it read a smaller one.
func TestLedgerViewRefusesAnInvalidPageRequest(t *testing.T) {
	_, h := ledgerViewHandler(t)
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=many", "after_id=../escape"} {
		rec := ledgerViewGet(t, h, query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", query, rec.Code, rec.Body)
			continue
		}
		var out map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["code"] != "invalid_argument" {
			t.Errorf("%s body = %s (%v)", query, rec.Body, err)
		}
	}
}

// Only this pod reads the slot's ledger. The sidecar binds loopback, so this
// guards a future wider bind rather than today's deployment.
func TestLedgerViewRefusesANonLoopbackReader(t *testing.T) {
	_, h := ledgerViewHandler(t)
	req := httptest.NewRequest(http.MethodGet, sessionDBLedgerPath, nil)
	req.RemoteAddr = "10.42.0.9:33333"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-pod read = %d %s, want 403", rec.Code, rec.Body)
	}
}

// Mounting the view must not shadow the K3s MCP proxy behind it.
func TestLedgerViewLeavesTheProxyUntouched(t *testing.T) {
	_, h := ledgerViewHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/anything/else", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("proxy passthrough = %d, want the proxy's own answer", rec.Code)
	}
}
