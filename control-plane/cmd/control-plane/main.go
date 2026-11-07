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

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/checkpointstore"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/static"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := loadConfig()
	if err == nil {
		err = validateMiniMaxSecret(cfg.miniMax, cfg.claudeCredentialsSecret, cfg.claudeConfigConfigMap)
	}
	if err == nil {
		others := map[string]string{
			"CLAUDE_CODE_CREDENTIALS_SECRET":  cfg.claudeCredentialsSecret,
			"CLAUDE_CODE_CONFIG_CONFIGMAP":    cfg.claudeConfigConfigMap,
			"MINIMAX_CODE_CREDENTIALS_SECRET": cfg.miniMax.credentialsSecret,
			"MINIMAX_CODE_CONFIG_CONFIGMAP":   cfg.miniMax.configConfigMap,
		}
		err = validateCodexTaskConfig(cfg.codexTask, map[string]string{
			"CODEX_CREDENTIALS_SECRET": cfg.codex.credentialsSecret,
			"CODEX_CONFIG_CONFIGMAP":   cfg.codex.configConfigMap,
		})
		if err == nil {
			err = validateCodexTaskConfig(cfg.codexTask, others)
		}
		if err == nil {
			err = validateCodexConfig(cfg.codex, others)
		}
	}
	if err != nil {
		logger.Error("invalid runtime configuration", "err", err)
		os.Exit(1)
	}

	// The control plane drives data plane pods AND stores session state via
	// client-go, so it needs a reachable cluster: the in-cluster config as a pod,
	// or the ambient kubeconfig for local development against a kind cluster.
	client, namespace, err := k8s.BuildClient()
	if err != nil {
		logger.Error("k8s: no reachable cluster (in-cluster config or kubeconfig required)", "err", err)
		os.Exit(1)
	}
	logger.Info("starting control plane",
		"addr", cfg.addr,
		"namespace", namespace,
		"shell_archive_enabled", cfg.shellArchiveEnabled,
		"approval_gated_archive_enabled", cfg.approvalGatedArchiveEnabled,
		"data_plane_image", cfg.dataPlaneImage,
		"data_plane_claude_code_image", cfg.dataPlaneClaudeCodeImage,
		"data_plane_approval_gated_image", cfg.dataPlaneApprovalGatedImage,
		"data_plane_minimax_code_image", cfg.miniMax.image,
		"data_plane_codex_image", cfg.codex.image,
		"data_plane_codex_task_image", cfg.codexTask.image,
		"codex_task_auth_slots", cfg.codexTask.pool.slots,
		"claude_code_auth_mode", cfg.claudeAuth.mode,
		"claude_code_auth_slots", cfg.claudeAuth.pool.slots,
		"tenants", len(cfg.tenancy.tenants.Tenants()),
		"approval_gateway_url", cfg.approvalGatewayURL,
		"data_plane_shell", cfg.dataPlaneShell,
		"claude_code_default_model", cfg.claudeCodeDefaultModel,
		"claude_code_models", len(cfg.claudeCodeModels),
		"repo_access_enabled", cfg.repoAccess.enabled,
		"drop_url", cfg.dropURL,
	)
	if cfg.dataPlaneImage == "" {
		// The in-code fallback image has no session agent, so session pods will
		// never pass the shell readiness probe (AC-D1). Deployments must inject
		// the published data plane agent image.
		logger.Warn("DATA_PLANE_IMAGE is not set; falling back to a placeholder image that cannot run session shells")
	}

	orch := k8s.NewClientOrchestrator(client, namespace,
		k8s.WithImage(cfg.dataPlaneImage), k8s.WithShell(cfg.dataPlaneShell),
		// Per-type image (AC-E1); empty is a no-op — see the config fields.
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, cfg.dataPlaneClaudeCodeImage),
		// Same for approval-gated (AC-F1).
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, cfg.dataPlaneApprovalGatedImage),
		k8s.WithWorkloadImage(session.WorkloadTypeMiniMaxCode, cfg.miniMax.image),
		k8s.WithClaudeCredentialsSecret(cfg.claudeCredentialsSecret),
		k8s.WithClaudeConfigConfigMap(cfg.claudeConfigConfigMap),
		k8s.WithMiniMaxCredentialsSecret(cfg.miniMax.credentialsSecret),
		k8s.WithMiniMaxConfigConfigMap(cfg.miniMax.configConfigMap),
		k8s.WithWorkloadImage(session.WorkloadTypeCodexChat, cfg.codex.image),
		k8s.WithCodexCredentialsSecret(cfg.codex.credentialsSecret),
		k8s.WithCodexConfigConfigMap(cfg.codex.configConfigMap),
		k8s.WithCodexPlugins(cfg.codex.plugins),
		k8s.WithWorkloadImage(session.WorkloadTypeCodexTask, cfg.codexTask.image),
		k8s.WithCodexTaskCredentialsSecret(cfg.codexTask.credentialsSecret),
		k8s.WithCodexTaskConfigConfigMap(cfg.codexTask.configConfigMap),
		k8s.WithCodexTaskPlugins(cfg.codexTask.plugins),
		k8s.WithClaudeCodeAuthMode(cfg.claudeAuth.mode),
		k8s.WithApprovalGatewayURL(cfg.approvalGatewayURL),
		k8s.WithSharedVolume(cfg.sharedVolumeStorageClass, cfg.sharedVolumeSize),
		k8s.WithRepoAccess(cfg.repoAccess.helperURL(namespace)),
		k8s.WithDropURL(cfg.dropURL))
	store := configmap.NewStore(client, namespace)
	// Shell I/O (write→stdin, read→scrollback delta) AND checkpoint/restore ride
	// the same agent client: it resolves pod name → IP per request and dials the
	// session agent directly (AC-D2/D3, and /checkpoint·/restore for archives).
	agentClient := agent.NewHTTPClient(client, namespace)

	// Snapshot archives may contain workspace and conversation data. They are
	// sent to the configured durable store only behind an explicit mechanism
	// gate per workload type: SHELL_ARCHIVE_ENABLED, CLAUDE_CODE_ARCHIVE_ENABLED,
	// APPROVAL_GATED_ARCHIVE_ENABLED and MINIMAX_CODE_ARCHIVE_ENABLED.
	var cstore archive.CheckpointStore
	if cfg.anyArchiveEnabled() {
		cstore, err = buildCheckpointStore(cfg)
		if err != nil {
			logger.Error("checkpoint store misconfigured", "err", err)
			os.Exit(1)
		}
	}

	// 게이트가 꺼진 배포에서 쉘 동결은 이 스텁이 거부한다(pod를 회수하지 않는다).
	var shellCkpt archive.Checkpointer = archive.NewStubCheckpointer(false)
	if cfg.shellArchiveEnabled {
		shellCkpt = archive.NewAgentCheckpointer(agentClient, cstore)
		logger.Info("shell filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}

	var serviceOpts []service.Option
	if cfg.claudeArchiveEnabled {
		if cfg.dataPlaneClaudeCodeImage == "" {
			logger.Error("CLAUDE_CODE_ARCHIVE_ENABLED needs DATA_PLANE_CLAUDE_CODE_IMAGE")
			os.Exit(1)
		}
		serviceOpts = append(serviceOpts, service.WithWorkloadCheckpointer(
			session.WorkloadTypeClaudeCode,
			archive.NewAgentCheckpointer(agentClient, cstore),
		))
		logger.Info("claude-code filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}
	approvalGatedArchive, err := approvalGatedArchiveOption(cfg, agentClient, cstore)
	if err != nil {
		logger.Error("approval-gated archive misconfigured", "err", err)
		os.Exit(1)
	}
	if approvalGatedArchive != nil {
		serviceOpts = append(serviceOpts, approvalGatedArchive)
		logger.Info("approval-gated filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}
	miniMaxArchive, err := miniMaxArchiveOption(cfg.miniMax, agentClient, cstore)
	if err != nil {
		logger.Error("minimax-code archive misconfigured", "err", err)
		os.Exit(1)
	}
	if miniMaxArchive != nil {
		serviceOpts = append(serviceOpts, miniMaxArchive)
		logger.Info("minimax-code filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}
	codexArchive, err := codexArchiveOption(cfg.codex, agentClient, cstore)
	if err != nil {
		logger.Error("codex archive misconfigured", "err", err)
		os.Exit(1)
	}
	if codexArchive != nil {
		serviceOpts = append(serviceOpts, codexArchive)
		logger.Info("codex filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}
	codexTaskArchive, err := codexTaskArchiveOption(cfg.codexTask, agentClient, cstore)
	if err != nil {
		logger.Error("codex-task archive misconfigured", "err", err)
		os.Exit(1)
	}
	if codexTaskArchive != nil {
		serviceOpts = append(serviceOpts, codexTaskArchive)
		logger.Info("codex-task filesystem archive enabled", "store", cfg.checkpointStoreDesc())
	}
	poolOpts, err := authPoolOptions(cfg, client, namespace)
	if err != nil {
		logger.Error("auth pool misconfigured", "err", err)
		os.Exit(1)
	}
	serviceOpts = append(serviceOpts, poolOpts...)
	mgr := service.New(orch, store, shellCkpt, agentClient, serviceOpts...)
	snapshotEnabled := cfg.anyArchiveEnabled()

	// AC-B1: the operational idle->snapshot trigger. Manual snapshots reach the
	// same manager operation through the product API, without the idle wait.
	reaper := service.NewIdleReaper(mgr, session.MaxIdle, cfg.idleScanInterval, nil, logger)

	mux := http.NewServeMux()
	claudeDefaultModel, claudeModels := cfg.claudeAuth.claudeCodeModelConfig(cfg.claudeCodeDefaultModel, cfg.claudeCodeModels)
	api.New(mgr,
		api.WithClaudeCodeModelConfig(claudeDefaultModel, claudeModels),
		// approval-gated reads the same ConfigMap's model keys (AC-F6 → AC-E6).
		api.WithWorkloadModelConfig(session.WorkloadTypeApprovalGated, cfg.claudeCodeDefaultModel, cfg.claudeCodeModels),
		api.WithWorkloadModelConfig(session.WorkloadTypeCodexChat, cfg.codex.defaultModel, cfg.codex.models),
		api.WithWorkloadModelConfig(session.WorkloadTypeCodexTask, cfg.codexTask.defaultModel, cfg.codexTask.models),
		api.WithOfferedWorkloadTypes(cfg.offeredWorkloadTypes()...),
		cfg.tenancy.apiOption(client),
	).Routes(mux)
	repoAccess, err := openRepoAccess(context.Background(), cfg.repoAccess)
	if err != nil {
		logger.Error("repo access misconfigured", "err", err)
		os.Exit(1)
	}
	defer repoAccess.close()
	repoAccess.routes(mux, client, namespace, mgr, logger)
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
	// dataPlaneApprovalGatedImage is the image for `workloadType=approval-gated`
	// sessions and their helper pods (AC-F1/AC-F4). Unset means the type is not
	// deployable here.
	dataPlaneApprovalGatedImage string
	// approvalGatewayURL is the PocketAide approval API the helper pod's MCP
	// container asks (AC-F3). Unset leaves approval-gated sessions without tools.
	approvalGatewayURL string
	// sharedVolumeStorageClass and sharedVolumeSize size the ReadWriteMany claim
	// an approval-gated session's two pods share (AC-F5; see WithSharedVolume).
	// An empty class leaves the volume off: only a deployment that knows which
	// of its classes serves ReadWriteMany can ask for one that will bind.
	sharedVolumeStorageClass string
	sharedVolumeSize         resource.Quantity
	// claudeArchiveEnabled explicitly permits workspace/conversation/output
	// archives to be written to CHECKPOINT_S3_* (default false).
	claudeArchiveEnabled bool
	// approvalGatedArchiveEnabled is approval-gated's own gate: its archive also
	// carries the AC-F5 shared volume's contents (default false).
	approvalGatedArchiveEnabled bool
	// shellArchiveEnabled permits a shell's home directory and scrollback to
	// be archived to CHECKPOINT_S3_* (default false). A restored shell is a
	// fresh process started in that home directory.
	shellArchiveEnabled     bool
	claudeCredentialsSecret string
	claudeConfigConfigMap   string
	// claudeCodeDefaultModel is the effective, public default shown by the SPA.
	// It is a concrete ConfigMap-backed model when configured, otherwise the
	// stable platform-default alias.
	claudeCodeDefaultModel string
	// claudeCodeModels is the ordered, public model catalog shown by the SPA.
	// Empty preserves the free-text model input and existing API behaviour.
	claudeCodeModels []string
	// idleScanInterval is how often the reaper scans for sessions past their
	// idle limit (AC-B1). The 60m limit itself is session.MaxIdle; this only
	// bounds how promptly a newly-idle session is noticed.
	idleScanInterval time.Duration
	// Checkpoint/archive store used by any explicitly enabled strategy: an S3
	// bucket reached through checkpointstore.NewS3. checkpointS3Endpoint targets
	// an S3-compatible backend instead of AWS.
	checkpointS3Bucket      string
	checkpointS3RoleARN     string
	checkpointS3Region      string
	checkpointS3Prefix      string
	checkpointS3SessionName string
	checkpointS3Endpoint    string
	miniMax                 miniMaxConfig
	codex                   codexConfig
	codexTask               codexTaskConfig
	claudeAuth              claudeAuthConfig
	tenancy                 tenancyConfig
	repoAccess              repoAccessConfig
	dropURL                 string // drop's in-cluster URL for helper pods (docs/prd/drop-file-share.md AC-I2)
}

// GET /config lists only these, so clients never offer a type whose create would fail.
func (c config) offeredWorkloadTypes() []session.WorkloadType {
	types := []session.WorkloadType{session.WorkloadTypeShell}
	for w, offered := range map[session.WorkloadType]bool{
		session.WorkloadTypeClaudeCode:    c.dataPlaneClaudeCodeImage != "" && c.claudeAuth.offered(),
		session.WorkloadTypeApprovalGated: c.dataPlaneApprovalGatedImage != "",
		session.WorkloadTypeMiniMaxCode:   c.miniMax.image != "",
		session.WorkloadTypeCodexChat:     c.codex.image != "",
		session.WorkloadTypeCodexTask:     c.codexTask.offered(),
	} {
		if offered {
			types = append(types, w)
		}
	}
	return types
}

// anyArchiveEnabled reports whether any workload type's archive gate is on,
// which is what makes the durable store and the idle reaper necessary.
func (c config) anyArchiveEnabled() bool {
	return c.shellArchiveEnabled || c.claudeArchiveEnabled ||
		c.approvalGatedArchiveEnabled || c.miniMax.archiveEnabled || c.codex.archiveEnabled ||
		c.codexTask.archiveEnabled
}

// approvalGatedArchiveOption returns the service option that installs the
// filesystem-archive checkpointer for approval-gated, or nil when the gate is
// off. Like claude-code's gate it needs the type's image to be meaningful.
func approvalGatedArchiveOption(cfg config, client *agent.HTTPClient, store archive.CheckpointStore) (service.Option, error) {
	if !cfg.approvalGatedArchiveEnabled {
		return nil, nil
	}
	if cfg.dataPlaneApprovalGatedImage == "" {
		return nil, errors.New("APPROVAL_GATED_ARCHIVE_ENABLED needs DATA_PLANE_APPROVAL_GATED_IMAGE")
	}
	return service.WithWorkloadCheckpointer(
		session.WorkloadTypeApprovalGated,
		archive.NewAgentCheckpointer(client, store),
	), nil
}

// buildCheckpointStore builds the checkpoint archive backend.
func buildCheckpointStore(cfg config) (archive.CheckpointStore, error) {
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
	// Parsed even when the volume is off, so a typo surfaces at startup rather
	// than on the day the class is configured. Unlike envDuration/envBool below
	// a bad value fails startup: a claim that binds at an unintended size is worse.
	sharedVolumeSize, err := resource.ParseQuantity(env("SESSION_SHARED_VOLUME_SIZE", "1Gi"))
	if err != nil {
		return config{}, fmt.Errorf("SESSION_SHARED_VOLUME_SIZE: %w", err)
	}
	codex, err := loadCodexConfig()
	if err != nil {
		return config{}, err
	}
	codexTask, err := loadCodexTaskConfig()
	if err != nil {
		return config{}, err
	}
	claudeAuth, err := loadClaudeAuthConfig()
	if err != nil {
		return config{}, err
	}
	tenants, err := loadTenancyConfig()
	if err != nil {
		return config{}, err
	}
	return config{
		addr:           env("CP_ADDR", ":8080"),
		dataPlaneImage: env("DATA_PLANE_IMAGE", ""),
		// Propagated into session pods; the agent launches
		// ${DATA_PLANE_SHELL:-/bin/bash} on a PTY (AC-D1).
		dataPlaneShell:              env("DATA_PLANE_SHELL", ""),
		dataPlaneClaudeCodeImage:    env("DATA_PLANE_CLAUDE_CODE_IMAGE", ""),
		dataPlaneApprovalGatedImage: env("DATA_PLANE_APPROVAL_GATED_IMAGE", ""),
		approvalGatewayURL:          env("APPROVAL_GATEWAY_URL", ""),
		sharedVolumeStorageClass:    env("SESSION_SHARED_VOLUME_STORAGE_CLASS", ""),
		sharedVolumeSize:            sharedVolumeSize,
		claudeArchiveEnabled:        envBool("CLAUDE_CODE_ARCHIVE_ENABLED", false),
		approvalGatedArchiveEnabled: envBool("APPROVAL_GATED_ARCHIVE_ENABLED", false),
		claudeCredentialsSecret:     env("CLAUDE_CODE_CREDENTIALS_SECRET", "claude-code-credentials"),
		claudeConfigConfigMap:       env("CLAUDE_CODE_CONFIG_CONFIGMAP", "claude-code-config"),
		claudeCodeDefaultModel:      claudeCodeDefaultModel,
		claudeCodeModels:            claudeCodeModels,
		shellArchiveEnabled:         envBool("SHELL_ARCHIVE_ENABLED", false),
		idleScanInterval:            envDuration("IDLE_SCAN_INTERVAL", time.Minute),
		checkpointS3Endpoint:        env("CHECKPOINT_S3_ENDPOINT", ""),
		checkpointS3Bucket:          env("CHECKPOINT_S3_BUCKET", ""),
		checkpointS3RoleARN:         env("CHECKPOINT_S3_ROLE_ARN", ""),
		checkpointS3Region:          env("CHECKPOINT_S3_REGION", env("AWS_REGION", "")),
		checkpointS3Prefix:          env("CHECKPOINT_S3_PREFIX", "checkpoints"),
		checkpointS3SessionName:     env("CHECKPOINT_S3_SESSION_NAME", ""),
		miniMax:                     loadMiniMaxConfig(),
		codex:                       codex,
		codexTask:                   codexTask,
		claudeAuth:                  claudeAuth,
		tenancy:                     tenants,
		repoAccess:                  loadRepoAccessConfig(),
		dropURL:                     env("DROP_URL", ""),
	}, nil
}

// parseClaudeCodeDefaultModel validates the optional ConfigMap-backed default
// model. The reserved alias is produced only as the missing/empty fallback;
// explicitly configuring it would hide a likely ConfigMap projection mistake.
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

// parseClaudeCodeModels decodes the optional ConfigMap-backed JSON catalog. It is
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
