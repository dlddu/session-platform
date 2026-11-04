package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type miniMaxConfig struct {
	// image is the data plane image for `workloadType=minimax-code` sessions.
	// Unset means the type is not deployable here: creating such a session
	// fails loudly rather than getting a shell pod (AC-G1, same rule as AC-E1).
	image string
	// credentialsSecret names the Secret whose auth-token (and optional
	// k3s-mcp-token) reach this type's helper pod proxies.
	credentialsSecret string
	// configConfigMap names the ConfigMap holding this type's non-credential
	// keys: base-url (required), model and ca-cert (optional).
	configConfigMap string
	// archiveEnabled is this type's own mechanism gate: workspace, conversation
	// and output archives go to CHECKPOINT_S3_* only when it is explicitly on,
	// exactly as CLAUDE_CODE_ARCHIVE_ENABLED is for claude-code (AC-G4).
	archiveEnabled bool
}

func loadMiniMaxConfig() miniMaxConfig {
	return miniMaxConfig{
		image:             env("DATA_PLANE_MINIMAX_CODE_IMAGE", ""),
		credentialsSecret: env("MINIMAX_CODE_CREDENTIALS_SECRET", "minimax-code-credentials"),
		configConfigMap:   env("MINIMAX_CODE_CONFIG_CONFIGMAP", "minimax-code-config"),
		archiveEnabled:    envBool("MINIMAX_CODE_ARCHIVE_ENABLED", false),
	}
}

// validateMiniMaxSecret refuses a configuration where minimax-code would read
// the claude-code Secret or ConfigMap. AC-G3 promises the two types never share
// provider configuration, and naming the same object twice would break that
// silently.
func validateMiniMaxSecret(cfg miniMaxConfig, claudeSecret, claudeConfigMap string) error {
	if strings.TrimSpace(cfg.credentialsSecret) == strings.TrimSpace(claudeSecret) {
		return fmt.Errorf("MINIMAX_CODE_CREDENTIALS_SECRET must differ from CLAUDE_CODE_CREDENTIALS_SECRET (both %q)", claudeSecret)
	}
	if strings.TrimSpace(cfg.configConfigMap) == strings.TrimSpace(claudeConfigMap) {
		return fmt.Errorf("MINIMAX_CODE_CONFIG_CONFIGMAP must differ from CLAUDE_CODE_CONFIG_CONFIGMAP (both %q)", claudeConfigMap)
	}
	return nil
}

// miniMaxArchiveOption returns the service option that installs the
// filesystem-archive checkpointer for minimax-code, or nil when the gate is off.
// The gate needs the image for the same reason claude-code's does: a gate that
// is on for a type nobody can create is a misconfiguration worth failing on.
func miniMaxArchiveOption(cfg miniMaxConfig, client *agent.HTTPClient, store archive.CheckpointStore) (service.Option, error) {
	if !cfg.archiveEnabled {
		return nil, nil
	}
	if cfg.image == "" {
		return nil, errors.New("MINIMAX_CODE_ARCHIVE_ENABLED needs DATA_PLANE_MINIMAX_CODE_IMAGE")
	}
	return service.WithWorkloadCheckpointer(
		session.WorkloadTypeMiniMaxCode,
		archive.NewAgentCheckpointer(client, store),
	), nil
}
