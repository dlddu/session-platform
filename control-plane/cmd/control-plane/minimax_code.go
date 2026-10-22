package main

import (
	"errors"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// miniMaxConfig is the minimax-code slice of the control-plane configuration
// (AC-G1, AC-G3, AC-G4). It is kept apart from claude-code's fields so that
// turning one type on never changes what the other does.
type miniMaxConfig struct {
	// image is the data plane image for `workloadType=minimax-code` sessions.
	// Unset means the type is not deployable here: creating such a session
	// fails loudly rather than getting a shell pod (AC-G1, same rule as AC-E1).
	image string
	// credentialsSecret names the Secret whose base-url/auth-token (and
	// optional model/ca-cert) keys reach this type's credential proxy sidecar.
	credentialsSecret string
	// archiveEnabled is this type's own mechanism gate: workspace, conversation
	// and output archives go to CHECKPOINT_S3_* only when it is explicitly on,
	// exactly as CLAUDE_CODE_ARCHIVE_ENABLED is for claude-code (AC-G4).
	archiveEnabled bool
}

func loadMiniMaxConfig() miniMaxConfig {
	return miniMaxConfig{
		image:             env("DATA_PLANE_MINIMAX_CODE_IMAGE", ""),
		credentialsSecret: env("MINIMAX_CODE_CREDENTIALS_SECRET", "minimax-code-credentials"),
		archiveEnabled:    envBool("MINIMAX_CODE_ARCHIVE_ENABLED", false),
	}
}

// miniMaxArchiveOption returns the service option that installs the
// filesystem-archive checkpointer for minimax-code, or nil when the gate is off.
// The gate needs the image for the same reason claude-code's does: a gate that
// is on for a type nobody can create is a misconfiguration worth failing on.
func miniMaxArchiveOption(cfg miniMaxConfig, client *agent.HTTPClient, store criu.CheckpointStore) (service.Option, error) {
	if !cfg.archiveEnabled {
		return nil, nil
	}
	if cfg.image == "" {
		return nil, errors.New("MINIMAX_CODE_ARCHIVE_ENABLED needs DATA_PLANE_MINIMAX_CODE_IMAGE")
	}
	return service.WithWorkloadCheckpointer(
		session.WorkloadTypeMiniMaxCode,
		criu.NewAgentArchiveCheckpointer(client, store),
	), nil
}
