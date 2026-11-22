package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/monitorpair"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// MonitorPairs is the subset of *monitorpair.Store these handlers use.
type MonitorPairs interface {
	Bind(ctx context.Context, parentSessionID, monitorSessionID, tenant string) (monitorpair.Binding, error)
	For(ctx context.Context, sessionID string) (monitorpair.Binding, monitorpair.Role, error)
}

// SessionIdentity proves which session a request comes from. It is the same
// shape repo access already uses for its helper pods (AC-H3): a projected
// token for one audience, TokenReview, and the bound pod's session label.
type SessionIdentity interface {
	SessionID(r *http.Request) (string, error)
}

// Monitor serves the separated-monitor routes: the parent (manager/chat)
// session reads the work records its bound monitor owns, and sends
// instructions into the monitor's request inbox. It owns no records of its
// own — every read and every write goes to the monitor's slot through
// session.Manager, so the monitor's slot stays the single write origin for
// tasks and turns (docs/prd/manager-monitor.md).
//
// The caller never names a session. Identity is proved, the binding resolves
// the monitor, and the handlers address that monitor only; a body or query
// naming another session is ignored, which is what blocks reaching another
// slot.
type Monitor struct {
	pairs    MonitorPairs
	identity SessionIdentity
	mgr      session.Manager
	logger   *slog.Logger
}

func NewMonitor(pairs MonitorPairs, identity SessionIdentity, mgr session.Manager, logger *slog.Logger) *Monitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{pairs: pairs, identity: identity, mgr: mgr, logger: logger}
}

func (m *Monitor) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/internal/monitor/pair", m.session(m.showPair))
	mux.HandleFunc("POST /api/v1/internal/monitor/pair", m.session(m.bindPair))
	mux.HandleFunc("GET /api/v1/internal/monitor/work/tasks", m.session(m.tasks))
	mux.HandleFunc("GET /api/v1/internal/monitor/work/tasks/{taskID}/turns", m.session(m.turns))
	mux.HandleFunc("POST /api/v1/internal/monitor/work/tasks/{taskID}/requests", m.session(m.request))
}

var errMonitorDisabled = errors.New("the managed monitor pair is not enabled on this deployment")

type callerKey struct{}

// session proves the caller and puts its session ID in the context. A
// deployment without the store or the verifier answers 503 rather than
// falling back to an unauthenticated route.
func (m *Monitor) session(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.pairs == nil || m.identity == nil {
			http.Error(w, errMonitorDisabled.Error(), http.StatusServiceUnavailable)
			return
		}
		id, err := m.identity.SessionID(r)
		if err != nil {
			if errors.Is(err, helperauth.ErrUnauthenticated) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="session-platform"`)
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			m.logger.Error("monitor identity check failed", "err", err)
			http.Error(w, "identity check failed", http.StatusBadGateway)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, id)))
	}
}

func caller(r *http.Request) string {
	id, _ := r.Context().Value(callerKey{}).(string)
	return id
}

// resolve turns the proved caller into its binding and role. Anything else is
// 403: a session outside a binding has no work records to reach here.
func (m *Monitor) resolve(w http.ResponseWriter, r *http.Request) (monitorpair.Binding, monitorpair.Role, bool) {
	b, role, err := m.pairs.For(r.Context(), caller(r))
	if err != nil {
		if errors.Is(err, monitorpair.ErrNotFound) || errors.Is(err, monitorpair.ErrInvalidID) {
			http.Error(w, "this session is not part of a managed monitor pair", http.StatusForbidden)
			return monitorpair.Binding{}, "", false
		}
		m.logger.Error("monitor pair lookup failed", "err", err)
		http.Error(w, "pair lookup failed", http.StatusBadGateway)
		return monitorpair.Binding{}, "", false
	}
	return b, role, true
}

type pairView struct {
	Role    monitorpair.Role    `json:"role"`
	Binding monitorpair.Binding `json:"binding"`
	// Monitor is how the monitor session looks right now, so the parent can
	// tell "nothing to report" from "the monitor is not running". It is read
	// from the session record, which needs no live pod.
	Monitor *monitorStateView `json:"monitor,omitempty"`
}

type monitorStateView struct {
	SessionID string `json:"sessionId"`
	State     string `json:"state"`
	// Reads and Intake say what works in this state, and Recovery says what
	// would change it. Both reads and intake go through the monitor's slot, so
	// a stopped monitor refuses both — saying so is the point.
	Reads    string `json:"reads"`
	Intake   string `json:"intake"`
	Recovery string `json:"recovery,omitempty"`
}

func (m *Monitor) monitorState(ctx context.Context, b monitorpair.Binding) *monitorStateView {
	s, err := m.mgr.Get(ctx, b.MonitorSessionID)
	if err != nil {
		return &monitorStateView{SessionID: b.MonitorSessionID, State: "unknown",
			Reads: "unavailable", Intake: "refused",
			Recovery: "the monitor session record could not be read; check the session list"}
	}
	v := &monitorStateView{SessionID: b.MonitorSessionID, State: string(s.State)}
	switch s.State {
	case session.StateActive, session.StateIdle:
		v.Reads, v.Intake = "available", "available"
	default:
		v.Reads, v.Intake = "unavailable", "refused"
		v.Recovery = "restore the monitor session (POST /api/v1/sessions/" + b.MonitorSessionID + "/switch) before reading or sending"
	}
	return v
}

func (m *Monitor) showPair(w http.ResponseWriter, r *http.Request) {
	b, role, ok := m.resolve(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pairView{Role: role, Binding: b, Monitor: m.monitorState(r.Context(), b)})
}

type bindPairReq struct {
	// ParentSessionID is the session this monitor serves. The monitor proves
	// its own identity, so only the other side is named here, and both must be
	// sessions of the same tenant.
	ParentSessionID string `json:"parentSessionId"`
}

// bindPair is called by the monitor, never by the parent: the side that does
// the work claims the pair. Re-calling it after a restart returns the same
// binding.
func (m *Monitor) bindPair(w http.ResponseWriter, r *http.Request) {
	monitorID := caller(r)
	var body bindPairReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "body must be JSON with parentSessionId", http.StatusBadRequest)
		return
	}
	parentID := strings.TrimSpace(body.ParentSessionID)
	if parentID == "" {
		http.Error(w, "parentSessionId is required", http.StatusBadRequest)
		return
	}
	// The tenant comes from the session records, not from the caller: a
	// binding may not bridge two tenants, and a monitor may not claim a parent
	// outside its own.
	parent, err := m.mgr.Get(r.Context(), parentID)
	if err != nil {
		writeErr(w, err)
		return
	}
	mon, err := m.mgr.Get(r.Context(), monitorID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if parent.Tenant != mon.Tenant {
		http.Error(w, "parent and monitor belong to different tenants", http.StatusForbidden)
		return
	}
	b, err := m.pairs.Bind(r.Context(), parentID, monitorID, mon.Tenant)
	if err != nil {
		if errors.Is(err, monitorpair.ErrBound) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.Is(err, monitorpair.ErrInvalidID) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.logger.Error("monitor pair bind failed", "err", err)
		http.Error(w, "bind failed", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, pairView{Role: monitorpair.RoleMonitor, Binding: b,
		Monitor: m.monitorState(r.Context(), b)})
}

type tasksView struct {
	session.WorkTaskPage
	Monitor *monitorStateView `json:"monitor,omitempty"`
}

// tasks is the parent's window on the work: one keyset page of the monitor's
// tasks with each task's latest turn. It is a read — it does not wake the
// monitor, claim anything, acknowledge anything or move last access, because
// it only calls the manager's read path for the monitor's session.
func (m *Monitor) tasks(w http.ResponseWriter, r *http.Request) {
	b, _, ok := m.resolve(w, r)
	if !ok {
		return
	}
	limit, err := workLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	page, err := m.mgr.WorkTasks(r.Context(), b.MonitorSessionID, limit,
		strings.TrimSpace(q.Get("after_id")), strings.TrimSpace(q.Get("status")), "")
	if err != nil {
		m.workErr(w, r, b, err)
		return
	}
	writeJSON(w, http.StatusOK, tasksView{WorkTaskPage: page, Monitor: m.monitorState(r.Context(), b)})
}

func (m *Monitor) turns(w http.ResponseWriter, r *http.Request) {
	b, _, ok := m.resolve(w, r)
	if !ok {
		return
	}
	limit, err := workLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	var afterTurnID int64
	if raw := strings.TrimSpace(q.Get("after_turn_id")); raw != "" {
		afterTurnID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || afterTurnID < 0 {
			http.Error(w, "after_turn_id must be a non-negative integer", http.StatusBadRequest)
			return
		}
	}
	page, err := m.mgr.WorkTurns(r.Context(), b.MonitorSessionID, r.PathValue("taskID"),
		afterTurnID, limit, q.Get("open") == "true")
	if err != nil {
		m.workErr(w, r, b, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

type monitorRequestReq struct {
	// ID is the caller's request id. It is the existing idempotency key of the
	// work request path: the same id with the same text replays the stored
	// turn instead of delegating the work twice.
	ID           string `json:"id"`
	Text         string `json:"text"`
	ChoiceTurnID *int64 `json:"choice_turn_id,omitempty"`
}

type monitorRequestView struct {
	session.WorkRequestStored
	Monitor *monitorStateView `json:"monitor,omitempty"`
}

// request puts one instruction in the monitor's inbox. Only the parent may
// call it: the monitor writes its own records directly, and letting the parent
// in here is the whole point of the separation — the parent asks, the monitor
// decides and records.
//
// There is deliberately no route that lets the parent create or change a task
// or a turn. Intake is the only write it has, and the reply says plainly
// whether this was a fresh intake or a replay of one already accepted.
func (m *Monitor) request(w http.ResponseWriter, r *http.Request) {
	b, role, ok := m.resolve(w, r)
	if !ok {
		return
	}
	if role != monitorpair.RoleParent {
		http.Error(w, "only the parent session sends instructions; the monitor writes its own records", http.StatusForbidden)
		return
	}
	var body monitorRequestReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "body must be JSON with id and text", http.StatusBadRequest)
		return
	}
	stored, err := m.mgr.CreateWorkRequest(r.Context(), b.MonitorSessionID, session.WorkRequestInput{
		ID:           strings.TrimSpace(body.ID),
		TaskID:       r.PathValue("taskID"),
		Text:         body.Text,
		Author:       b.ParentSessionID,
		ChoiceTurnID: body.ChoiceTurnID,
	})
	if err != nil {
		m.workErr(w, r, b, err)
		return
	}
	writeJSON(w, http.StatusOK, monitorRequestView{WorkRequestStored: stored, Monitor: m.monitorState(r.Context(), b)})
}

// workErr keeps the "the monitor is not running" case distinguishable from a
// bad request: the body carries what reads and intake can do right now and
// what would restore them, instead of a bare 503.
func (m *Monitor) workErr(w http.ResponseWriter, r *http.Request, b monitorpair.Binding, err error) {
	if errors.Is(err, session.ErrWorkUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   err.Error(),
			"monitor": m.monitorState(r.Context(), b),
		})
		return
	}
	writeErr(w, err)
}
