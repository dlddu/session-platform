package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func newServer(opts ...api.Option) *httptest.Server {
	srv, _, _ := newServerWithOrchestrator(opts...)
	return srv
}

func newServerWithOrchestrator(
	opts ...api.Option,
) (*httptest.Server, *k8s.StubOrchestrator, *configmap.Store) {
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := criu.NewStubCheckpointer(true)
	stateStore := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	mgr := service.New(
		orch,
		stateStore,
		ckpt,
		agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	mux := http.NewServeMux()
	api.New(mgr, opts...).Routes(mux)
	return httptest.NewServer(mux), orch, stateStore
}

func createForTest(t *testing.T, srv *httptest.Server, name string) session.Session {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var s session.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	return s
}

// The product snapshot endpoint lets a user archive a session immediately,
// without waiting for the idle reaper, and reclaims its pod.
func TestSnapshotEndpointArchivesSession(t *testing.T) {
	srv := newServer()
	defer srv.Close()
	s := createForTest(t, srv, "manual-archive")
	resp, err := http.Post(srv.URL+"/api/v1/sessions/"+s.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", resp.StatusCode)
	}
	var frozen session.Session
	if err := json.NewDecoder(resp.Body).Decode(&frozen); err != nil {
		t.Fatalf("decode snapshot response: %v", err)
	}
	if frozen.State != session.StateSnapshot {
		t.Errorf("state = %q, want snapshot", frozen.State)
	}
	if frozen.Pod != "" {
		t.Errorf("pod = %q, want it reclaimed (AC-A3)", frozen.Pod)
	}
}

func TestSnapshotUnknownSession(t *testing.T) {
	srv := newServer()
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/v1/sessions/nope/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("snapshot of an unknown session = %d, want 404", resp.StatusCode)
	}
}

// Create -> list -> switch end-to-end through the HTTP surface with stub adapters.
func TestHappyPath(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	// create
	body, _ := json.Marshal(map[string]string{"name": "api-gateway-dev"})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created session.Session
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	resp.Body.Close()
	if created.State != session.StateActive {
		t.Errorf("new session state = %q, want active", created.State)
	}
	if created.Pod == "" {
		t.Error("new session should have a pod assigned (AC-A2)")
	}

	// list
	resp, err = http.Get(srv.URL + "/api/v1/sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(listed.Sessions) != 1 {
		t.Fatalf("list len = %d, want 1", len(listed.Sessions))
	}

	// switch (active -> active no-op)
	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/switch", "application/json", nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// Drives write→read over HTTP: the write payload comes back from the (stub) agent
// and read honours the offset/nextOffset cursor contract.
func TestReadWriteWireContract(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"name": "shell-wire"})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created session.Session
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	resp.Body.Close()

	wbody, _ := json.Marshal(map[string]string{"payload": "echo hi\n"})
	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/write", "application/json", bytes.NewReader(wbody))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status = %d, want 200", resp.StatusCode)
	}

	readAt := func(offset int64) (string, int64) {
		t.Helper()
		rbody, _ := json.Marshal(map[string]int64{"offset": offset})
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/read", "application/json", bytes.NewReader(rbody))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("read status = %d, want 200", resp.StatusCode)
		}
		var out struct {
			Payload    string `json:"payload"`
			NextOffset int64  `json:"nextOffset"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode read: %v", err)
		}
		return out.Payload, out.NextOffset
	}

	payload, next := readAt(0)
	if payload != "echo hi\n" {
		t.Fatalf("read payload = %q, want the written payload", payload)
	}
	if next != int64(len("echo hi\n")) {
		t.Fatalf("nextOffset = %d, want %d", next, len("echo hi\n"))
	}
	if delta, _ := readAt(next); delta != "" {
		t.Fatalf("cursor read = %q, want empty delta", delta)
	}

	// A negative offset is invalid input.
	rbody, _ := json.Marshal(map[string]int64{"offset": -1})
	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/read", "application/json", bytes.NewReader(rbody))
	if err != nil {
		t.Fatalf("read -1: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative offset status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"name": ""})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty-name create status = %d, want 400", resp.StatusCode)
	}
}

func TestGetUnknownReturns404(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/sessions/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown get status = %d, want 404", resp.StatusCode)
	}
}

// Deleting an active session returns an empty 204 response, reclaims its pod,
// and removes it from subsequent reads.
func TestDeleteSession(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()
	created := createForTest(t, srv, "delete-me")

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/sessions/"+created.ID, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		t.Errorf("delete content-type = %q, want no response body", contentType)
	}
	if got := orch.RunningCount(); got != 0 {
		t.Errorf("running pods after delete = %d, want 0", got)
	}

	getResp, err := http.Get(srv.URL + "/api/v1/sessions/" + created.ID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", getResp.StatusCode)
	}
}

// Unknown IDs stay distinguishable from successfully deleted sessions.
func TestDeleteUnknownSessionReturns404(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/sessions/nope", nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown status = %d, want 404", resp.StatusCode)
	}
}

// A competing lifecycle holder is surfaced as 409 and leaves the session intact.
func TestDeleteConflictReturns409(t *testing.T) {
	srv, _, stateStore := newServerWithOrchestrator()
	defer srv.Close()
	created := createForTest(t, srv, "delete-conflict")
	const owner = "snapshot-owner"

	if err := stateStore.Lock(context.Background(), created.ID, owner); err != nil {
		t.Fatalf("lock session: %v", err)
	}
	defer func() {
		_ = stateStore.Unlock(context.Background(), created.ID, owner)
	}()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/sessions/"+created.ID, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete conflicted session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete conflict status = %d, want 409", resp.StatusCode)
	}

	getResp, err := http.Get(srv.URL + "/api/v1/sessions/" + created.ID)
	if err != nil {
		t.Fatalf("get after conflict: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get after conflict status = %d, want 200", getResp.StatusCode)
	}
}

// A snapshotted session has no live pod but its logical record is still
// deletable through the same product endpoint.
func TestDeleteSnapshotSession(t *testing.T) {
	srv := newServer()
	defer srv.Close()
	created := createForTest(t, srv, "delete-frozen")

	snapshotResp, err := http.Post(
		srv.URL+"/api/v1/sessions/"+created.ID+"/snapshot",
		"application/json",
		nil,
	)
	if err != nil {
		t.Fatalf("snapshot before delete: %v", err)
	}
	snapshotResp.Body.Close()
	if snapshotResp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", snapshotResp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/sessions/"+created.ID, nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete snapshot status = %d, want 204", resp.StatusCode)
	}

	getResp, err := http.Get(srv.URL + "/api/v1/sessions/" + created.ID)
	if err != nil {
		t.Fatalf("get after snapshot delete: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after snapshot delete status = %d, want 404", getResp.StatusCode)
	}
}

func TestStreamEndpointProxiesSSEAndPrefersLastEventID(t *testing.T) {
	srv := newServer()
	defer srv.Close()
	created := createForTest(t, srv, "stream-wire")

	body, _ := json.Marshal(map[string]string{"payload": "abcdef"})
	writeResp, err := http.Post(
		srv.URL+"/api/v1/sessions/"+created.ID+"/write",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("write before stream: %v", err)
	}
	writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusOK {
		t.Fatalf("write status = %d, want 200", writeResp.StatusCode)
	}

	req, err := http.NewRequest(
		http.MethodGet,
		srv.URL+"/api/v1/sessions/"+created.ID+"/stream?offset=not-used",
		nil,
	)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Last-Event-ID", "2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	streamBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("stream content type = %q", got)
	}
	text := string(streamBody)
	for _, want := range []string{
		"id: 6",
		"event: output",
		`"offset":2`,
		`"payloadBase64":"Y2RlZg=="`,
		`"nextOffset":6`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream body %q does not contain %q", text, want)
		}
	}

	badReq, _ := http.NewRequest(
		http.MethodGet,
		srv.URL+"/api/v1/sessions/"+created.ID+"/stream?offset=0",
		nil,
	)
	badReq.Header.Set("Last-Event-ID", "-1")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("invalid stream cursor: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid Last-Event-ID status = %d, want 400", badResp.StatusCode)
	}
}

func TestStreamDoesNotRestoreSnapshot(t *testing.T) {
	srv := newServer()
	defer srv.Close()
	created := createForTest(t, srv, "passive-stream")
	snapshotResp, err := http.Post(
		srv.URL+"/api/v1/sessions/"+created.ID+"/snapshot",
		"application/json",
		nil,
	)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snapshotResp.Body.Close()
	if snapshotResp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", snapshotResp.StatusCode)
	}

	resp, err := http.Get(srv.URL + "/api/v1/sessions/" + created.ID + "/stream?offset=0")
	if err != nil {
		t.Fatalf("stream snapshot: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("snapshot stream status = %d, want 422", resp.StatusCode)
	}

	getResp, err := http.Get(srv.URL + "/api/v1/sessions/" + created.ID)
	if err != nil {
		t.Fatalf("get after passive stream: %v", err)
	}
	defer getResp.Body.Close()
	var after session.Session
	if err := json.NewDecoder(getResp.Body).Decode(&after); err != nil {
		t.Fatalf("decode session after passive stream: %v", err)
	}
	if after.State != session.StateSnapshot || after.Pod != "" {
		t.Fatalf("stream restored snapshot unexpectedly: state=%s pod=%q", after.State, after.Pod)
	}
}
