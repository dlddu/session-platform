package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/monitorpair"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// MonitorPairs is the subset of *monitorpair.Store these handlers use.
type MonitorPairs interface {
	Bind(ctx context.Context, parentSessionID, monitorSessionID, tenant string) (monitorpair.Binding, error)
	For(ctx context.Context, sessionID string) (monitorpair.Binding, monitorpair.Role, error)
	RecordMirror(ctx context.Context, monitorSessionID, payload string) error
	LastMirror(ctx context.Context, monitorSessionID string) (monitorpair.Mirror, error)
	Accept(ctx context.Context, monitorSessionID, requestID, taskID, text, author string) (monitorpair.InboxItem, bool, error)
	Pending(ctx context.Context, monitorSessionID string) ([]monitorpair.InboxItem, error)
	Item(ctx context.Context, monitorSessionID, requestID string) (monitorpair.InboxItem, error)
	MarkForwarded(ctx context.Context, monitorSessionID, requestID string, turnID int64, sessionSeq *int64) (monitorpair.InboxItem, error)
	MarkRefused(ctx context.Context, monitorSessionID, requestID, detail string) (monitorpair.InboxItem, error)
	PublishSnapshot(ctx context.Context, monitorSessionID, kind, key, payload string) error
	Snapshot(ctx context.Context, monitorSessionID, kind, key string) (monitorpair.Mirror, error)
}

// Snapshot kinds the monitor publishes.
const (
	snapshotTasks = "tasks"
	snapshotTurns = "turns"
)

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
	mux.HandleFunc("GET /api/v1/internal/monitor/requests/{requestID}", m.session(m.requestStatus))
	mux.HandleFunc("GET /api/v1/internal/monitor/inbox", m.session(m.inbox))
	mux.HandleFunc("POST /api/v1/internal/monitor/inbox/{requestID}/forwarded", m.session(m.markForwarded))
	mux.HandleFunc("POST /api/v1/internal/monitor/inbox/{requestID}/refused", m.session(m.markRefused))
	mux.HandleFunc("POST /api/v1/internal/monitor/work/snapshot", m.session(m.publishSnapshot))
}

var errMonitorDisabled = errors.New("the managed monitor pair is not enabled on this deployment")

type callerKey struct{}

// boundKey carries the binding the request was resolved to, so the handlers
// are the same whether the caller proved itself as a session (the workload
// routes) or as the tenant that owns the parent session (the backend routes).
type boundKey struct{}

type boundPair struct {
	binding monitorpair.Binding
	role    monitorpair.Role
}

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
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, id))
		// A session with no binding still reaches the handlers: claiming the
		// pair is how one comes to exist. Everything that needs a binding
		// calls resolve, which refuses when the middleware found none.
		b, role, err := m.pairs.For(r.Context(), id)
		switch {
		case err == nil:
			r = r.WithContext(context.WithValue(r.Context(), boundKey{}, boundPair{binding: b, role: role}))
		case errors.Is(err, monitorpair.ErrNotFound), errors.Is(err, monitorpair.ErrInvalidID):
		default:
			m.logger.Error("monitor pair lookup failed", "err", err)
			http.Error(w, "pair lookup failed", http.StatusBadGateway)
			return
		}
		next(w, r)
	}
}

func caller(r *http.Request) string {
	id, _ := r.Context().Value(callerKey{}).(string)
	return id
}

// resolve returns the binding the middleware put on the request. Both route
// sets resolve it before the handler runs, each from an identity it proved,
// so no handler ever takes a session id from a body, a query or a header.
func (m *Monitor) resolve(w http.ResponseWriter, r *http.Request) (monitorpair.Binding, monitorpair.Role, bool) {
	bound, ok := r.Context().Value(boundKey{}).(boundPair)
	if !ok {
		http.Error(w, "this session is not part of a managed monitor pair", http.StatusForbidden)
		return monitorpair.Binding{}, "", false
	}
	return bound.binding, bound.role, true
}

// TenantGuard is the check the backend routes run first: it is the control
// plane's existing session authorization, which loads `{id}` and refuses a
// caller whose tenant does not own it. The guard is injected rather than
// re-implemented so there is one ownership rule, not two.
type TenantGuard func(w http.ResponseWriter, r *http.Request) bool

// BackendRoutes serves the same pair surface to a backend that calls on a
// person's behalf — PocketAide's server, not a session. Identity is the
// caller's own tenant token, `{id}` is the **parent** session it owns, and the
// monitor comes from the binding: the caller never names a monitor, and no
// header is trusted for either side.
//
// It exists because the published read model and the durable inbox live behind
// the workload-token routes, which a backend cannot use — and must not, since
// that would mean handing a workload's token to something outside the pod.
func (m *Monitor) BackendRoutes(mux *http.ServeMux, guard TenantGuard) {
	backend := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if m.pairs == nil {
				http.Error(w, errMonitorDisabled.Error(), http.StatusServiceUnavailable)
				return
			}
			if guard != nil && !guard(w, r) {
				return
			}
			parentID := r.PathValue("id")
			b, role, err := m.pairs.For(r.Context(), parentID)
			if err != nil {
				if errors.Is(err, monitorpair.ErrNotFound) || errors.Is(err, monitorpair.ErrInvalidID) {
					http.Error(w, "this session has no monitor", http.StatusNotFound)
					return
				}
				m.logger.Error("monitor pair lookup failed", "err", err)
				http.Error(w, "pair lookup failed", http.StatusBadGateway)
				return
			}
			if role != monitorpair.RoleParent || b.ParentSessionID != parentID {
				// The path must name the parent. Naming the monitor here would
				// let a caller read a pair from the wrong side.
				http.Error(w, "this session has no monitor", http.StatusNotFound)
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), boundKey{}, boundPair{binding: b, role: role})))
		}
	}
	mux.HandleFunc("GET /api/v1/sessions/{id}/monitor/pair", backend(m.showPair))
	mux.HandleFunc("GET /api/v1/sessions/{id}/monitor/work/tasks", backend(m.tasks))
	mux.HandleFunc("GET /api/v1/sessions/{id}/monitor/work/tasks/{taskID}/turns", backend(m.turns))
	mux.HandleFunc("POST /api/v1/sessions/{id}/monitor/work/tasks/{taskID}/requests", backend(m.request))
	mux.HandleFunc("GET /api/v1/sessions/{id}/monitor/requests/{requestID}", backend(m.requestStatus))
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

// bindPair is called by the monitor, never by the parent: the side that does
// the work claims the pair. It takes **no parent from the request** — the
// parent is the one the control plane recorded on this session when it was
// created (`monitorFor`). That is what stops a workload from binding an
// arbitrary other parent's slot: asking cannot change the record, and the
// record was checked against the tenant at create time.
//
// Re-calling it after a restart returns the same binding, so a monitor that
// comes back does not need to remember anything.
func (m *Monitor) bindPair(w http.ResponseWriter, r *http.Request) {
	monitorID := caller(r)
	mon, err := m.mgr.Get(r.Context(), monitorID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(mon.MonitorFor) == "" {
		http.Error(w, "this session was not created as a monitor for a parent session", http.StatusForbidden)
		return
	}
	parent, err := m.mgr.Get(r.Context(), mon.MonitorFor)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Belt and braces: the tenant was checked when the pair was recorded, and
	// it is checked again here in case either record moved tenants since.
	if parent.Tenant != mon.Tenant {
		http.Error(w, "parent and monitor belong to different tenants", http.StatusForbidden)
		return
	}
	b, err := m.pairs.Bind(r.Context(), parent.ID, monitorID, mon.Tenant)
	if err != nil {
		switch {
		case errors.Is(err, monitorpair.ErrBound):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, monitorpair.ErrInvalidID):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			m.logger.Error("monitor pair bind failed", "err", err)
			http.Error(w, "bind failed", http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, http.StatusOK, pairView{Role: monitorpair.RoleMonitor, Binding: b,
		Monitor: m.monitorState(r.Context(), b)})
}

type tasksView struct {
	session.WorkTaskPage
	Monitor *monitorStateView `json:"monitor,omitempty"`
	// Source is "slot" for a live read of the monitor's own records and
	// "mirror" for the last read the control plane managed to take. AsOf is
	// set only for a mirror, so a reader always knows how old the answer is
	// and can never mistake stored records for live ones.
	Source string     `json:"source"`
	AsOf   *time.Time `json:"asOf,omitempty"`
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
		// A monitor that is frozen, stopped or gone has no pod to proxy to.
		// The parent still needs its list, so the last read the control plane
		// took is served — labelled, timestamped, and only to this binding.
		if errors.Is(err, session.ErrWorkUnavailable) {
			// The monitor's own published state comes first: it is current as
			// of its last commit, and it exists even for work this parent has
			// never read. The read-through mirror is only the fallback for a
			// deployment whose monitor has not published yet.
			if pub, pErr := m.pairs.Snapshot(r.Context(), b.MonitorSessionID, snapshotTasks, ""); pErr == nil {
				var published session.WorkTaskPage
				if json.Unmarshal([]byte(pub.Payload), &published) == nil {
					at := pub.MirroredAt
					writeJSON(w, http.StatusOK, tasksView{WorkTaskPage: published,
						Monitor: m.monitorState(r.Context(), b), Source: "published", AsOf: &at})
					return
				}
			}
			if stored, mErr := m.pairs.LastMirror(r.Context(), b.MonitorSessionID); mErr == nil {
				var mirrored session.WorkTaskPage
				if json.Unmarshal([]byte(stored.Payload), &mirrored) == nil {
					at := stored.MirroredAt
					writeJSON(w, http.StatusOK, tasksView{WorkTaskPage: mirrored,
						Monitor: m.monitorState(r.Context(), b), Source: "mirror", AsOf: &at})
					return
				}
			}
		}
		m.workErr(w, r, b, err)
		return
	}
	// Keep the mirror current from the live read. This is the only writer of
	// the mirror, and it never writes back to the slot.
	if raw, mErr := json.Marshal(page); mErr == nil {
		if err := m.pairs.RecordMirror(r.Context(), b.MonitorSessionID, string(raw)); err != nil {
			m.logger.Warn("could not mirror the monitor's work records", "monitor", b.MonitorSessionID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, tasksView{WorkTaskPage: page,
		Monitor: m.monitorState(r.Context(), b), Source: "slot"})
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
	taskID := r.PathValue("taskID")
	page, err := m.mgr.WorkTurns(r.Context(), b.MonitorSessionID, taskID,
		afterTurnID, limit, q.Get("open") == "true")
	if err != nil {
		if errors.Is(err, session.ErrWorkUnavailable) {
			if pub, pErr := m.pairs.Snapshot(r.Context(), b.MonitorSessionID, snapshotTurns, taskID); pErr == nil {
				var published session.WorkTurnPage
				if json.Unmarshal([]byte(pub.Payload), &published) == nil {
					at := pub.MirroredAt
					writeJSON(w, http.StatusOK, turnsView{WorkTurnPage: published,
						Monitor: m.monitorState(r.Context(), b), Source: "published", AsOf: &at})
					return
				}
			}
		}
		m.workErr(w, r, b, err)
		return
	}
	writeJSON(w, http.StatusOK, turnsView{WorkTurnPage: page,
		Monitor: m.monitorState(r.Context(), b), Source: "slot"})
}

type turnsView struct {
	session.WorkTurnPage
	Monitor *monitorStateView `json:"monitor,omitempty"`
	Source  string            `json:"source"`
	AsOf    *time.Time        `json:"asOf,omitempty"`
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

// request is durable intake, then delivery — in that order, always.
//
// The instruction is stored here first, under the parent's stable request id.
// Only then is the monitor's slot asked to take it as a turn, and the turn id
// that comes back is written beside the stored item. That order is what makes
// a lost response safe: the retry finds the stored item (receipt), and the
// monitor finds "accepted but not forwarded" rather than guessing.
//
// A monitor that is asleep or frozen cannot take a turn, so the item stays
// accepted and the monitor drains it when it comes back. Nothing is dropped
// and nothing is reported as run that did not run.
//
// Only the parent may call it: the monitor writes its own records directly.
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
	requestID := strings.TrimSpace(body.ID)
	taskID := r.PathValue("taskID")
	item, created, err := m.pairs.Accept(r.Context(), b.MonitorSessionID, requestID, taskID, body.Text, b.ParentSessionID)
	if err != nil {
		switch {
		case errors.Is(err, monitorpair.ErrRequestReused):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, monitorpair.ErrInvalidID), errors.Is(err, monitorpair.ErrInvalidBody):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			m.logger.Error("monitor intake failed", "err", err)
			http.Error(w, "intake failed", http.StatusBadGateway)
		}
		return
	}
	// Already forwarded: the receipt is the answer, and the slot is not asked
	// again. This is the path a reconnecting client takes.
	if item.State == monitorpair.InboxForwarded {
		writeJSON(w, http.StatusOK, intakeView{Item: item, Replayed: true,
			Monitor: m.monitorState(r.Context(), b)})
		return
	}
	stored, err := m.mgr.CreateWorkRequest(r.Context(), b.MonitorSessionID, session.WorkRequestInput{
		ID:           requestID,
		TaskID:       taskID,
		Text:         body.Text,
		Author:       b.ParentSessionID,
		ChoiceTurnID: body.ChoiceTurnID,
	})
	if err != nil {
		if errors.Is(err, session.ErrWorkUnavailable) {
			// Durably accepted, not delivered. The monitor drains it on resume.
			writeJSON(w, http.StatusAccepted, intakeView{Item: item, Replayed: !created,
				Monitor: m.monitorState(r.Context(), b),
				Note:    "accepted and held: the monitor is not running, and it will take this from its inbox when it resumes"})
			return
		}
		if errors.Is(err, session.ErrWorkChoiceStale) || errors.Is(err, session.ErrInvalidInput) {
			if refused, rErr := m.pairs.MarkRefused(r.Context(), b.MonitorSessionID, requestID, err.Error()); rErr == nil {
				writeJSON(w, http.StatusConflict, intakeView{Item: refused, Monitor: m.monitorState(r.Context(), b)})
				return
			}
		}
		m.workErr(w, r, b, err)
		return
	}
	marked, err := m.pairs.MarkForwarded(r.Context(), b.MonitorSessionID, requestID, stored.Turn.ID, stored.Turn.SessionSeq)
	if err != nil {
		// The slot has it; the bookkeeping did not land. Say so rather than
		// claim either outcome: the next drain reconciles from the slot.
		m.logger.Warn("forwarded but not recorded", "request", requestID, "err", err)
		writeJSON(w, http.StatusOK, intakeView{Item: item, Replayed: stored.Replayed, Turn: &stored.Turn,
			Monitor: m.monitorState(r.Context(), b),
			Note:    "the monitor's slot took this turn but the inbox record did not update; the next drain reconciles it"})
		return
	}
	writeJSON(w, http.StatusOK, intakeView{Item: marked, Replayed: stored.Replayed, Turn: &stored.Turn,
		Monitor: m.monitorState(r.Context(), b)})
}

type intakeView struct {
	Item monitorpair.InboxItem `json:"item"`
	// Replayed is the existing receipt: true when this instruction was already
	// stored or already a turn, so a retry is never a second delegation.
	Replayed bool              `json:"replayed"`
	Turn     *session.WorkTurn `json:"turn,omitempty"`
	Monitor  *monitorStateView `json:"monitor,omitempty"`
	Note     string            `json:"note,omitempty"`
}

// requestStatus is the parent asking what happened to one instruction. It is a
// read: it exposes state and history only and never runs or re-runs anything.
func (m *Monitor) requestStatus(w http.ResponseWriter, r *http.Request) {
	b, _, ok := m.resolve(w, r)
	if !ok {
		return
	}
	item, err := m.pairs.Item(r.Context(), b.MonitorSessionID, r.PathValue("requestID"))
	if err != nil {
		if errors.Is(err, monitorpair.ErrNotFound) {
			http.Error(w, "no such request for this pair", http.StatusNotFound)
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, intakeView{Item: item, Monitor: m.monitorState(r.Context(), b)})
}

// inbox is the monitor draining what the parent sent while it was away.
// Reading it claims nothing; the monitor claims an item by forwarding it.
func (m *Monitor) inbox(w http.ResponseWriter, r *http.Request) {
	b, role, ok := m.resolve(w, r)
	if !ok {
		return
	}
	if role != monitorpair.RoleMonitor {
		http.Error(w, "only the monitor drains its own inbox", http.StatusForbidden)
		return
	}
	items, err := m.pairs.Pending(r.Context(), b.MonitorSessionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if items == nil {
		items = []monitorpair.InboxItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": items})
}

type forwardedReq struct {
	// TurnID is the turn the slot made for this instruction — the existing
	// handle, not a new identifier.
	TurnID     int64  `json:"turnId"`
	SessionSeq *int64 `json:"sessionSeq,omitempty"`
}

// markForwarded is the monitor saying "this is now turn N in my slot". It is
// idempotent on the same turn, so a monitor that died between the slot write
// and this call repeats it safely; a different turn for the same request is
// refused, because that would be a second run.
func (m *Monitor) markForwarded(w http.ResponseWriter, r *http.Request) {
	b, role, ok := m.resolve(w, r)
	if !ok {
		return
	}
	if role != monitorpair.RoleMonitor {
		http.Error(w, "only the monitor records its own delegation", http.StatusForbidden)
		return
	}
	var body forwardedReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.TurnID <= 0 {
		http.Error(w, "body must be JSON with a positive turnId", http.StatusBadRequest)
		return
	}
	item, err := m.pairs.MarkForwarded(r.Context(), b.MonitorSessionID, r.PathValue("requestID"), body.TurnID, body.SessionSeq)
	if err != nil {
		switch {
		case errors.Is(err, monitorpair.ErrNotFound):
			http.Error(w, "no such request for this pair", http.StatusNotFound)
		case errors.Is(err, monitorpair.ErrRequestReused):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			writeErr(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, intakeView{Item: item})
}

type refusedReq struct {
	Detail string `json:"detail"`
}

func (m *Monitor) markRefused(w http.ResponseWriter, r *http.Request) {
	b, role, ok := m.resolve(w, r)
	if !ok {
		return
	}
	if role != monitorpair.RoleMonitor {
		http.Error(w, "only the monitor refuses its own inbox items", http.StatusForbidden)
		return
	}
	var body refusedReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || strings.TrimSpace(body.Detail) == "" {
		http.Error(w, "body must be JSON with a detail saying why", http.StatusBadRequest)
		return
	}
	item, err := m.pairs.MarkRefused(r.Context(), b.MonitorSessionID, r.PathValue("requestID"), body.Detail)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, intakeView{Item: item})
}

type snapshotReq struct {
	// Kind is "tasks" for the page or "turns" for one task's history, and Key
	// is the task id for turns. Payload is the committed view as the slot
	// serves it, so the parent reads exactly the same shape either way.
	Kind    string          `json:"kind"`
	Key     string          `json:"key"`
	Payload json.RawMessage `json:"payload"`
}

// publishSnapshot is the monitor pushing its own committed state after it
// registers, changes or finishes work. That is what lets the parent read the
// latest stored list, detail and history for work it never looked at, and
// while the monitor is asleep.
func (m *Monitor) publishSnapshot(w http.ResponseWriter, r *http.Request) {
	b, role, ok := m.resolve(w, r)
	if !ok {
		return
	}
	if role != monitorpair.RoleMonitor {
		http.Error(w, "only the monitor publishes its own state", http.StatusForbidden)
		return
	}
	var body snapshotReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "body must be JSON with kind, key and payload", http.StatusBadRequest)
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if kind != snapshotTasks && kind != snapshotTurns {
		http.Error(w, `kind must be "tasks" or "turns"`, http.StatusBadRequest)
		return
	}
	if kind == snapshotTurns && strings.TrimSpace(body.Key) == "" {
		http.Error(w, `"turns" needs the task id as key`, http.StatusBadRequest)
		return
	}
	if len(body.Payload) == 0 {
		http.Error(w, "payload is required", http.StatusBadRequest)
		return
	}
	if err := m.pairs.PublishSnapshot(r.Context(), b.MonitorSessionID, kind, strings.TrimSpace(body.Key), string(body.Payload)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"published": kind, "key": body.Key})
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
