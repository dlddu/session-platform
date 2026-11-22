package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/monitorpair"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeIdentity stands in for the TokenReview verifier: the test sets which
// session the next request is proved to come from. The point under test is
// that the handlers use *this* and never a body or query field, so a client
// cannot name a monitor itself.
type fakeIdentity struct {
	id  string
	err error
}

func (f *fakeIdentity) SessionID(*http.Request) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

// monitorServer is the isolated fixture: stub orchestrator, stub agent, a
// ConfigMap state store on a fake clientset and a pair store in a temp dir.
// Nothing here touches a real cluster, a real slot or a deployed schedule.
func monitorServer(t *testing.T) (*httptest.Server, *agent.StubClient, *fakeIdentity, *monitorpair.Store) {
	t.Helper()
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := archive.NewStubCheckpointer(true)
	stub := agent.NewStubClient()
	mgr := service.New(orch, configmap.NewStore(fake.NewSimpleClientset(), "sessions"), ckpt, stub,
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexChat, ckpt))
	pairs, err := monitorpair.Open(t.Context(), filepath.Join(t.TempDir(), "pairs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pairs.Close() })
	ident := &fakeIdentity{}
	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	api.NewMonitor(pairs, ident, mgr, nil).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, stub, ident, pairs
}

func monitorCall(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The monitor claims the pair, and claiming it again after a restart lands on
// the same binding instead of making a second one.
func TestMonitorPairIsClaimedByTheMonitorAndSurvivesRestart(t *testing.T) {
	srv, _, ident, _ := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": parent.ID})

	ident.id = monitor.ID
	status, first := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/pair", nil)
	if status != http.StatusOK {
		t.Fatalf("bind = %d %+v", status, first)
	}
	if first["role"] != "monitor" {
		t.Fatalf("role = %v, want monitor", first["role"])
	}
	// A restarted monitor repeats the call; the binding must be the same one.
	_, again := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/pair", nil)
	b1, _ := first["binding"].(map[string]any)
	b2, _ := again["binding"].(map[string]any)
	if b1["createdAt"] != b2["createdAt"] || b1["monitorSessionId"] != b2["monitorSessionId"] {
		t.Fatalf("restart made a different binding: %+v vs %+v", b1, b2)
	}
	// The parent sees the same pair from its own side, with its role.
	ident.id = parent.ID
	status, view := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/pair", nil)
	if status != http.StatusOK || view["role"] != "parent" {
		t.Fatalf("parent view = %d %+v", status, view)
	}
	mon, _ := view["monitor"].(map[string]any)
	if mon["sessionId"] != monitor.ID {
		t.Fatalf("monitor view points elsewhere: %+v", mon)
	}
}

// A session outside any binding reaches nothing, and the one inside a binding
// reaches only its own pair's monitor — a query naming another session does
// not move the target.
func TestMonitorRoutesRefuseStrangersAndIgnoreNamedSessions(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{"name": "monitor", "workloadType": "codex-chat"})
	_, other := createRaw(t, srv.URL, map[string]any{"name": "other", "workloadType": "codex-chat"})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)

	// The unbound session is refused, not served someone else's work.
	ident.id = other.ID
	if status, _ := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil); status != http.StatusForbidden {
		t.Fatalf("stranger = %d, want 403", status)
	}

	// The parent is served, and naming another session changes nothing: the
	// target comes from the binding.
	ident.id = parent.ID
	status, plain := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if status != http.StatusOK {
		t.Fatalf("parent read = %d %+v", status, plain)
	}
	status, named := monitorCall(t, srv, "GET",
		"/api/v1/internal/monitor/work/tasks?session_id="+other.ID+"&monitorSessionId="+other.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("parent read with a named session = %d %+v", status, named)
	}
	a, _ := json.Marshal(plain["tasks"])
	b, _ := json.Marshal(named["tasks"])
	if string(a) != string(b) {
		t.Fatalf("a named session changed the answer:\n%s\n%s", a, b)
	}
}

// The parent's window: tasks with their latest turn, turn history, and the
// monitor's state beside them. The parent has no route that writes a task or a
// turn — intake is its only write.
func TestParentReadsTasksTurnsAndHasNoTaskWriteRoute(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{"name": "monitor", "workloadType": "codex-chat"})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)
	ident.id = parent.ID

	status, page := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	tasks, _ := page["tasks"].([]any)
	if status != http.StatusOK || len(tasks) != 2 {
		t.Fatalf("tasks = %d %+v", status, page)
	}
	if mon, _ := page["monitor"].(map[string]any); mon["state"] == nil {
		t.Fatalf("the parent must see the monitor's state beside the work: %+v", page)
	}
	status, turns := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks/task-moved/turns", nil)
	if status != http.StatusOK {
		t.Fatalf("turns = %d %+v", status, turns)
	}
	if _, ok := turns["turns"]; !ok {
		t.Fatalf("turn history missing: %+v", turns)
	}

	// There is no parent route that creates or edits a task or a turn. The
	// task path exists only under .../requests, so a write attempt elsewhere
	// is not served at all.
	for _, path := range []string{
		"/api/v1/internal/monitor/work/tasks",
		"/api/v1/internal/monitor/work/tasks/task-moved/turns",
	} {
		status, _ := monitorCall(t, srv, "POST", path, map[string]any{"status": "done"})
		if status == http.StatusOK {
			t.Fatalf("POST %s was served; the parent must not write task state", path)
		}
	}
}

// Intake: the parent sends an instruction into the monitor's inbox. The author
// is the proved parent session, not anything the body claims, and the reply
// carries the existing replay receipt so a retry is distinguishable from a
// second delegation.
func TestParentIntakeUsesTheExistingRequestReceipt(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{"name": "monitor", "workloadType": "codex-chat"})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)

	ident.id = parent.ID
	status, first := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-1", "text": "코멘트 전부 반영해 줘"})
	if status != http.StatusOK {
		t.Fatalf("intake = %d %+v", status, first)
	}
	if _, ok := first["replayed"]; !ok {
		t.Fatalf("the reply must carry the replay receipt: %+v", first)
	}
	// The monitor side may not send instructions to itself through the inbox:
	// it writes its own records directly.
	ident.id = monitor.ID
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-2", "text": "x"}); status != http.StatusForbidden {
		t.Fatalf("monitor intake = %d, want 403", status)
	}
}

// A deployment without the pair store answers 503 rather than serving the
// routes unauthenticated.
func TestMonitorRoutesAreOffWithoutTheStore(t *testing.T) {
	mux := http.NewServeMux()
	api.NewMonitor(nil, nil, nil, nil).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if status, _ := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/pair", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("disabled deployment = %d, want 503", status)
	}
}

// A workload cannot bind a parent slot by asking: the parent comes from the
// record the control plane wrote at create time, so a session created without
// one binds nothing, and a body naming someone else is not read at all.
func TestMonitorCannotBindAnArbitraryParentSlot(t *testing.T) {
	srv, _, ident, _ := monitorServer(t)
	_, victim := createRaw(t, srv.URL, map[string]any{"name": "victim-chat", "workloadType": "codex-chat"})
	_, mine := createRaw(t, srv.URL, map[string]any{"name": "my-chat", "workloadType": "codex-chat"})
	_, loose := createRaw(t, srv.URL, map[string]any{"name": "loose", "workloadType": "codex-chat"})
	_, bound := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": mine.ID})

	// No recorded pair: nothing to bind, whatever the body says.
	ident.id = loose.ID
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/pair",
		map[string]any{"parentSessionId": victim.ID}); status != http.StatusForbidden {
		t.Fatalf("unassigned workload bind = %d, want 403", status)
	}

	// A recorded pair binds to *that* parent even when the body names another.
	ident.id = bound.ID
	status, view := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/pair",
		map[string]any{"parentSessionId": victim.ID})
	if status != http.StatusOK {
		t.Fatalf("bind = %d %+v", status, view)
	}
	b, _ := view["binding"].(map[string]any)
	if b["parentSessionId"] != mine.ID {
		t.Fatalf("bound to %v, want the recorded parent %s", b["parentSessionId"], mine.ID)
	}

	// The parent side cannot claim a pair: it is not a monitor of anyone.
	ident.id = mine.ID
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/pair", nil); status != http.StatusForbidden {
		t.Fatalf("parent bind = %d, want 403", status)
	}
}

// A monitor may not itself be monitored: the create path refuses a second
// level, so the chain cannot become a loop or a ladder.
func TestMonitorChainIsOneDeep(t *testing.T) {
	srv, _, _, _ := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": parent.ID})
	status, _ := createStatus(t, srv.URL, map[string]any{
		"name": "second", "workloadType": "codex-chat", "monitorFor": monitor.ID})
	if status != http.StatusBadRequest {
		t.Fatalf("monitoring a monitor = %d, want 400", status)
	}
	// An unknown parent is refused too.
	if status, _ := createStatus(t, srv.URL, map[string]any{
		"name": "orphan", "workloadType": "codex-chat", "monitorFor": "nope"}); status != http.StatusNotFound {
		t.Fatalf("monitoring an unknown session = %d, want 404", status)
	}
}

// createStatus posts a create request and returns only its status, for the
// cases where the request is meant to be refused.
func createStatus(t *testing.T, base string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+"/api/v1/sessions", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}
