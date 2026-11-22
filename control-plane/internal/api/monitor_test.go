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

// The parent keeps its window while the monitor is frozen: the control plane
// serves the last read it took, labelled as a mirror with its own timestamp,
// and says plainly that intake is refused and how to recover.
func TestParentReadsStoredRecordsWhileTheMonitorIsFrozen(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": parent.ID})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)
	ident.id = parent.ID

	// A live read fills the mirror.
	status, live := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if status != http.StatusOK || live["source"] != "slot" {
		t.Fatalf("live read = %d %+v", status, live)
	}
	liveTasks, _ := json.Marshal(live["tasks"])

	// Freeze the monitor for real: snapshot it through the product API, so its
	// pod is gone and there is nothing to proxy to.
	freeze, err := http.Post(srv.URL+"/api/v1/sessions/"+monitor.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	freeze.Body.Close()
	if freeze.StatusCode != http.StatusOK && freeze.StatusCode != http.StatusAccepted {
		t.Fatalf("snapshot = %d", freeze.StatusCode)
	}
	status, stored := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if status != http.StatusOK {
		t.Fatalf("frozen read = %d %+v; the parent must still see its list", status, stored)
	}
	if stored["source"] != "mirror" {
		t.Fatalf("source = %v, want mirror", stored["source"])
	}
	if stored["asOf"] == nil {
		t.Fatalf("a mirror must carry its own timestamp: %+v", stored)
	}
	if got, _ := json.Marshal(stored["tasks"]); string(got) != string(liveTasks) {
		t.Fatalf("mirror lost records:\n%s\n%s", got, liveTasks)
	}

	// Intake is a different matter: it cannot be mirrored, so it is refused
	// rather than silently dropped, and the reply says how to recover.
	status, heldNow := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-frozen", "text": "x"})
	if status != http.StatusAccepted {
		t.Fatalf("frozen intake = %d %+v, want 202 durable intake", status, heldNow)
	}
	heldItem, _ := heldNow["item"].(map[string]any)
	if heldItem["state"] != "accepted" || heldItem["turnId"] != nil {
		t.Fatalf("a held instruction must not claim a turn: %+v", heldNow)
	}
	mon, _ := heldNow["monitor"].(map[string]any)
	if mon["intake"] != "refused" || mon["recovery"] == nil {
		t.Fatalf("the reply must still say the monitor is down and how to recover: %+v", heldNow)
	}
	if heldNow["note"] == nil {
		t.Fatalf("a held instruction must say it is held, not run: %+v", heldNow)
	}

	// A binding the control plane never managed to read has no mirror to
	// serve, which the parent is told instead of being shown an empty list.
	_, otherParent := createRaw(t, srv.URL, map[string]any{"name": "chat2", "workloadType": "codex-chat"})
	_, otherMonitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor2", "workloadType": "codex-chat", "monitorFor": otherParent.ID})
	if _, err := pairs.Bind(t.Context(), otherParent.ID, otherMonitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	// While that monitor is up the read is live and simply empty — that is a
	// real answer, not a mirror.
	ident.id = otherParent.ID
	status, empty := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if status != http.StatusOK || empty["source"] != "slot" {
		t.Fatalf("live empty read = %d %+v", status, empty)
	}
	// That read, empty as it was, is now the stored answer: freezing the
	// monitor afterwards serves it as a mirror, not as live state.
	frozen2, err := http.Post(srv.URL+"/api/v1/sessions/"+otherMonitor.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen2.Body.Close()
	status, mirroredEmpty := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if status != http.StatusOK || mirroredEmpty["source"] != "mirror" || mirroredEmpty["asOf"] == nil {
		t.Fatalf("frozen read after an empty live read = %d %+v", status, mirroredEmpty)
	}

	// A pair the control plane never managed to read has nothing stored, so
	// the parent is told rather than shown an empty list it might trust.
	_, thirdParent := createRaw(t, srv.URL, map[string]any{"name": "chat3", "workloadType": "codex-chat"})
	_, thirdMonitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor3", "workloadType": "codex-chat", "monitorFor": thirdParent.ID})
	if _, err := pairs.Bind(t.Context(), thirdParent.ID, thirdMonitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	frozen3, err := http.Post(srv.URL+"/api/v1/sessions/"+thirdMonitor.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen3.Body.Close()
	ident.id = thirdParent.ID
	if status, _ := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("never-read binding = %d, want 503 rather than an empty list", status)
	}
}

// snapshotOf is what the monitor publishes after a commit: the same shape the
// slot serves, so the parent reads one format either way.
func publishTasks(t *testing.T, srv *httptest.Server, tasks []map[string]any) {
	t.Helper()
	status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/snapshot", map[string]any{
		"kind": "tasks", "payload": map[string]any{"tasks": tasks, "next_cursor": ""},
	})
	if status != http.StatusOK {
		t.Fatalf("publish tasks = %d", status)
	}
}

// One real instruction end to end: the parent sends it, the control plane
// stores it durably first, the monitor's slot takes it as a turn, the turn id
// lands beside the stored item, and the parent can read the outcome.
func TestOneInstructionGoesIntakeThenDelegationThenResult(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": parent.ID})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)

	ident.id = parent.ID
	status, sent := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-e2e", "text": "코멘트 전부 반영해 줘"})
	if status != http.StatusOK {
		t.Fatalf("intake = %d %+v", status, sent)
	}
	item, _ := sent["item"].(map[string]any)
	if item["state"] != "forwarded" || item["turnId"] == nil {
		t.Fatalf("a live monitor must have taken the turn: %+v", sent)
	}
	if sent["turn"] == nil {
		t.Fatalf("the slot's turn is the handle and must come back: %+v", sent)
	}
	// Exactly one delegation reached the slot, under the parent's own key.
	if len(stub.WorkRequests) != 1 || stub.WorkRequests[0].ID != "req-e2e" {
		t.Fatalf("slot got %+v, want one request keyed req-e2e", stub.WorkRequests)
	}
	if stub.WorkRequests[0].Author != parent.ID {
		t.Fatalf("author = %q, want the proved parent", stub.WorkRequests[0].Author)
	}

	// A reconnect replays the receipt and does not delegate again.
	status, again := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-e2e", "text": "코멘트 전부 반영해 줘"})
	if status != http.StatusOK || again["replayed"] != true {
		t.Fatalf("retry = %d %+v, want a replayed receipt", status, again)
	}
	if len(stub.WorkRequests) != 1 {
		t.Fatalf("retry delegated again: %+v", stub.WorkRequests)
	}

	// The same id with different text is refused rather than overwriting what
	// may already be running.
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-e2e", "text": "something else"}); status != http.StatusConflict {
		t.Fatalf("reused id with new text = %d, want 409", status)
	}

	// The parent can ask what happened to it, as a pure read.
	status, view := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/requests/req-e2e", nil)
	if status != http.StatusOK {
		t.Fatalf("request status = %d %+v", status, view)
	}
	got, _ := view["item"].(map[string]any)
	if got["state"] != "forwarded" || got["author"] != parent.ID {
		t.Fatalf("request view = %+v", got)
	}
}

// While the monitor is away the instruction is held, not dropped and not
// faked: the parent sees "accepted", the monitor drains it on resume, and the
// delegation happens exactly once even across a restart.
func TestHeldInstructionIsDrainedOnceAfterRestart(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": parent.ID})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)

	// The monitor goes away.
	freeze, err := http.Post(srv.URL+"/api/v1/sessions/"+monitor.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	freeze.Body.Close()

	ident.id = parent.ID
	status, held := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-held", "text": "재개하면 처리해 줘"})
	if status != http.StatusAccepted {
		t.Fatalf("held intake = %d %+v, want 202", status, held)
	}
	item, _ := held["item"].(map[string]any)
	if item["state"] != "accepted" || item["turnId"] != nil {
		t.Fatalf("a held item must not claim a turn: %+v", held)
	}
	if len(stub.WorkRequests) != 0 {
		t.Fatalf("nothing should have reached the slot: %+v", stub.WorkRequests)
	}
	// Retrying while still away is still one item, still accepted.
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/tasks/task-moved/requests",
		map[string]any{"id": "req-held", "text": "재개하면 처리해 줘"}); status != http.StatusAccepted {
		t.Fatalf("second held intake = %d, want 202", status)
	}

	// The monitor comes back and drains its inbox.
	ident.id = monitor.ID
	status, inbox := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/inbox", nil)
	pending, _ := inbox["pending"].([]any)
	if status != http.StatusOK || len(pending) != 1 {
		t.Fatalf("inbox = %d %+v, want exactly one pending", status, inbox)
	}
	// It delegates through the slot's own path and records the turn it got.
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/inbox/req-held/forwarded",
		map[string]any{"turnId": 7, "sessionSeq": 3}); status != http.StatusOK {
		t.Fatalf("mark forwarded = %d", status)
	}
	// A monitor that crashed after the slot write repeats the same call safely.
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/inbox/req-held/forwarded",
		map[string]any{"turnId": 7}); status != http.StatusOK {
		t.Fatalf("repeating the same turn = %d, want 200", status)
	}
	// Claiming a *different* turn for the same request would mean it ran
	// twice, so it is refused.
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/inbox/req-held/forwarded",
		map[string]any{"turnId": 8}); status != http.StatusConflict {
		t.Fatalf("second turn for one request = %d, want 409", status)
	}
	// Drained: nothing is pending any more.
	_, drained := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/inbox", nil)
	if left, _ := drained["pending"].([]any); len(left) != 0 {
		t.Fatalf("still pending after forwarding: %+v", drained)
	}
	// And the parent sees the delegation, with the slot's turn id.
	ident.id = parent.ID
	_, view := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/requests/req-held", nil)
	got, _ := view["item"].(map[string]any)
	if got["state"] != "forwarded" || got["turnId"] == nil {
		t.Fatalf("parent view of a drained item = %+v", got)
	}
}

// The parent's offline window does not depend on having read before: the
// monitor publishes its committed state, so work the parent never looked at is
// still there, with its history and the time it was published.
func TestParentReadsPublishedStateItNeverFetchedBefore(t *testing.T) {
	srv, stub, ident, pairs := monitorServer(t)
	_, parent := createRaw(t, srv.URL, map[string]any{"name": "chat", "workloadType": "codex-chat"})
	_, monitor := createRaw(t, srv.URL, map[string]any{
		"name": "monitor", "workloadType": "codex-chat", "monitorFor": parent.ID})
	if _, err := pairs.Bind(t.Context(), parent.ID, monitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	workFixture(stub)

	// The monitor registers work and publishes it. The parent has never read.
	ident.id = monitor.ID
	publishTasks(t, srv, []map[string]any{
		{"task": map[string]any{"id": "task-new", "title": "모니터가 등록한 작업", "status": "planned", "version": 1}, "turns_total": 1},
	})
	if status, _ := monitorCall(t, srv, "POST", "/api/v1/internal/monitor/work/snapshot", map[string]any{
		"kind": "turns", "key": "task-new",
		"payload": map[string]any{
			"task_id": "task-new", "task_version": 1,
			"task":  map[string]any{"id": "task-new", "title": "모니터가 등록한 작업", "status": "planned", "version": 1},
			"turns": []map[string]any{{"id": 1, "task_id": "task-new", "requested_at": "2026-11-22T00:00:00Z"}},
		},
	}); status != http.StatusOK {
		t.Fatalf("publish turns failed")
	}

	// Now the monitor goes away, and the parent looks for the first time.
	freeze, err := http.Post(srv.URL+"/api/v1/sessions/"+monitor.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	freeze.Body.Close()

	ident.id = parent.ID
	status, page := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if status != http.StatusOK || page["source"] != "published" || page["asOf"] == nil {
		t.Fatalf("first-ever read while frozen = %d %+v", status, page)
	}
	tasks, _ := page["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("published tasks not served: %+v", page)
	}
	status, turns := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks/task-new/turns", nil)
	if status != http.StatusOK || turns["source"] != "published" {
		t.Fatalf("published history = %d %+v", status, turns)
	}

	// Reading again changes nothing — not the payload, not the published time.
	_, againPage := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil)
	if againPage["asOf"] != page["asOf"] {
		t.Fatalf("a read moved the published time: %v -> %v", page["asOf"], againPage["asOf"])
	}
	a, _ := json.Marshal(page["tasks"])
	b2, _ := json.Marshal(againPage["tasks"])
	if string(a) != string(b2) {
		t.Fatalf("a read changed the records")
	}

	// Another pair's parent cannot see it.
	_, otherParent := createRaw(t, srv.URL, map[string]any{"name": "other", "workloadType": "codex-chat"})
	_, otherMonitor := createRaw(t, srv.URL, map[string]any{
		"name": "other-monitor", "workloadType": "codex-chat", "monitorFor": otherParent.ID})
	if _, err := pairs.Bind(t.Context(), otherParent.ID, otherMonitor.ID, ""); err != nil {
		t.Fatal(err)
	}
	frozen, err := http.Post(srv.URL+"/api/v1/sessions/"+otherMonitor.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen.Body.Close()
	ident.id = otherParent.ID
	if status, _ := monitorCall(t, srv, "GET", "/api/v1/internal/monitor/work/tasks", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("another pair's parent saw something: %d", status)
	}
}
