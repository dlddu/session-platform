// Package api exposes the control plane REST surface (/api/v1) over a
// session.Manager. Handlers are thin: decode, delegate to the manager, encode.
// Domain errors are mapped to HTTP status codes here.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// API holds the dependencies the handlers need.
type API struct {
	mgr           session.Manager
	testEndpoints bool
}

const maxRequestBodyBytes = 8 << 20

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

	if a.testEndpoints {
		// Test-only: freeze on demand without waiting for IdleReaper's 60-minute
		// product window. The e2e suite uses it to verify workload-specific archive
		// and restore behavior; it is deliberately not a product API. Restore needs
		// no endpoint because any access resumes a frozen session.
		mux.HandleFunc("POST /api/v1/sessions/{id}/snapshot", a.snapshotSession)
	}
}

// ---- request/response DTOs ----

type createReq struct {
	Name string `json:"name"`
	// WorkloadType stays raw so omitted can differ from explicit empty/null.
	WorkloadType json.RawMessage `json:"workloadType"`

	// Model is accepted only for claude-code. Omitted resolves to the stable
	// platform-default alias and is immutable with the workload type (AC-E6).
	Model json.RawMessage `json:"model"`
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
	if err := decodeRequestBody(r.Body, &req, true); err != nil {
		if errors.Is(err, session.ErrRequestBodyTooLarge) {
			writeErr(w, err)
		} else {
			writeErr(w, session.ErrInvalidInput)
		}
		return
	}
	var workload session.WorkloadType
	if len(req.WorkloadType) != 0 {
		if err := json.Unmarshal(req.WorkloadType, &workload); err != nil || !workload.Valid() {
			writeErr(w, session.ErrInvalidInput)
			return
		}
	}
	var model string
	if len(req.Model) != 0 {
		if err := json.Unmarshal(req.Model, &model); err != nil || model == "" || model != strings.TrimSpace(model) {
			writeErr(w, session.ErrInvalidInput)
			return
		}
	}
	sess, err := a.mgr.Create(r.Context(), session.CreateRequest{
		Name: req.Name, WorkloadType: workload, Model: model,
	})
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
	if err := decodeRequestBody(r.Body, &req, false); err != nil {
		writeErr(w, err)
		return
	}
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
	if err := decodeRequestBody(r.Body, &req, false); err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.mgr.Write(r.Context(), r.PathValue("id"), req.Payload)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeResp{Session: res.Session, Path: res.Path})
}

func (a *API) switchSession(w http.ResponseWriter, r *http.Request) {
	if err := decodeRequestBody(r.Body, &struct{}{}, false); err != nil {
		writeErr(w, err)
		return
	}
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
	if err := decodeRequestBody(r.Body, &struct{}{}, false); err != nil {
		writeErr(w, err)
		return
	}
	sess, err := a.mgr.Snapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// ---- helpers ----

func decodeRequestBody(body io.Reader, dst any, required bool) error {
	wire, err := io.ReadAll(io.LimitReader(body, maxRequestBodyBytes+1))
	if err != nil {
		return session.ErrInvalidInput
	}
	if len(wire) > maxRequestBodyBytes {
		return session.ErrRequestBodyTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(wire))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) && !required {
			return nil
		}
		return session.ErrInvalidInput
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil || containsJSONNull(value) {
		return session.ErrInvalidInput
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return session.ErrInvalidInput
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dst); err != nil {
		return session.ErrInvalidInput
	}
	return nil
}

func containsJSONNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case map[string]any:
		for _, child := range value {
			if containsJSONNull(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsJSONNull(child) {
				return true
			}
		}
	}
	return false
}

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
	case errors.Is(err, session.ErrWorkloadQueueFull):
		status = http.StatusTooManyRequests
	case errors.Is(err, session.ErrWorkloadPromptTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, session.ErrRequestBodyTooLarge):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, session.ErrWorkloadOutputFull):
		status = http.StatusInsufficientStorage
	case errors.Is(err, session.ErrCheckpointDisabled):
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, errResp{Error: err.Error()})
}
