// Command control-plane is the single entrypoint for the session platform
// control plane: it serves the REST API (/api/v1) and the embedded React SPA
// on one port. Domain logic is delegated to a session.Manager built from the
// (currently stubbed) k8s/redis/criu adapters.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/redis"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/static"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := loadConfig()
	logger.Info("starting control plane",
		"addr", cfg.addr,
		"redis", cfg.redisAddr,
		"namespace", cfg.namespace,
		"criu_enabled", cfg.criuEnabled,
	)

	// The control plane drives data plane pods via client-go, so it must run
	// inside a Kubernetes cluster. The redis / CRIU adapters remain stubs behind
	// the same interfaces (see each adapter package).
	client, err := k8s.InClusterClient()
	if err != nil {
		logger.Error("k8s: control plane must run inside a cluster", "err", err)
		os.Exit(1)
	}
	orch := k8s.NewClientOrchestrator(client, cfg.namespace, k8s.WithImage(cfg.dataPlaneImage))
	logger.Info("k8s: using client-go PodOrchestrator", "namespace", cfg.namespace)
	store := redis.NewStubStore(cfg.redisAddr)
	ckpt := criu.NewStubCheckpointer(cfg.criuEnabled)

	mgr := service.New(orch, store, ckpt)

	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	mux.Handle("/", static.Handler())

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           withLogging(logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("listening", "addr", cfg.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

type config struct {
	addr           string
	redisAddr      string
	namespace      string
	dataPlaneImage string
	criuEnabled    bool
}

func loadConfig() config {
	return config{
		addr:           env("CP_ADDR", ":8080"),
		redisAddr:      env("REDIS_ADDR", "redis:6379"),
		namespace:      k8s.NamespaceFromServiceAccount(),
		dataPlaneImage: env("DATA_PLANE_IMAGE", ""),
		criuEnabled:    envBool("CRIU_ENABLED", false),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

// withLogging is a tiny request logger so the scaffolding is observable.
func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Debug("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
	})
}
