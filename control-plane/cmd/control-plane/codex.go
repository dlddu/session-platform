package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type codexConfig struct {
	image string
	// credentialsSecret holds the optional k3s-mcp-token the sidecar reads.
	// There is no provider key: the first session on a slot logs in to ChatGPT
	// itself and the login stays in the slot (AC-J3, AC-N6).
	credentialsSecret string
	// configConfigMap holds model, models, k3s-mcp-url and
	// plugin-marketplace-url, all optional.
	configConfigMap string
	// archiveEnabled is this type's own data-transfer gate. Its archive no
	// longer carries the ChatGPT login, which lives in the slot (AC-J3, AC-N6).
	archiveEnabled bool
	// defaultModel and models are the UI picker snapshot, projected from the
	// ConfigMap like claude-code's (AC-E6).
	defaultModel string
	models       []string
	// pool holds the session's login and database (AC-N6); the type is not
	// offered without a slot.
	pool authPoolEnv
}

func loadCodexConfig() (codexConfig, error) {
	defaultModel, err := parseClaudeCodeDefaultModel(os.Getenv("CODEX_DEFAULT_MODEL"))
	if err != nil {
		return codexConfig{}, fmt.Errorf("CODEX_DEFAULT_MODEL: %w", err)
	}
	models, err := parseClaudeCodeModels(os.Getenv("CODEX_MODELS"))
	if err != nil {
		return codexConfig{}, fmt.Errorf("CODEX_MODELS: %w", err)
	}
	pool, err := loadAuthPoolEnv("CODEX_CHAT", k8s.DefaultCodexChatSlotStorageSize)
	if err != nil {
		return codexConfig{}, err
	}
	return codexConfig{
		image:             env("DATA_PLANE_CODEX_IMAGE", ""),
		credentialsSecret: env("CODEX_CREDENTIALS_SECRET", "codex-credentials"),
		configConfigMap:   env("CODEX_CONFIG_CONFIGMAP", "codex-config"),
		archiveEnabled:    envBool("CODEX_ARCHIVE_ENABLED", false),
		defaultModel:      defaultModel,
		models:            models,
		pool:              pool,
	}, nil
}

func (c codexConfig) offered() bool {
	return c.image != "" && c.pool.slots > 0
}

// validateCodexConfig refuses to share a Secret or ConfigMap with another
// agent type: their keys have the same names, so sharing would silently hand
// codex another type's K3s token or model (AC-J6).
func validateCodexConfig(cfg codexConfig, others map[string]string) error {
	for what, name := range others {
		if strings.TrimSpace(name) == strings.TrimSpace(cfg.credentialsSecret) ||
			strings.TrimSpace(name) == strings.TrimSpace(cfg.configConfigMap) {
			return fmt.Errorf("codex Secret/ConfigMap must differ from %s (%q)", what, name)
		}
	}
	return nil
}

func codexArchiveOption(cfg codexConfig, client *agent.HTTPClient, store archive.CheckpointStore) (service.Option, error) {
	if !cfg.archiveEnabled {
		return nil, nil
	}
	if cfg.image == "" {
		return nil, errors.New("CODEX_ARCHIVE_ENABLED needs DATA_PLANE_CODEX_IMAGE")
	}
	return service.WithWorkloadCheckpointer(session.WorkloadTypeCodexManager, archive.NewAgentCheckpointer(client, store)), nil
}
