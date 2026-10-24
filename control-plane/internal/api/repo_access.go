package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/repoaccess"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// RepoAccessStore is the subset of *repoaccess.Store the handlers use.
type RepoAccessStore interface {
	List(ctx context.Context) ([]repoaccess.Entry, error)
	Allowed(ctx context.Context, repo string) (bool, error)
	Add(ctx context.Context, repo, by string) (repoaccess.Entry, bool, error)
	Remove(ctx context.Context, repo string) (bool, error)
	Requests(ctx context.Context) ([]repoaccess.Request, error)
	Request(ctx context.Context, repo, sessionID, reason string) (repoaccess.Request, string, error)
	Approve(ctx context.Context, id, by string) (repoaccess.Entry, error)
	Reject(ctx context.Context, id string) error
}

type HelperVerifier interface {
	SessionID(r *http.Request) (string, error)
}

// RepoAccess serves docs/prd/repo-access.md: the management routes (AC-H2)
// and the helper-pod routes (AC-H3, behind TokenReview). The management routes
// carry no sign-in of their own — like the session APIs, they sit behind the
// deployment's Authelia forward-auth at the ingress. A nil store means the
// feature is off (AC-H1) and every route answers 503.
type RepoAccess struct {
	store    RepoAccessStore
	helpers  HelperVerifier
	sessions session.Manager
	logger   *slog.Logger
}

func NewRepoAccess(store RepoAccessStore, helpers HelperVerifier, sessions session.Manager, logger *slog.Logger) *RepoAccess {
	if logger == nil {
		logger = slog.Default()
	}
	return &RepoAccess{store: store, helpers: helpers, sessions: sessions, logger: logger}
}

func (ra *RepoAccess) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/repo-access", ra.admin(ra.overview))
	mux.HandleFunc("POST /api/v1/repo-access/allowlist", ra.admin(ra.add))
	mux.HandleFunc("DELETE /api/v1/repo-access/allowlist", ra.admin(ra.remove))
	mux.HandleFunc("POST /api/v1/repo-access/requests/{id}/approve", ra.admin(ra.approve))
	mux.HandleFunc("POST /api/v1/repo-access/requests/{id}/reject", ra.admin(ra.reject))
	mux.HandleFunc("GET /api/v1/internal/repo-access/check", ra.helper(ra.check))
	mux.HandleFunc("POST /api/v1/internal/repo-access/requests", ra.helper(ra.request))
}

var errRepoAccessDisabled = errors.New("repo access is not enabled on this deployment")

func (ra *RepoAccess) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ra.store == nil {
			writeJSON(w, http.StatusServiceUnavailable, errResp{Error: errRepoAccessDisabled.Error()})
			return
		}
		h(w, r)
	}
}

// operatorHeader is the user name Authelia's forward-auth hands upstream when
// the ingress middleware forwards it. It only labels who changed the list —
// it is never a gate, since anything inside the cluster can set it.
const operatorHeader = "Remote-User"

func operator(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(operatorHeader))
}

type helperCtxKey struct{}

func (ra *RepoAccess) helper(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ra.store == nil || ra.helpers == nil {
			writeJSON(w, http.StatusServiceUnavailable, errResp{Error: errRepoAccessDisabled.Error()})
			return
		}
		id, err := ra.helpers.SessionID(r)
		if err != nil {
			if !errors.Is(err, helperauth.ErrUnauthenticated) {
				ra.logger.Error("repo access: helper token review failed", "err", err)
			}
			writeJSON(w, http.StatusUnauthorized, errResp{Error: "unauthenticated"})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), helperCtxKey{}, id)))
	}
}

type requestView struct {
	repoaccess.Request
	SessionName  string `json:"sessionName,omitempty"`
	WorkloadType string `json:"workloadType,omitempty"`
}

type overviewResp struct {
	Allowlist []repoaccess.Entry `json:"allowlist"`
	Requests  []requestView      `json:"requests"`
}

func (ra *RepoAccess) overview(w http.ResponseWriter, r *http.Request) {
	list, err := ra.store.List(r.Context())
	if err != nil {
		ra.internal(w, "list allow list", err)
		return
	}
	reqs, err := ra.store.Requests(r.Context())
	if err != nil {
		ra.internal(w, "list requests", err)
		return
	}
	names := ra.sessionLabels(r.Context())
	views := make([]requestView, 0, len(reqs))
	for _, q := range reqs {
		v := requestView{Request: q}
		if s, ok := names[q.SessionID]; ok {
			v.SessionName, v.WorkloadType = s.Name, string(s.WorkloadType)
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, overviewResp{Allowlist: list, Requests: views})
}

// sessionLabels is best effort: a request outlives its session, and a failed
// listing only costs the screen its friendly names.
func (ra *RepoAccess) sessionLabels(ctx context.Context) map[string]*session.Session {
	out := map[string]*session.Session{}
	if ra.sessions == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	list, err := ra.sessions.List(ctx)
	if err != nil {
		ra.logger.Warn("repo access: session names unavailable", "err", err)
		return out
	}
	for _, s := range list {
		out[s.ID] = s
	}
	return out
}

type repoReq struct {
	Repo string `json:"repo"`
}

func (ra *RepoAccess) add(w http.ResponseWriter, r *http.Request) {
	var body repoReq
	if err := decodeRequestBody(r.Body, &body, true); err != nil {
		writeErr(w, err)
		return
	}
	by := operator(r)
	e, created, err := ra.store.Add(r.Context(), body.Repo, by)
	if errors.Is(err, repoaccess.ErrInvalidRepo) {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err != nil {
		ra.internal(w, "add", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		ra.logger.Info("repo access: added", "repo", e.Repo, "by", by)
	}
	writeJSON(w, status, e)
}

func (ra *RepoAccess) remove(w http.ResponseWriter, r *http.Request) {
	removed, err := ra.store.Remove(r.Context(), r.URL.Query().Get("repo"))
	if errors.Is(err, repoaccess.ErrInvalidRepo) {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err != nil {
		ra.internal(w, "remove", err)
		return
	}
	if removed {
		ra.logger.Info("repo access: removed", "repo", r.URL.Query().Get("repo"), "by", operator(r))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (ra *RepoAccess) approve(w http.ResponseWriter, r *http.Request) {
	by := operator(r)
	e, err := ra.store.Approve(r.Context(), r.PathValue("id"), by)
	if errors.Is(err, repoaccess.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errResp{Error: "request not found"})
		return
	}
	if err != nil {
		ra.internal(w, "approve", err)
		return
	}
	ra.logger.Info("repo access: request approved", "repo", e.Repo, "by", by)
	writeJSON(w, http.StatusOK, e)
}

func (ra *RepoAccess) reject(w http.ResponseWriter, r *http.Request) {
	err := ra.store.Reject(r.Context(), r.PathValue("id"))
	if errors.Is(err, repoaccess.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errResp{Error: "request not found"})
		return
	}
	if err != nil {
		ra.internal(w, "reject", err)
		return
	}
	ra.logger.Info("repo access: request rejected", "id", r.PathValue("id"), "by", operator(r))
	w.WriteHeader(http.StatusNoContent)
}

type checkResp struct {
	Repo    string `json:"repo"`
	Allowed bool   `json:"allowed"`
}

func (ra *RepoAccess) check(w http.ResponseWriter, r *http.Request) {
	repo, err := repoaccess.NormalizeRepo(r.URL.Query().Get("repo"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	ok, err := ra.store.Allowed(r.Context(), repo)
	if err != nil {
		ra.internal(w, "check", err)
		return
	}
	writeJSON(w, http.StatusOK, checkResp{Repo: repo, Allowed: ok})
}

type accessReq struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

type accessResp struct {
	Repo      string `json:"repo"`
	Status    string `json:"status"`
	RequestID string `json:"requestId,omitempty"`
}

func (ra *RepoAccess) request(w http.ResponseWriter, r *http.Request) {
	var body accessReq
	if err := decodeRequestBody(r.Body, &body, true); err != nil {
		writeErr(w, err)
		return
	}
	sessionID, _ := r.Context().Value(helperCtxKey{}).(string)
	q, status, err := ra.store.Request(r.Context(), body.Repo, sessionID, body.Reason)
	if errors.Is(err, repoaccess.ErrInvalidRepo) || errors.Is(err, repoaccess.ErrInvalidReason) {
		writeJSON(w, http.StatusBadRequest, errResp{Error: err.Error()})
		return
	}
	if err != nil {
		ra.internal(w, "request", err)
		return
	}
	if status == repoaccess.StatusPending {
		ra.logger.Info("repo access: requested", "repo", q.Repo, "session", sessionID, "request", q.ID)
	}
	writeJSON(w, http.StatusOK, accessResp{Repo: q.Repo, Status: status, RequestID: q.ID})
}

func (ra *RepoAccess) internal(w http.ResponseWriter, op string, err error) {
	ra.logger.Error("repo access: "+op, "err", err)
	writeJSON(w, http.StatusInternalServerError, errResp{Error: "repo access store error"})
}
