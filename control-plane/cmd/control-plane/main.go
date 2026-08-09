// Command control-plane is the single entrypoint for the session platform
// control plane: it serves the REST API (/api/v1) and the embedded React SPA
// on one port. Domain logic is delegated to a session.Manager built from the
// k8s pod orchestrator, the ConfigMap/Lease state store, and workload-specific
// snapshot strategies. Disabled strategies fail closed and never reclaim a
// live pod.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/checkpointstore"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/static"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid runtime configuration", "err", err)
		os.Exit(1)
	}

	// The control plane drives data plane pods AND stores session state via
	// client-go, so it needs a reachable cluster: the in-cluster config as a pod,
	// or the ambient kubeconfig for local development against a kind cluster. The
	// same client backs the pod orchestrator and ConfigMap/Lease state store.
	// Enabled snapshot strategies ask the session pod agent to produce either a
	// shell CRIU bundle or Claude filesystem archive, then persist it in the
	// configured durable store; disabled strategies fail closed before reclaim.
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
		"data_plane_claude_code_image", cfg.dataPlaneClaudeCodeImage,
		"data_plane_shell", cfg.dataPlaneShell,
		"claude_code_default_model", cfg.claudeCodeDefaultModel,
		"claude_code_models", len(cfg.claudeCodeModels),
	)
	if cfg.dataPlaneImage == "" {
		// The in-code fallback image has no session agent, so session pods will
		// never pass the shell readiness probe (AC-D1). Deployments must inject
		// the published data plane agent image.
		logger.Warn("DATA_PLANE_IMAGE is not set; falling back to a placeholder image that cannot run session shells")
	}

	orch := k8s.NewClientOrchestrator(client, namespace,
		k8s.WithImage(cfg.dataPlaneImage), k8s.WithShell(cfg.dataPlaneShell),
		// Per-type image (AC-E1). Empty is a no-op, which leaves claude-code
		// unconfigured — Start then refuses that type instead of provisioning a
		// shell pod under a claude-code label.
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, cfg.dataPlaneClaudeCodeImage),
		k8s.WithClaudeCredentialsSecret(cfg.claudeCredentialsSecret),
		k8s.WithCheckpointPrivileged(cfg.criuEnabled))
	store := configmap.NewStore(client, namespace)
	// Shell I/O (write→stdin, read→scrollback delta) AND checkpoint/restore ride
	// the same agent client: it resolves pod name → IP per request and dials the
	// session agent directly (AC-D2/D3, and /checkpoint·/restore for CRIU).
	agentClient := agent.NewHTTPClient(client, namespace)

	// Snapshot archives may contain workspace and conversation data. They are
	// sent to the configured durable store only behind an explicit mechanism
	// gate: CRIU_ENABLED for shell, CLAUDE_CODE_ARCHIVE_ENABLED for claude-code.
	var cstore criu.CheckpointStore
	if cfg.criuEnabled || cfg.claudeArchiveEnabled {
		cstore, err = buildCheckpointStore(cfg)
		if err != nil {
			logger.Error("checkpoint store misconfigured", "err", err)
			os.Exit(1)
		}
	}

	var shellCkpt criu.Checkpointer = criu.NewStubCheckpointer(false)
	if cfg.criuEnabled {
		shellCkpt = criu.NewAgentCheckpointer(agentClient, cstore)
		logger.Info("CRIU on: agent-driven in-pod checkpoint",
			"store", cfg.checkpointStoreDesc())
	}

	var serviceOpts []service.Option
	if cfg.claudeArchiveEnabled {
		if cfg.dataPlaneClaudeCodeImage == "" {
			logger.Error("CLAUDE_CODE_ARCHIVE_ENABLED needs DATA_PLANE_CLAUDE_CODE_IMAGE")
			os.Exit(1)
		}
		serviceOpts = append(serviceOpts, service.WithWorkloadCheckpointer(
			session.WorkloadTypeClaudeCode,
			criu.NewAgentArchiveCheckpointer(agentClient, cstore),
		))
		logger.Info("claude-code filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}
	mgr := service.New(orch, store, shellCkpt, agentClient, serviceOpts...)
	snapshotEnabled := shellCkpt.Enabled() || cfg.claudeArchiveEnabled

	// AC-B1: the operational idle->snapshot trigger. A background reaper scans
	// sessions every cfg.idleScanInterval and freezes any idle (no client
	// read/write, AC-D5) for at least session.MaxIdle, reclaiming its pod
	// (AC-A3). Manual snapshots use the same manager operation through the
	// product API without waiting for the idle limit.
	reaper := service.NewIdleReaper(mgr, session.MaxIdle, cfg.idleScanInterval, nil, logger)

	mux := http.NewServeMux()
	api.New(mgr,
		api.WithClaudeCodeModelConfig(cfg.claudeCodeDefaultModel, cfg.claudeCodeModels),
	).Routes(mux)
	mux.Handle("/", static.Handler())

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           withLogging(logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Drive the idle->snapshot reaper until shutdown (AC-B1).
	if snapshotEnabled {
		go reaper.Run(ctx)
	} else {
		logger.Warn("idle reaper disabled: no real snapshot strategy is enabled")
	}

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
	// dataPlaneClaudeCodeImage is the image for `workloadType=claude-code`
	// sessions (AC-E1). Unset means the type is not deployable here: creating
	// such a session fails loudly rather than getting a shell pod.
	dataPlaneClaudeCodeImage string
	// claudeArchiveEnabled explicitly permits workspace/conversation/output
	// archives to be written to CHECKPOINT_S3_* (default false).
	claudeArchiveEnabled    bool
	claudeCredentialsSecret string
	// claudeCodeDefaultModel is the effective, public default shown by the SPA.
	// It is a concrete Secret-backed model when configured, otherwise the
	// stable platform-default alias.
	claudeCodeDefaultModel string
	// claudeCodeModels is the ordered, public model catalog shown by the SPA.
	// Empty preserves the free-text model input and existing API behaviour.
	claudeCodeModels []string
	criuEnabled      bool
	// idleScanInterval is how often the reaper scans for sessions past their
	// idle limit (AC-B1). The 60m limit itself is session.MaxIdle; this only
	// bounds how promptly a newly-idle session is noticed.
	idleScanInterval time.Duration
	// Checkpoint/archive store used by either explicitly enabled strategy:
	// an S3 bucket,
	// accessed by assuming checkpointS3RoleARN over the ambient credentials
	// (node instance profile / IRSA). checkpointS3Endpoint targets an
	// S3-compatible backend instead of AWS — the e2e SUT points it at MinIO and
	// leaves the role empty, authenticating with static keys from the env.
	checkpointS3Bucket      string
	checkpointS3RoleARN     string
	checkpointS3Region      string
	checkpointS3Prefix      string
	checkpointS3SessionName string
	checkpointS3Endpoint    string
}

// buildCheckpointStore builds the checkpoint archive backend. The agent-driven
// checkpointer always needs one: the archive is produced inside a pod that is
// reclaimed moments later.
func buildCheckpointStore(cfg config) (criu.CheckpointStore, error) {
	if cfg.checkpointS3Bucket == "" {
		return nil, errors.New("snapshot strategy needs a checkpoint store: set CHECKPOINT_S3_BUCKET")
	}
	return checkpointstore.NewS3(context.Background(), checkpointstore.Config{
		Bucket:      cfg.checkpointS3Bucket,
		RoleARN:     cfg.checkpointS3RoleARN,
		Region:      cfg.checkpointS3Region,
		Prefix:      cfg.checkpointS3Prefix,
		SessionName: cfg.checkpointS3SessionName,
		Endpoint:    cfg.checkpointS3Endpoint,
	})
}

// checkpointStoreDesc renders the selected backend for the startup log.
func (c config) checkpointStoreDesc() string {
	desc := "s3:" + c.checkpointS3Bucket
	if c.checkpointS3Endpoint != "" {
		desc += " @" + c.checkpointS3Endpoint
	}
	if c.checkpointS3RoleARN != "" {
		desc += " (assume " + c.checkpointS3RoleARN + ")"
	}
	return desc
}

func loadConfig() (config, error) {
	claudeCodeDefaultModel, err := parseClaudeCodeDefaultModel(os.Getenv("CLAUDE_CODE_DEFAULT_MODEL"))
	if err != nil {
		return config{}, fmt.Errorf("CLAUDE_CODE_DEFAULT_MODEL: %w", err)
	}
	claudeCodeModels, err := parseClaudeCodeModels(os.Getenv("CLAUDE_CODE_MODELS"))
	if err != nil {
		return config{}, fmt.Errorf("CLAUDE_CODE_MODELS: %w", err)
	}
	return config{
		addr:           env("CP_ADDR", ":8080"),
		dataPlaneImage: env("DATA_PLANE_IMAGE", ""),
		// Propagated into session pods; the agent launches
		// ${DATA_PLANE_SHELL:-/bin/bash} on a PTY (AC-D1).
		dataPlaneShell:           env("DATA_PLANE_SHELL", ""),
		dataPlaneClaudeCodeImage: env("DATA_PLANE_CLAUDE_CODE_IMAGE", ""),
		claudeArchiveEnabled:     envBool("CLAUDE_CODE_ARCHIVE_ENABLED", false),
		claudeCredentialsSecret:  env("CLAUDE_CODE_CREDENTIALS_SECRET", "claude-code-credentials"),
		claudeCodeDefaultModel:   claudeCodeDefaultModel,
		claudeCodeModels:         claudeCodeModels,
		criuEnabled:              envBool("CRIU_ENABLED", false),
		idleScanInterval:         envDuration("IDLE_SCAN_INTERVAL", time.Minute),
		checkpointS3Endpoint:     env("CHECKPOINT_S3_ENDPOINT", ""),
		checkpointS3Bucket:       env("CHECKPOINT_S3_BUCKET", ""),
		checkpointS3RoleARN:      env("CHECKPOINT_S3_ROLE_ARN", ""),
		checkpointS3Region:       env("CHECKPOINT_S3_REGION", env("AWS_REGION", "")),
		checkpointS3Prefix:       env("CHECKPOINT_S3_PREFIX", "checkpoints"),
		checkpointS3SessionName:  env("CHECKPOINT_S3_SESSION_NAME", ""),
	}, nil
}

// parseClaudeCodeDefaultModel validates the optional Secret-backed default
// model. The reserved alias is produced only as the missing/empty fallback;
// explicitly configuring it would hide a likely Secret projection mistake.
func parseClaudeCodeDefaultModel(value string) (string, error) {
	if value == "" {
		return session.PlatformDefaultModel, nil
	}
	if value != strings.TrimSpace(value) {
		return "", errors.New("must be a model without surrounding whitespace")
	}
	normalized, err := session.NormalizeModel(session.WorkloadTypeClaudeCode, value)
	if err != nil {
		return "", errors.New("must be a valid model")
	}
	if normalized == session.PlatformDefaultModel {
		return "", fmt.Errorf("reserved alias %q cannot be configured explicitly", session.PlatformDefaultModel)
	}
	return normalized, nil
}

// parseClaudeCodeModels decodes the optional Secret-backed JSON catalog. It is
// intentionally presentation configuration, not an API allowlist: existing
// clients may still submit any model accepted by session.NormalizeModel.
func parseClaudeCodeModels(value string) ([]string, error) {
	if value == "" {
		return []string{}, nil
	}
	var models []string
	if err := json.Unmarshal([]byte(value), &models); err != nil {
		return nil, fmt.Errorf("must be a JSON string array: %w", err)
	}
	if models == nil {
		return nil, errors.New("must be a JSON string array, not null")
	}
	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		if model == "" || model != strings.TrimSpace(model) {
			return nil, fmt.Errorf("entry %d must be a non-empty model without surrounding whitespace", i)
		}
		normalized, err := session.NormalizeModel(session.WorkloadTypeClaudeCode, model)
		if err != nil {
			return nil, fmt.Errorf("entry %d is not a valid model", i)
		}
		if normalized == session.PlatformDefaultModel {
			return nil, fmt.Errorf("entry %d uses reserved alias %q", i, session.PlatformDefaultModel)
		}
		if _, duplicate := seen[normalized]; duplicate {
			return nil, fmt.Errorf("entry %d duplicates model %q", i, normalized)
		}
		seen[normalized] = struct{}{}
	}
	return append([]string{}, models...), nil
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

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d
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
