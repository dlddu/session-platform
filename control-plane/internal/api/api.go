// Package api exposes the control plane REST surface (/api/v1) over a
// session.Manager. Handlers are thin: decode, delegate to the manager, encode.
// Domain errors are mapped to HTTP status codes here.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// API holds the dependencies the handlers need.
type API struct {
	mgr           session.Manager
	testEndpoints bool
}

// Option customises the API surface.
type Option func(*API)

// WithTestEndpoints registers the test-only endpoints (see Routes). Deployments
// leave it off; the e2e SUT turns it on via E2E_TEST_ENDPOINTS.
func WithTestEndpoints(enabled bool) Option {
	return func(a *API) { a.testEndpoints = enabled }
}

// New returns an API bound to a session.Manager.
func New(mgr session.Manager, opts ...Option) *API {
	a := &API{mgr: mgr}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Routes registers the /api/v1 endpoints on a ServeMux. Go 1.22+ method+path
// patterns keep routing dependency-free.
func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/healthz", a.health)
	mux.HandleFunc("POST /api/v1/sessions", a.createSession)
	mux.HandleFunc("GET /api/v1/sessions", a.listSessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.getSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/read", a.readSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/write", a.writeSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/switch", a.switchSession)

	// mock-exception: SNAPSHOT-TRIG — 동결에 이르는 제품 경로가 reaper의 유휴 창뿐이라 HTTP로
	// 결정적으로 도달할 수단이 없다. 이 스위치가 켜진 SUT에서만 등록되며, 배포는 끄고 쓴다.
	// 등재: docs/test/e2e.md 「e2e 충실도 허용목록」.
	if a.testEndpoints {
		// Test-only: freeze a session on demand (AC-B1's effect without its
		// trigger). The product's idle->snapshot *trigger policy* is still an
		// open decision (docs/doc-tracker.md), so this deliberately is NOT part
		// of the product API — it exists so the e2e suite can reach the snapshot
		// state and assert the CRIU round trip (AC-B2/B3/D4), which is otherwise
		// unreachable over HTTP. Restore needs no endpoint: any access to a
		// frozen session restores it (resume-on-access).
		mux.HandleFunc("POST /api/v1/sessions/{id}/snapshot", a.snapshotSession)
	}
}

// ---- request/response DTOs ----

type createReq struct {
	Name string `json:"name"`
}

type writeReq struct {
	Payload string `json:"payload"`
}

type readReq struct {
	// Offset is the nextOffset cursor issued by the previous read; 0 (or an
	// omitted body) reads the full output since session start (AC-D3).
	Offset int64 `json:"offset"`
}

type readResp struct {
	Session    *session.Session `json:"session"`
	Path       string           `json:"path"`
	Payload    string           `json:"payload"`
	NextOffset int64            `json:"nextOffset"`
}

type writeResp struct {
	Session *session.Session `json:"session"`
	Path    string           `json:"path"`
}

type errResp struct {
	Error string `json:"error"`
}

// ---- handlers ----

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, session.ErrInvalidInput)
		return
	}
	sess, err := a.mgr.Create(r.Context(), session.CreateRequest{Name: req.Name})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.mgr.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := a.mgr.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (a *API) readSession(w http.ResponseWriter, r *http.Request) {
	var req readReq
	// body is optional: no body (or no offset) means "from the beginning"
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Offset < 0 {
		writeErr(w, session.ErrInvalidInput)
		return
	}
	res, err := a.mgr.Read(r.Context(), r.PathValue("id"), req.Offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, readResp{Session: res.Session, Path: res.Path, Payload: res.Payload, NextOffset: res.NextOffset})
}

func (a *API) writeSession(w http.ResponseWriter, r *http.Request) {
	var req writeReq
	// body is optional: an empty payload is a valid (no-op) shell write
	_ = json.NewDecoder(r.Body).Decode(&req)
	res, err := a.mgr.Write(r.Context(), r.PathValue("id"), req.Payload)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeResp{Session: res.Session, Path: res.Path})
}

func (a *API) switchSession(w http.ResponseWriter, r *http.Request) {
	sess, err := a.mgr.Switch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// snapshotSession freezes a session and reclaims its pod (AC-B1/AC-A3).
// Registered only with WithTestEndpoints — see Routes.
func (a *API) snapshotSession(w http.ResponseWriter, r *http.Request) {
	sess, err := a.mgr.Snapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, session.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, session.ErrInvalidInput):
		status = http.StatusBadRequest
	case errors.Is(err, session.ErrInvalidState):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, session.ErrConflict):
		status = http.StatusConflict
	}
	writeJSON(w, status, errResp{Error: err.Error()})
}
