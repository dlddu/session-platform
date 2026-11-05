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

// codexConfig is the codex slice of the control-plane configuration
// (docs/prd/codex-workload.md, AC-J1, AC-J4~J6), kept apart from the other
// types' fields so turning codex on changes nothing else.
type codexConfig struct {
	// image is the data plane image for `workloadType=codex`. Unset means the
	// type is not offered and creating one fails (AC-J1, same rule as AC-E1).
	image string
	// credentialsSecret holds the optional k3s-mcp-token the sidecar reads.
	// There is no provider key: each session logs in to ChatGPT itself (AC-J3).
	credentialsSecret string
	// configConfigMap holds model, models, k3s-mcp-url and
	// plugin-marketplace-url, all optional.
	configConfigMap string
	// plugins names the marketplace plugins whose skills sessions get (AC-J5).
	plugins string
	// archiveEnabled is this type's own data-transfer gate. Its archive carries
	// the session's ChatGPT login (AC-J3, AC-J4).
	archiveEnabled bool
	// defaultModel and models are the UI picker snapshot, projected from the
	// ConfigMap like claude-code's (AC-E6).
	defaultModel string
	models       []string
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
	return codexConfig{
		image:             env("DATA_PLANE_CODEX_IMAGE", ""),
		credentialsSecret: env("CODEX_CREDENTIALS_SECRET", "codex-credentials"),
		configConfigMap:   env("CODEX_CONFIG_CONFIGMAP", "codex-config"),
		plugins:           env("CODEX_PLUGINS", k8s.DefaultCodexPlugins),
		archiveEnabled:    envBool("CODEX_ARCHIVE_ENABLED", false),
		defaultModel:      defaultModel,
		models:            models,
	}, nil
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

// codexArchiveOption installs codex's filesystem-archive checkpointer, or
// returns nil when its gate is off (AC-J4).
func codexArchiveOption(cfg codexConfig, client *agent.HTTPClient, store archive.CheckpointStore) (service.Option, error) {
	if !cfg.archiveEnabled {
		return nil, nil
	}
	if cfg.image == "" {
		return nil, errors.New("CODEX_ARCHIVE_ENABLED needs DATA_PLANE_CODEX_IMAGE")
	}
	return service.WithWorkloadCheckpointer(session.WorkloadTypeCodex, archive.NewAgentCheckpointer(client, store)), nil
}
