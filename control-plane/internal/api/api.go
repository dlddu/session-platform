// Package api exposes the control plane REST surface (/api/v1) over a
// session.Manager.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/tenancy"
)

type API struct {
	mgr          session.Manager
	modelConfigs map[session.WorkloadType]modelConfig
	// nil offers every known type; an empty map offers none.
	offered map[session.WorkloadType]bool
	callers tenancy.Resolver
	tenants *tenancy.Config
}

type modelConfig struct {
	defaultModel string
	models       []string
}

const maxRequestBodyBytes = 8 << 20

type Option func(*API)

// WithClaudeCodeModelConfig exposes the rollout-scoped, non-sensitive model
// picker configuration to the SPA. Copying it keeps the API's startup snapshot
// immutable.
func WithClaudeCodeModelConfig(defaultModel string, models []string) Option {
	return WithWorkloadModelConfig(session.WorkloadTypeClaudeCode, defaultModel, models)
}

func WithWorkloadModelConfig(w session.WorkloadType, defaultModel string, models []string) Option {
	return func(a *API) {
		a.modelConfigs[w] = modelConfig{defaultModel: defaultModel, models: append([]string{}, models...)}
	}
}

// WithOfferedWorkloadTypes filters GET /config only; create still validates against every known type.
func WithOfferedWorkloadTypes(types ...session.WorkloadType) Option {
	return func(a *API) {
		a.offered = make(map[session.WorkloadType]bool, len(types))
		for _, w := range types {
			a.offered[w] = true
		}
	}
}

// WithTenancy scopes every session route to the caller's tenant (AC-K1~K4).
// Without it every request is the operator, exactly as before tenancy.
func WithTenancy(callers tenancy.Resolver, tenants *tenancy.Config) Option {
	return func(a *API) {
		a.callers = callers
		a.tenants = tenants
	}
}

func New(mgr session.Manager, opts ...Option) *API {
	a := &API{
		mgr:          mgr,
		modelConfigs: map[session.WorkloadType]modelConfig{},
		callers:      tenancy.StaticResolver(tenancy.OperatorCaller),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Routes registers the /api/v1 endpoints on a ServeMux. Go 1.22+ method+path
// patterns keep routing dependency-free.
func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/healthz", a.health)
	mux.HandleFunc("GET /api/v1/config", a.runtimeConfig)
	mux.HandleFunc("POST /api/v1/sessions", a.createSession)
	mux.HandleFunc("GET /api/v1/sessions", a.listSessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.getSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", a.deleteSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/stream", a.streamSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/read", a.readSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/write", a.writeSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/switch", a.switchSession)

	mux.HandleFunc("POST /api/v1/sessions/{id}/snapshot", a.snapshotSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/plugins/refresh", a.refreshPlugins)
	mux.HandleFunc("POST /api/v1/sessions/{id}/recover", a.recoverSession)
}

// ---- request/response DTOs ----

type createReq struct {
	Name string `json:"name"`
	// WorkloadType stays raw so omitted can differ from explicit empty/null.
	WorkloadType json.RawMessage `json:"workloadType"`

	// Model is raw for the same reason; its contract is AC-E6.
	Model json.RawMessage `json:"model"`

	// Plugins is raw so an omitted list (the type's default plugins) differs
	// from an explicit null; its contract is AC-L1.
	Plugins json.RawMessage `json:"plugins"`
	// ReasoningEffort is raw for the same reason; its contract is AC-E8.
	ReasoningEffort json.RawMessage `json:"reasoningEffort"`

	Tenant json.RawMessage `json:"tenant"`
}

type writeReq struct {
	Payload string `json:"payload"`
	// PermissionMode is the turn's permission mode, claude-code only (AC-E2).
	// A pointer so an explicitly empty value is refused rather than read as the
	// default.
	PermissionMode *string `json:"permissionMode"`
}

type readReq struct {
	// Offset is the read cursor defined by AC-D3.
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

// pluginRefreshResp flattens the refresh's changes beside the session.
type pluginRefreshResp struct {
	Session *session.Session `json:"session"`
	Path    string           `json:"path"`
	session.PluginChanges
}

type runtimeConfigResp struct {
	// ClaudeCode predates WorkloadTypes and is kept for existing clients.
	ClaudeCode    claudeCodeConfigResp   `json:"claudeCode"`
	WorkloadTypes []workloadTypeConfResp `json:"workloadTypes"`
	// Tenant is the calling tenant, null for the operator (AC-K4).
	Tenant *tenantResp `json:"tenant"`
	// Tenants is what the operator may pick from; empty for a tenant caller.
	Tenants []tenantResp `json:"tenants"`
}

type tenantResp struct {
	Name          string                 `json:"name"`
	WorkloadTypes []session.WorkloadType `json:"workloadTypes"`
}

type workloadTypeConfResp struct {
	Type        session.WorkloadType `json:"type"`
	Label       string               `json:"label"`
	Description string               `json:"description"`
	TakesModel  bool                 `json:"takesModel"`
	// TakesPlugins says whether a create request may carry plugins (AC-L1).
	TakesPlugins bool              `json:"takesPlugins"`
	Input        session.InputMode `json:"input"`
	// ApprovalGate tells the client to render the gated workspace (AC-F2~F6).
	ApprovalGate bool `json:"approvalGate"`
	// DefaultModel is null exactly when TakesModel is false.
	DefaultModel *string  `json:"defaultModel"`
	Models       []string `json:"models"`
	// ReasoningEfforts is never null; empty means the type takes none (AC-E8).
	ReasoningEfforts []string `json:"reasoningEfforts"`
}

type claudeCodeConfigResp struct {
	DefaultModel string   `json:"defaultModel"`
	Models       []string `json:"models"`
}

type errResp struct {
	Error string `json:"error"`
}

// ---- handlers ----

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) runtimeConfig(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.caller(w, r)
	if !ok {
		return
	}
	// Avoid browser/proxy caching so a control-plane rollout immediately
	// exposes its new snapshot.
	w.Header().Set("Cache-Control", "no-store")
	claude := a.modelConfig(session.WorkloadTypeClaudeCode)
	resp := runtimeConfigResp{
		ClaudeCode:    claudeCodeConfigResp{DefaultModel: claude.defaultModel, Models: claude.models},
		WorkloadTypes: []workloadTypeConfResp{},
		Tenants:       []tenantResp{},
	}
	if caller.Tenant != nil {
		resp.Tenant = &tenantResp{Name: caller.Tenant.Name, WorkloadTypes: a.offeredOf(caller.Tenant)}
	} else {
		for _, t := range a.tenants.Tenants() {
			resp.Tenants = append(resp.Tenants, tenantResp{Name: t.Name, WorkloadTypes: a.offeredOf(t)})
		}
	}
	for _, info := range session.WorkloadTypes() {
		if a.offered != nil && !a.offered[info.Type] {
			continue
		}
		if caller.Tenant != nil && !caller.Tenant.Allows(info.Type) {
			continue
		}
		entry := workloadTypeConfResp{
			Type:         info.Type,
			Label:        info.Label,
			Description:  info.Description,
			TakesModel:   info.TakesModel,
			TakesPlugins: info.TakesPlugins,
			Input:        info.Input,
			ApprovalGate: info.ApprovalGate,
			Models:       []string{},
			// WorkloadTypes copies the list, so it is never the catalog's own.
			ReasoningEfforts: info.ReasoningEfforts,
		}
		if info.TakesModel {
			mc := a.modelConfig(info.Type)
			entry.DefaultModel = &mc.defaultModel
			entry.Models = mc.models
		}
		resp.WorkloadTypes = append(resp.WorkloadTypes, entry)
	}
	writeJSON(w, http.StatusOK, resp)
}

// offeredOf keeps the types of a tenant's grant this deployment offers, in
// catalog order, so a picker never shows a type whose create would fail.
func (a *API) offeredOf(t *tenancy.Tenant) []session.WorkloadType {
	out := []session.WorkloadType{}
	for _, info := range session.WorkloadTypes() {
		if (a.offered == nil || a.offered[info.Type]) && t.Allows(info.Type) {
			out = append(out, info.Type)
		}
	}
	return out
}

func (a *API) modelConfig(w session.WorkloadType) modelConfig {
	mc, ok := a.modelConfigs[w]
	if !ok || mc.defaultModel == "" {
		mc.defaultModel = session.PlatformDefaultModel
	}
	mc.models = append([]string{}, mc.models...)
	return mc
}

func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.caller(w, r)
	if !ok {
		return
	}
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
		if err := json.Unmarshal(req.WorkloadType, &workload); err != nil || workload == "" {
			writeErr(w, session.ErrInvalidInput)
			return
		}
		normalized, err := session.NormalizeWorkloadType(workload)
		if err != nil {
			writeErr(w, session.ErrInvalidInput)
			return
		}
		workload = normalized
	}
	var model string
	if len(req.Model) != 0 {
		if err := json.Unmarshal(req.Model, &model); err != nil || model == "" || model != strings.TrimSpace(model) {
			writeErr(w, session.ErrInvalidInput)
			return
		}
	}
	var plugins []string
	if len(req.Plugins) != 0 {
		// An explicit null decodes to a nil slice, which would silently mean
		// "type default"; only a JSON array is a selection.
		if err := json.Unmarshal(req.Plugins, &plugins); err != nil || plugins == nil {
			writeErr(w, session.ErrInvalidInput)
			return
		}
	}
	var effort string
	if len(req.ReasoningEffort) != 0 {
		if err := json.Unmarshal(req.ReasoningEffort, &effort); err != nil || effort == "" {
			writeErr(w, session.ErrInvalidInput)
			return
		}
	}
	tenant, err := a.createTenant(caller, req.Tenant, workload)
	if err != nil {
		writeErr(w, err)
		return
	}
	sess, err := a.mgr.Create(r.Context(), session.CreateRequest{
		Name: req.Name, WorkloadType: workload, Model: model, Plugins: plugins, ReasoningEffort: effort, Tenant: tenant,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.caller(w, r)
	if !ok {
		return
	}
	filter, filtered := r.URL.Query()["tenant"]
	if filtered && (!caller.Operator || len(filter) != 1) {
		writeErr(w, session.ErrInvalidInput)
		return
	}
	sessions, err := a.mgr.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	visible := make([]*session.Session, 0, len(sessions))
	for _, sess := range sessions {
		if !caller.Sees(sess.Tenant) || (filtered && sess.Tenant != filter[0]) {
			continue
		}
		visible = append(visible, sess)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": visible})
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	sess, err := a.mgr.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (a *API) deleteSession(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	if err := a.mgr.Terminate(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// streamSession proxies the selected pod's passive SSE output feed. The
// Last-Event-ID header wins over the query cursor so a native EventSource
// reconnect resumes from the last event the browser accepted.
func (a *API) streamSession(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	offset, err := outputStreamOffset(r)
	if err != nil {
		writeErr(w, session.ErrInvalidInput)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errResp{Error: "streaming is unsupported by this server"})
		return
	}
	stream, err := a.mgr.Stream(r.Context(), r.PathValue("id"), offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	buf := make([]byte, 32<<10)
	for {
		n, readErr := stream.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

func outputStreamOffset(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("offset")
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		value = lastEventID
	}
	if value == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		return 0, session.ErrInvalidInput
	}
	return offset, nil
}

func (a *API) readSession(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	var req readReq
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
	if !a.authorizeSession(w, r) {
		return
	}
	var req writeReq
	if err := decodeRequestBody(r.Body, &req, false); err != nil {
		writeErr(w, err)
		return
	}
	var opts session.WriteOptions
	if req.PermissionMode != nil {
		if *req.PermissionMode == "" {
			writeErr(w, session.ErrInvalidInput)
			return
		}
		opts.PermissionMode = *req.PermissionMode
	}
	res, err := a.mgr.Write(r.Context(), r.PathValue("id"), req.Payload, opts)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeResp{Session: res.Session, Path: res.Path})
}

func (a *API) switchSession(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
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
func (a *API) snapshotSession(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
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

// recoverSession puts a deleted session back (AC-B4). The id has no record to
// authorize against, so the check runs on the recovered manifest instead.
func (a *API) recoverSession(w http.ResponseWriter, r *http.Request) {
	caller, ok := a.caller(w, r)
	if !ok {
		return
	}
	if err := decodeRequestBody(r.Body, &struct{}{}, false); err != nil {
		writeErr(w, err)
		return
	}
	sess, err := a.mgr.Recover(r.Context(), r.PathValue("id"), func(rec *session.Session) error {
		if !caller.Sees(rec.Tenant) {
			return session.ErrNotFound
		}
		if caller.Tenant != nil && !caller.Tenant.Allows(rec.WorkloadType) {
			return session.ErrForbidden
		}
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// refreshPlugins fetches a codex session's plugin skills again, between
// turns, without ending its conversation (AC-J7).
func (a *API) refreshPlugins(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	if err := decodeRequestBody(r.Body, &struct{}{}, false); err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.mgr.RefreshPlugins(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pluginRefreshResp{
		Session:       res.Session,
		Path:          res.Path,
		PluginChanges: res.Changes,
	})
}

// ---- tenancy ----

// caller resolves the request's caller, answering the request itself when it
// cannot (AC-K1).
func (a *API) caller(w http.ResponseWriter, r *http.Request) (tenancy.Caller, bool) {
	if a.callers == nil {
		return tenancy.OperatorCaller, true
	}
	caller, err := a.callers.Resolve(r)
	if err != nil {
		writeErr(w, err)
		return tenancy.Caller{}, false
	}
	return caller, true
}

// authorizeSession admits a session-id route only for a caller that sees the
// session. Another tenant's session answers exactly like a missing one, so a
// tenant cannot learn which ids exist (AC-K2).
func (a *API) authorizeSession(w http.ResponseWriter, r *http.Request) bool {
	caller, ok := a.caller(w, r)
	if !ok {
		return false
	}
	if caller.Operator {
		return true
	}
	sess, err := a.mgr.Get(r.Context(), r.PathValue("id"))
	if err == nil && !caller.Sees(sess.Tenant) {
		err = session.ErrNotFound
	}
	if err != nil {
		writeErr(w, err)
		return false
	}
	return true
}

// createTenant decides the tenant a new session belongs to (AC-K2, AC-K3). A
// tenant caller always creates into its own tenant; the operator names one or
// none. Either way the owning tenant must be granted the workload type.
func (a *API) createTenant(caller tenancy.Caller, raw json.RawMessage, requested session.WorkloadType) (string, error) {
	var named string
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &named); err != nil || named == "" {
			return "", session.ErrInvalidInput
		}
	}
	owner := caller.Tenant
	if caller.Operator && named != "" {
		t, ok := a.tenants.Lookup(named)
		if !ok {
			return "", session.ErrInvalidInput
		}
		owner = t
	}
	if owner == nil {
		return "", nil
	}
	if named != "" && named != owner.Name {
		return "", session.ErrForbidden
	}
	workload, err := session.NormalizeWorkloadType(requested)
	if err != nil {
		return "", err
	}
	if !owner.Allows(workload) {
		return "", session.ErrForbidden
	}
	return owner.Name, nil
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
	case errors.Is(err, session.ErrUnauthenticated):
		status = http.StatusUnauthorized
	case errors.Is(err, session.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, session.ErrInvalidInput):
		status = http.StatusBadRequest
	case errors.Is(err, session.ErrInvalidState):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, session.ErrConflict), errors.Is(err, session.ErrAlreadyExists):
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
	case errors.Is(err, session.ErrWorkloadBusy):
		status = http.StatusConflict
	case errors.Is(err, session.ErrPluginRefreshFailed):
		status = http.StatusBadGateway
	case errors.Is(err, session.ErrAuthPoolExhausted):
		// The wrapped cause names slots and holders; clients only get the class.
		writeJSON(w, http.StatusServiceUnavailable, errResp{Error: session.ErrAuthPoolExhausted.Error()})
		return
	}
	writeJSON(w, status, errResp{Error: err.Error()})
}
