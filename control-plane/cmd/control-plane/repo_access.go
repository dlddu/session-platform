package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/adminauth"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/repoaccess"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// repoAccessConfig is docs/prd/repo-access.md's opt-in (AC-H1/AC-H2). An empty
// dbPath leaves the feature off; sign-in may still be configured on its own.
type repoAccessConfig struct {
	dbPath string
	// internalURL is how helper pods reach this control plane (AC-H3).
	internalURL string
	oidc        oidcConfig
}

type oidcConfig struct {
	issuer, clientID, clientSecret, redirectURL, cookieKey string
	insecureCookie                                         bool
}

func (c oidcConfig) configured() bool {
	return c.issuer != "" || c.clientID != "" || c.clientSecret != "" || c.redirectURL != "" || c.cookieKey != ""
}

func loadRepoAccessConfig() repoAccessConfig {
	return repoAccessConfig{
		dbPath:      strings.TrimSpace(env("REPO_ACCESS_DB_PATH", "")),
		internalURL: env("REPO_ACCESS_INTERNAL_URL", ""),
		oidc: oidcConfig{
			issuer:         env("OIDC_ISSUER", ""),
			clientID:       env("OIDC_CLIENT_ID", ""),
			clientSecret:   env("OIDC_CLIENT_SECRET", ""),
			redirectURL:    env("OIDC_REDIRECT_URL", ""),
			cookieKey:      env("OIDC_COOKIE_KEY", ""),
			insecureCookie: envBool("OIDC_COOKIE_INSECURE", false),
		},
	}
}

// helperURL defaults to this namespace's control-plane Service (k8s/service.yaml).
func (c repoAccessConfig) helperURL(namespace string) string {
	if c.dbPath == "" {
		return ""
	}
	if c.internalURL != "" {
		return c.internalURL
	}
	return "http://control-plane." + namespace + ".svc.cluster.local"
}

type repoAccess struct {
	store *repoaccess.Store
	auth  *adminauth.Auth
}

// openRepoAccess fails startup on a half-configured feature: an allow list
// nobody can sign in to manage would only ever collect requests.
func openRepoAccess(ctx context.Context, cfg repoAccessConfig) (repoAccess, error) {
	var ra repoAccess
	if cfg.oidc.configured() {
		a, err := adminauth.New(adminauth.Config{
			Issuer:       cfg.oidc.issuer,
			ClientID:     cfg.oidc.clientID,
			ClientSecret: cfg.oidc.clientSecret,
			RedirectURL:  cfg.oidc.redirectURL,
			CookieKey:    []byte(cfg.oidc.cookieKey),
			Secure:       !cfg.oidc.insecureCookie,
		})
		if err != nil {
			return ra, err
		}
		ra.auth = a
	}
	if cfg.dbPath == "" {
		return ra, nil
	}
	if ra.auth == nil {
		return ra, errors.New("REPO_ACCESS_DB_PATH needs OIDC_ISSUER, OIDC_CLIENT_ID, OIDC_REDIRECT_URL and OIDC_COOKIE_KEY (OIDC_CLIENT_SECRET only for a confidential client)")
	}
	store, err := repoaccess.Open(ctx, cfg.dbPath)
	if err != nil {
		return ra, fmt.Errorf("open %s: %w", cfg.dbPath, err)
	}
	ra.store = store
	return ra, nil
}

func (ra repoAccess) routes(mux *http.ServeMux, client kubernetes.Interface, namespace string, mgr session.Manager, logger *slog.Logger) {
	if ra.auth != nil {
		ra.auth.Routes(mux)
	} else {
		// The SPA asks /me to choose between its sign-in and "not configured"
		// views; without this the static handler would answer with index.html.
		mux.HandleFunc("/api/v1/auth/", (*adminauth.Auth)(nil).Require(nil))
	}
	// Typed nils would defeat the handlers' nil checks.
	var store api.RepoAccessStore
	var helpers api.HelperVerifier
	if ra.store != nil {
		store = ra.store
		helpers = helperauth.New(client, namespace, k8s.SessionMCPServiceAccountName,
			k8s.RepoAccessTokenAudience, k8s.LabelSessionID, k8s.LabelPodRole, k8s.PodRoleHelper)
	}
	api.NewRepoAccess(store, ra.auth, helpers, mgr, logger).Routes(mux)
}

func (ra repoAccess) close() {
	if ra.store != nil {
		_ = ra.store.Close()
	}
}
