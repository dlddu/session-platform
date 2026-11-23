package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/helperauth"
	"github.com/dlddu/session-platform/control-plane/internal/monitorpair"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"k8s.io/client-go/kubernetes"
)

// monitorPairDBPath holds only the parent↔monitor pairs. A deployment that
// turns the feature on mounts a persistent volume at its directory; a missing
// mount fails startup rather than quietly keeping the pairs on the container's
// filesystem, where a restart would lose every binding.
const monitorPairDBPath = "/var/lib/session-platform/monitor-pair/monitor-pair.db"

// monitorPairConfig is the opt-in for the separated monitor. Off unless
// MONITOR_PAIR_ENABLED, and off means the routes answer 503 and no token is
// projected into any pod.
type monitorPairConfig struct {
	enabled bool
	// dbPath is monitorPairDBPath outside tests.
	dbPath string
	// internalURL is how a session's workload reaches this control plane.
	internalURL string
}

func loadMonitorPairConfig() monitorPairConfig {
	return monitorPairConfig{
		enabled:     envBool("MONITOR_PAIR_ENABLED", false),
		dbPath:      monitorPairDBPath,
		internalURL: env("MONITOR_PAIR_INTERNAL_URL", ""),
	}
}

// workloadURL defaults to this namespace's control-plane Service, the same
// default repo access uses.
func (c monitorPairConfig) workloadURL(namespace string) string {
	if !c.enabled {
		return ""
	}
	if c.internalURL != "" {
		return c.internalURL
	}
	return "http://control-plane." + namespace + ".svc.cluster.local"
}

type monitorPairs struct {
	store *monitorpair.Store
}

func openMonitorPairs(ctx context.Context, cfg monitorPairConfig) (monitorPairs, error) {
	var mp monitorPairs
	if !cfg.enabled {
		return mp, nil
	}
	store, err := monitorpair.Open(ctx, cfg.dbPath)
	if err != nil {
		return mp, fmt.Errorf("open %s (is a persistent volume mounted at %s?): %w",
			cfg.dbPath, filepath.Dir(cfg.dbPath), err)
	}
	mp.store = store
	return mp, nil
}

// routes mounts the monitor-pair API. The verifier accepts only a token minted
// for this feature's own audience by the data plane service account, bound to
// a pod labelled with the session's ID and the workload role — so the caller
// is a session's workload container and nothing else.
func (mp monitorPairs) routes(mux *http.ServeMux, client kubernetes.Interface, namespace string,
	mgr session.Manager, guard api.TenantGuard, logger *slog.Logger) {
	// Typed nils would defeat the handler's nil checks.
	var pairs api.MonitorPairs
	var identity api.SessionIdentity
	if mp.store != nil {
		pairs = mp.store
		identity = helperauth.New(client, namespace, k8s.DataPlaneServiceAccountName,
			k8s.MonitorPairTokenAudience, k8s.LabelSessionID, k8s.LabelPodRole, k8s.PodRoleWorkload)
	}
	monitor := api.NewMonitor(pairs, identity, mgr, logger)
	monitor.Routes(mux)
	// The same pair surface for a backend that calls on a person's behalf.
	// The guard is the API's own session authorization, so ownership is
	// decided in one place.
	if mp.store != nil {
		monitor.BackendRoutes(mux, guard)
	}
}

func (mp monitorPairs) close() {
	if mp.store != nil {
		_ = mp.store.Close()
	}
}

// roleOf is the lookup the orchestrator uses to tell a pod its side of the
// pair. It reads the control plane's own store and nothing else.
func (mp monitorPairs) roleOf(sessionID string) string {
	if mp.store == nil {
		return ""
	}
	_, role, err := mp.store.For(context.Background(), sessionID)
	if err != nil {
		return ""
	}
	return string(role)
}
