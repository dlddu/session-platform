// Command control-plane is the single entrypoint for the session platform
// control plane: it serves the REST API (/api/v1) and the embedded React SPA
// on one port. Domain logic is delegated to a session.Manager built from the
// k8s pod orchestrator, the ConfigMap/Lease state store, and the (stubbed)
// CRIU checkpointer.
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

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/checkpointstore"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/static"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := loadConfig()

	// The control plane drives data plane pods AND stores session state via
	// client-go, so it needs a reachable cluster: the in-cluster config as a pod,
	// or the ambient kubeconfig for local development against a kind cluster. The
	// same client backs the pod orchestrator, the ConfigMap/Lease state store,
	// and — when CRIU_ENABLED is on — the real CRIU checkpointer (kubelet
	// ContainerCheckpoint API); with the gate off the checkpointer is a no-op stub.
	client, namespace, err := k8s.BuildClient()
	if err != nil {
		logger.Error("k8s: no reachable cluster (in-cluster config or kubeconfig required)", "err", err)
		os.Exit(1)
	}
	logger.Info("starting control plane",
		"addr", cfg.addr,
		"namespace", namespace,
		"criu_enabled", cfg.criuEnabled,
		"data_plane_image", cfg.dataPlaneImage,
		"data_plane_shell", cfg.dataPlaneShell,
	)
	if cfg.dataPlaneImage == "" {
		// The in-code fallback image has no session agent, so session pods will
		// never pass the shell readiness probe (AC-D1). Deployments must inject
		// the published data plane agent image.
		logger.Warn("DATA_PLANE_IMAGE is not set; falling back to a placeholder image that cannot run session shells")
	}

	orch := k8s.NewClientOrchestrator(client, namespace,
		k8s.WithImage(cfg.dataPlaneImage), k8s.WithShell(cfg.dataPlaneShell),
		k8s.WithCheckpointCapabilities(cfg.criuEnabled))
	store := configmap.NewStore(client, namespace)
	// Shell I/O (write→stdin, read→scrollback delta) AND checkpoint/restore ride
	// the same agent client: it resolves pod name → IP per request and dials the
	// session agent directly (AC-D2/D3, and /checkpoint·/restore for CRIU).
	agentClient := agent.NewHTTPClient(client, namespace)

	// CRIU gate: on → agent-driven in-pod checkpoint/restore (the pod's own agent
	// CRIU-dumps/restores its shell tree), archives streamed to S3 by assuming
	// CHECKPOINT_S3_ROLE_ARN over the node instance profile. The archive is
	// produced inside a pod that is about to be reclaimed, so a durable store is
	// required. off → the no-op stub so the happy path runs without CRIU.
	var ckpt criu.Checkpointer = criu.NewStubCheckpointer(false)
	if cfg.criuEnabled {
		if cfg.checkpointS3Bucket == "" {
			logger.Error("CRIU_ENABLED but CHECKPOINT_S3_BUCKET unset; agent-driven checkpoint needs a durable store")
			os.Exit(1)
		}
		s3store, err := checkpointstore.NewS3(context.Background(), checkpointstore.Config{
			Bucket:      cfg.checkpointS3Bucket,
			RoleARN:     cfg.checkpointS3RoleARN,
			Region:      cfg.checkpointS3Region,
			Prefix:      cfg.checkpointS3Prefix,
			SessionName: cfg.checkpointS3SessionName,
		})
		if err != nil {
			logger.Error("checkpoint S3 store misconfigured", "err", err)
			os.Exit(1)
		}
		ckpt = criu.NewAgentCheckpointer(agentClient, s3store)
		logger.Info("CRIU on: agent-driven in-pod checkpoint → S3 (assume-role)",
			"bucket", cfg.checkpointS3Bucket, "role", cfg.checkpointS3RoleARN)
	}

	mgr := service.New(orch, store, ckpt, agentClient)

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
	dataPlaneImage string
	dataPlaneShell string
	criuEnabled    bool
	// Checkpoint archive S3 store (used only when criuEnabled). Empty bucket =
	// keep archives node-local. Access is by assuming checkpointS3RoleARN over
	// the node instance profile (see internal/adapter/checkpointstore).
	checkpointS3Bucket      string
	checkpointS3RoleARN     string
	checkpointS3Region      string
	checkpointS3Prefix      string
	checkpointS3SessionName string
}

func loadConfig() config {
	return config{
		addr:           env("CP_ADDR", ":8080"),
		dataPlaneImage: env("DATA_PLANE_IMAGE", ""),
		// Propagated into session pods; the agent launches
		// ${DATA_PLANE_SHELL:-/bin/bash} on a PTY (AC-D1).
		dataPlaneShell:          env("DATA_PLANE_SHELL", ""),
		criuEnabled:             envBool("CRIU_ENABLED", false),
		checkpointS3Bucket:      env("CHECKPOINT_S3_BUCKET", ""),
		checkpointS3RoleARN:     env("CHECKPOINT_S3_ROLE_ARN", ""),
		checkpointS3Region:      env("CHECKPOINT_S3_REGION", env("AWS_REGION", "")),
		checkpointS3Prefix:      env("CHECKPOINT_S3_PREFIX", "checkpoints"),
		checkpointS3SessionName: env("CHECKPOINT_S3_SESSION_NAME", ""),
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
