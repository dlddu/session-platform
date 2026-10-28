package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/repoaccess"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// repoAccessDBPath is where the allow list lives. It is not configurable: the
// deployment that turns the feature on mounts a persistent volume at its
// directory, and a missing mount fails startup instead of silently keeping the
// list on the container's filesystem.
const repoAccessDBPath = "/var/lib/session-platform/repo-access/repo-access.db"

// repoAccessConfig is docs/prd/repo-access.md's opt-in (AC-H1). Off unless
// REPO_ACCESS_ENABLED. The management screen has no sign-in of its own: the
// deployment's ingress (Authelia forward-auth) is its door, as for every other
// screen (AC-H2).
type repoAccessConfig struct {
	enabled bool
	// dbPath is repoAccessDBPath outside tests.
	dbPath string
	// internalURL is how helper pods reach this control plane (AC-H3).
	internalURL string
}

func loadRepoAccessConfig() repoAccessConfig {
	return repoAccessConfig{
		enabled:     envBool("REPO_ACCESS_ENABLED", false),
		dbPath:      repoAccessDBPath,
		internalURL: env("REPO_ACCESS_INTERNAL_URL", ""),
	}
}

// helperURL defaults to this namespace's control-plane Service (k8s/service.yaml).
func (c repoAccessConfig) helperURL(namespace string) string {
	if !c.enabled {
		return ""
	}
	if c.internalURL != "" {
		return c.internalURL
	}
	return "http://control-plane." + namespace + ".svc.cluster.local"
}

type repoAccess struct {
	store *repoaccess.Store
}

func openRepoAccess(ctx context.Context, cfg repoAccessConfig) (repoAccess, error) {
	var ra repoAccess
	if !cfg.enabled {
		return ra, nil
	}
	store, err := repoaccess.Open(ctx, cfg.dbPath)
	if err != nil {
		return ra, fmt.Errorf("open %s (is a persistent volume mounted at %s?): %w", cfg.dbPath, filepath.Dir(cfg.dbPath), err)
	}
	ra.store = store
	return ra, nil
}

func (ra repoAccess) routes(mux *http.ServeMux, client kubernetes.Interface, namespace string, mgr session.Manager, logger *slog.Logger) {
	// Typed nils would defeat the handlers' nil checks.
	var store api.RepoAccessStore
	var helpers api.HelperVerifier
	if ra.store != nil {
		store = ra.store
		helpers = helperauth.New(client, namespace, k8s.SessionMCPServiceAccountName,
			k8s.RepoAccessTokenAudience, k8s.LabelSessionID, k8s.LabelPodRole, k8s.PodRoleHelper)
	}
	api.NewRepoAccess(store, helpers, mgr, logger).Routes(mux)
}

func (ra repoAccess) close() {
	if ra.store != nil {
		_ = ra.store.Close()
	}
}
