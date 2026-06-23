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
	mgr session.Manager
}

// New returns an API bound to a session.Manager.
func New(mgr session.Manager) *API { return &API{mgr: mgr} }

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
}

// ---- request/response DTOs ----

type createReq struct {
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
}

type writeReq struct {
	Payload string `json:"payload"`
}

type readResp struct {
	Session *session.Session `json:"session"`
	Path    string           `json:"path"`
	Payload string           `json:"payload"`
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
	sess, err := a.mgr.Create(r.Context(), session.CreateRequest{Name: req.Name, Region: req.Region})
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
	res, err := a.mgr.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, readResp{Session: res.Session, Path: res.Path, Payload: res.Payload})
}

func (a *API) writeSession(w http.ResponseWriter, r *http.Request) {
	var req writeReq
	// body is optional for the stub
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
