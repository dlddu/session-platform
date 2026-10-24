package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/repoaccess"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// helpers maps bearer tokens to the session whose helper holds them.
type helpers map[string]string

func (h helpers) SessionID(r *http.Request) (string, error) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if id, ok := h[tok]; ok {
		return id, nil
	}
	return "", helperauth.ErrUnauthenticated
}

type repoAccessFixture struct {
	srv     *httptest.Server
	mgr     session.Manager
	helpers helpers
}

func newRepoAccessServer(t *testing.T, enabled bool) repoAccessFixture {
	t.Helper()
	mgr := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(true),
		agent.NewStubClient(),
	)
	var store api.RepoAccessStore
	if enabled {
		s, err := repoaccess.Open(context.Background(), filepath.Join(t.TempDir(), "repo-access.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		store = s
	}
	h := helpers{"tok-a": "sess-a"}
	mux := http.NewServeMux()
	api.NewRepoAccess(store, h, mgr, nil).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return repoAccessFixture{srv: srv, mgr: mgr, helpers: h}
}

type call struct {
	method, path, body string
	bearer             string
}

func (f repoAccessFixture) do(t *testing.T, c call) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(c.method, f.srv.URL+c.path, strings.NewReader(c.body))
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("%s %s: decode: %v", c.method, c.path, err)
	}
	return res.StatusCode, out
}

func TestRepoAccessDisabledAnswers503(t *testing.T) {
	f := newRepoAccessServer(t, false)
	for _, c := range []call{
		{method: "GET", path: "/api/v1/repo-access"},
		{method: "POST", path: "/api/v1/repo-access/allowlist", body: `{"repo":"a/b"}`},
		{method: "GET", path: "/api/v1/internal/repo-access/check?repo=a/b", bearer: "tok-a"},
		{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"a/b","reason":"x"}`, bearer: "tok-a"},
	} {
		if code, _ := f.do(t, c); code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", c.method, c.path, code)
		}
	}
}

// The management routes have no sign-in of their own (AC-H2): the ingress's
// forward-auth is the door.
func TestRepoAccessAdminRoutesHaveNoSignIn(t *testing.T) {
	f := newRepoAccessServer(t, true)
	if code, _ := f.do(t, call{method: "GET", path: "/api/v1/repo-access"}); code != http.StatusOK {
		t.Fatalf("overview = %d, want 200", code)
	}
	code, e := f.do(t, call{method: "POST", path: "/api/v1/repo-access/allowlist", body: `{"repo":"a/b"}`})
	if code != http.StatusCreated {
		t.Fatalf("add = %d %v", code, e)
	}
	if _, has := e["addedBy"]; has {
		t.Fatalf("entry still records who added it: %v", e)
	}
}

func TestRepoAccessInternalRoutesNeedHelperToken(t *testing.T) {
	f := newRepoAccessServer(t, true)
	for _, c := range []call{
		{method: "GET", path: "/api/v1/internal/repo-access/check?repo=a/b"},
		{method: "GET", path: "/api/v1/internal/repo-access/check?repo=a/b", bearer: "forged"},
		// Filing a request needs a helper token too.
		{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"a/b","reason":"x"}`},
	} {
		if code, _ := f.do(t, c); code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, code)
		}
	}
}

func TestRepoAccessManualAllowList(t *testing.T) {
	f := newRepoAccessServer(t, true)
	code, e := f.do(t, call{method: "POST", path: "/api/v1/repo-access/allowlist", body: `{"repo":"DLDDU/Infra.git"}`})
	if code != http.StatusCreated || e["repo"] != "dlddu/infra" || e["source"] != "manual" {
		t.Fatalf("add = %d %v", code, e)
	}
	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/repo-access/allowlist", body: `{"repo":"dlddu/infra"}`}); code != http.StatusOK {
		t.Fatalf("re-add = %d, want 200", code)
	}
	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/repo-access/allowlist", body: `{"repo":"not a repo"}`}); code != http.StatusBadRequest {
		t.Fatalf("bad repo = %d, want 400", code)
	}
	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/repo-access/allowlist", body: `{"repo":"a/b","extra":1}`}); code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", code)
	}
	_, check := f.do(t, call{method: "GET", path: "/api/v1/internal/repo-access/check?repo=dlddu/INFRA", bearer: "tok-a"})
	if check["allowed"] != true || check["repo"] != "dlddu/infra" {
		t.Fatalf("check = %v", check)
	}
	if code, _ := f.do(t, call{method: "DELETE", path: "/api/v1/repo-access/allowlist?repo=dlddu/infra"}); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	_, check = f.do(t, call{method: "GET", path: "/api/v1/internal/repo-access/check?repo=dlddu/infra", bearer: "tok-a"})
	if check["allowed"] != false {
		t.Fatalf("check after delete = %v", check)
	}
	if code, _ := f.do(t, call{method: "GET", path: "/api/v1/internal/repo-access/check?repo=..", bearer: "tok-a"}); code != http.StatusBadRequest {
		t.Fatalf("bad check repo = %d, want 400", code)
	}
}

func TestRepoAccessRequestApproveFlow(t *testing.T) {
	f := newRepoAccessServer(t, true)
	sess, err := f.mgr.Create(context.Background(), session.CreateRequest{Name: "infra-fix"})
	if err != nil {
		t.Fatal(err)
	}
	f.helpers["tok-a"] = sess.ID

	code, rq := f.do(t, call{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"dlddu/infra","reason":"fix the ingress"}`, bearer: "tok-a"})
	if code != http.StatusOK || rq["status"] != "pending" || !strings.HasPrefix(rq["requestId"].(string), "rq_") {
		t.Fatalf("request = %d %v", code, rq)
	}
	_, again := f.do(t, call{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"dlddu/infra","reason":"again"}`, bearer: "tok-a"})
	if again["requestId"] != rq["requestId"] {
		t.Fatalf("duplicate request got a new id: %v vs %v", again, rq)
	}
	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"dlddu/infra","reason":""}`, bearer: "tok-a"}); code != http.StatusBadRequest {
		t.Fatalf("empty reason = %d, want 400", code)
	}

	_, ov := f.do(t, call{method: "GET", path: "/api/v1/repo-access"})
	reqs := ov["requests"].([]any)
	if len(reqs) != 1 || len(ov["allowlist"].([]any)) != 0 {
		t.Fatalf("overview = %v", ov)
	}
	r0 := reqs[0].(map[string]any)
	if r0["sessionId"] != sess.ID || r0["sessionName"] != "infra-fix" || r0["workloadType"] != string(sess.WorkloadType) || r0["reason"] != "fix the ingress" || r0["alreadyAllowed"] != false {
		t.Fatalf("request view = %v", r0)
	}

	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/repo-access/requests/rq_missing/approve"}); code != http.StatusNotFound {
		t.Fatalf("approve missing = %d, want 404", code)
	}
	code, e := f.do(t, call{method: "POST", path: "/api/v1/repo-access/requests/" + rq["requestId"].(string) + "/approve"})
	if code != http.StatusOK || e["source"] != "request" || e["requestSessionId"] != sess.ID {
		t.Fatalf("approve = %d %v", code, e)
	}
	_, after := f.do(t, call{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"dlddu/infra","reason":"x"}`, bearer: "tok-a"})
	if after["status"] != "allowed" {
		t.Fatalf("request after approval = %v", after)
	}
	if _, has := after["requestId"]; has {
		t.Fatalf("allowed answer carries a request id: %v", after)
	}
}

func TestRepoAccessReject(t *testing.T) {
	f := newRepoAccessServer(t, true)
	_, rq := f.do(t, call{method: "POST", path: "/api/v1/internal/repo-access/requests", body: `{"repo":"a/b","reason":"x"}`, bearer: "tok-a"})
	id := rq["requestId"].(string)
	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/repo-access/requests/" + id + "/reject"}); code != http.StatusNoContent {
		t.Fatalf("reject = %d", code)
	}
	if code, _ := f.do(t, call{method: "POST", path: "/api/v1/repo-access/requests/" + id + "/reject"}); code != http.StatusNotFound {
		t.Fatalf("reject twice = %d, want 404", code)
	}
	_, ov := f.do(t, call{method: "GET", path: "/api/v1/repo-access"})
	if len(ov["requests"].([]any)) != 0 || len(ov["allowlist"].([]any)) != 0 {
		t.Fatalf("overview after reject = %v", ov)
	}
}
