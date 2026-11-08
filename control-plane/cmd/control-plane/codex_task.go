package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// authPoolEnv sizes one auth pool (docs/prd/agent-auth-pool.md, AC-N1).
type authPoolEnv struct {
	// slots is the pool size; 0 means no pool, and the type that needs one is
	// then not offered.
	slots        int
	storageClass string
	storageSize  string
}

// loadAuthPoolEnv reads <PREFIX>_AUTH_SLOTS, _AUTH_STORAGE_CLASS and
// _AUTH_STORAGE_SIZE, the size defaulting to defaultSize. A malformed size or
// count fails startup: a pool that silently shrank to zero would turn a typo
// into an unoffered type.
func loadAuthPoolEnv(prefix, defaultSize string) (authPoolEnv, error) {
	cfg := authPoolEnv{
		storageClass: strings.TrimSpace(os.Getenv(prefix + "_AUTH_STORAGE_CLASS")),
		storageSize:  env(prefix+"_AUTH_STORAGE_SIZE", defaultSize),
	}
	if raw := strings.TrimSpace(os.Getenv(prefix + "_AUTH_SLOTS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return authPoolEnv{}, fmt.Errorf("%s_AUTH_SLOTS: must be a non-negative integer, got %q", prefix, raw)
		}
		cfg.slots = n
	}
	return cfg, nil
}

// pool builds the Lease-backed pool, or nil when it has no slots.
func (c authPoolEnv) pool(client kubernetes.Interface, namespace, name string) (k8s.AuthPool, error) {
	if c.slots == 0 {
		return nil, nil
	}
	return k8s.NewLeaseAuthPool(client, namespace, k8s.AuthPoolConfig{
		Pool: name, Slots: c.slots, StorageClass: c.storageClass, StorageSize: c.storageSize,
	})
}

// codexTaskConfig is the codex-task slice of the configuration
// (docs/prd/codex-task-workload.md, AC-M1~M4), apart from codex-chat's so the
// two types never share a Secret, ConfigMap, gate or pool.
type codexTaskConfig struct {
	image             string
	credentialsSecret string
	configConfigMap   string
	plugins           string
	// archiveEnabled is this type's own data-transfer gate (AC-M3).
	archiveEnabled bool
	defaultModel   string
	models         []string
	pool           authPoolEnv
}

func loadCodexTaskConfig() (codexTaskConfig, error) {
	defaultModel, err := parseClaudeCodeDefaultModel(os.Getenv("CODEX_TASK_DEFAULT_MODEL"))
	if err != nil {
		return codexTaskConfig{}, fmt.Errorf("CODEX_TASK_DEFAULT_MODEL: %w", err)
	}
	models, err := parseClaudeCodeModels(os.Getenv("CODEX_TASK_MODELS"))
	if err != nil {
		return codexTaskConfig{}, fmt.Errorf("CODEX_TASK_MODELS: %w", err)
	}
	pool, err := loadAuthPoolEnv("CODEX_TASK", k8s.DefaultAuthSlotStorageSize)
	if err != nil {
		return codexTaskConfig{}, err
	}
	return codexTaskConfig{
		image:             env("DATA_PLANE_CODEX_TASK_IMAGE", ""),
		credentialsSecret: env("CODEX_TASK_CREDENTIALS_SECRET", "codex-task-credentials"),
		configConfigMap:   env("CODEX_TASK_CONFIG_CONFIGMAP", "codex-task-config"),
		plugins:           env("CODEX_TASK_PLUGINS", k8s.DefaultCodexPlugins),
		archiveEnabled:    envBool("CODEX_TASK_ARCHIVE_ENABLED", false),
		defaultModel:      defaultModel,
		models:            models,
		pool:              pool,
	}, nil
}

// offered is AC-M1's rule: an image and at least one slot.
func (c codexTaskConfig) offered() bool {
	return c.image != "" && c.pool.slots > 0
}

// objects names this type's Secret and ConfigMap for the overlap check.
func (c codexTaskConfig) objects() map[string]string {
	return map[string]string{
		"CODEX_TASK_CREDENTIALS_SECRET": c.credentialsSecret,
		"CODEX_TASK_CONFIG_CONFIGMAP":   c.configConfigMap,
	}
}

// validateCodexTaskConfig refuses to share a Secret or ConfigMap with another
// agent type, for codex-chat's reason (AC-J6): the key names are the same.
func validateCodexTaskConfig(cfg codexTaskConfig, others map[string]string) error {
	for what, name := range others {
		for own, mine := range cfg.objects() {
			if strings.TrimSpace(name) == strings.TrimSpace(mine) {
				return fmt.Errorf("%s must differ from %s (%q)", own, what, name)
			}
		}
	}
	return nil
}

func codexTaskArchiveOption(cfg codexTaskConfig, client *agent.HTTPClient, store archive.CheckpointStore) (service.Option, error) {
	if !cfg.archiveEnabled {
		return nil, nil
	}
	if cfg.image == "" {
		return nil, errors.New("CODEX_TASK_ARCHIVE_ENABLED needs DATA_PLANE_CODEX_TASK_IMAGE")
	}
	return service.WithWorkloadCheckpointer(session.WorkloadTypeCodexTask, archive.NewAgentCheckpointer(client, store)), nil
}

// claudeAuthConfig is claude-code's provider auth choice (AC-E7).
type claudeAuthConfig struct {
	mode k8s.ClaudeCodeAuthMode
	pool authPoolEnv
	// subscriptionDefaultModel and subscriptionModels are claude-code's picker
	// in subscription mode. They come from their own ConfigMap keys because the
	// gateway's model ids (OpenRouter) mean nothing to a Claude subscription, and
	// approval-gated keeps reading the gateway's keys (AC-E7).
	subscriptionDefaultModel string
	subscriptionModels       []string
}

func loadClaudeAuthConfig() (claudeAuthConfig, error) {
	mode, err := k8s.ParseClaudeCodeAuthMode(os.Getenv("CLAUDE_CODE_AUTH_MODE"))
	if err != nil {
		return claudeAuthConfig{}, fmt.Errorf("CLAUDE_CODE_AUTH_MODE: %w", err)
	}
	pool, err := loadAuthPoolEnv("CLAUDE_CODE", k8s.DefaultAuthSlotStorageSize)
	if err != nil {
		return claudeAuthConfig{}, err
	}
	defaultModel, err := parseClaudeCodeDefaultModel(os.Getenv("CLAUDE_CODE_SUBSCRIPTION_DEFAULT_MODEL"))
	if err != nil {
		return claudeAuthConfig{}, fmt.Errorf("CLAUDE_CODE_SUBSCRIPTION_DEFAULT_MODEL: %w", err)
	}
	models, err := parseClaudeCodeModels(os.Getenv("CLAUDE_CODE_SUBSCRIPTION_MODELS"))
	if err != nil {
		return claudeAuthConfig{}, fmt.Errorf("CLAUDE_CODE_SUBSCRIPTION_MODELS: %w", err)
	}
	return claudeAuthConfig{
		mode: mode, pool: pool,
		subscriptionDefaultModel: defaultModel, subscriptionModels: models,
	}, nil
}

// claudeCodeModelConfig is the picker claude-code offers under this auth mode.
func (c claudeAuthConfig) claudeCodeModelConfig(gatewayDefault string, gatewayModels []string) (string, []string) {
	if c.mode == k8s.ClaudeCodeAuthSubscription {
		return c.subscriptionDefaultModel, c.subscriptionModels
	}
	return gatewayDefault, gatewayModels
}

// offered reports whether claude-code can provision here given its auth:
// the gateway needs nothing more, a subscription needs a slot (AC-E7, AC-N1).
func (c claudeAuthConfig) offered() bool {
	return c.mode != k8s.ClaudeCodeAuthSubscription || c.pool.slots > 0
}

// authPoolOptions builds the pools of the types that lease one. A claude-code
// pool is configured only in subscription mode: in gateway mode its slots
// would never be mounted, so leasing them would only lock sessions out.
func authPoolOptions(cfg config, client kubernetes.Interface, namespace string) ([]service.Option, error) {
	var opts []service.Option
	if cfg.codex.image != "" {
		pool, err := cfg.codex.pool.pool(client, namespace, k8s.AuthPoolCodexChat)
		if err != nil {
			return nil, err
		}
		if pool != nil {
			opts = append(opts, service.WithAuthPool(session.WorkloadTypeCodexChat, pool))
		}
	}
	if cfg.codexTask.image != "" {
		pool, err := cfg.codexTask.pool.pool(client, namespace, k8s.AuthPoolCodexTask)
		if err != nil {
			return nil, err
		}
		if pool != nil {
			opts = append(opts, service.WithAuthPool(session.WorkloadTypeCodexTask, pool))
		}
	}
	if cfg.claudeAuth.mode == k8s.ClaudeCodeAuthSubscription && cfg.dataPlaneClaudeCodeImage != "" {
		pool, err := cfg.claudeAuth.pool.pool(client, namespace, k8s.AuthPoolClaudeCode)
		if err != nil {
			return nil, err
		}
		if pool != nil {
			opts = append(opts, service.WithAuthPool(session.WorkloadTypeClaudeCode, pool))
		}
	}
	return opts, nil
}
