package main

import (
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func TestLoadCodexConfigDefaults(t *testing.T) {
	cfg, err := loadCodexConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.image != "" || cfg.credentialsSecret != "codex-credentials" || cfg.configConfigMap != "codex-config" ||
		cfg.plugins != "session-platform" || cfg.archiveEnabled || cfg.defaultModel != session.PlatformDefaultModel {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadCodexConfigRejectsBadCatalog(t *testing.T) {
	t.Setenv("CODEX_MODELS", `["ok", "ok"]`)
	if _, err := loadCodexConfig(); err == nil {
		t.Fatal("duplicate catalog entry accepted")
	}
}

func TestValidateCodexConfigRefusesSharedObjects(t *testing.T) {
	cfg, _ := loadCodexConfig()
	if err := validateCodexConfig(cfg, map[string]string{"CLAUDE_CODE_CREDENTIALS_SECRET": "claude-code-credentials"}); err != nil {
		t.Fatalf("distinct names refused: %v", err)
	}
	cfg.credentialsSecret = "claude-code-credentials"
	if err := validateCodexConfig(cfg, map[string]string{"CLAUDE_CODE_CREDENTIALS_SECRET": "claude-code-credentials"}); err == nil {
		t.Fatal("codex reading claude-code's Secret was accepted")
	}
}

func TestCodexArchiveGateNeedsImage(t *testing.T) {
	if opt, err := codexArchiveOption(codexConfig{}, nil, nil); opt != nil || err != nil {
		t.Fatalf("gate off: %v, %v", opt, err)
	}
	if _, err := codexArchiveOption(codexConfig{archiveEnabled: true}, nil, nil); err == nil {
		t.Fatal("gate on without an image accepted")
	}
}

func TestOfferedWorkloadTypesIncludesCodexOnlyWithImage(t *testing.T) {
	has := func(c config) bool {
		for _, w := range c.offeredWorkloadTypes() {
			if w == session.WorkloadTypeCodex {
				return true
			}
		}
		return false
	}
	if has(config{}) {
		t.Fatal("codex offered without an image")
	}
	if !has(config{codex: codexConfig{image: "img"}}) {
		t.Fatal("codex not offered with an image")
	}
}

func TestLoadTenancyConfig(t *testing.T) {
	t.Setenv("SESSION_PLATFORM_TENANTS", `{"tenants":[{"name":"a","serviceAccounts":["n/sa"],"workloadTypes":["codex"]}]}`)
	cfg, err := loadTenancyConfig()
	if err != nil || len(cfg.tenants.Tenants()) != 1 || cfg.audience != "session-platform" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	t.Setenv("SESSION_PLATFORM_TENANTS", `{"tenants":[{"name":"A"}]}`)
	if _, err := loadTenancyConfig(); err == nil {
		t.Fatal("invalid tenant config accepted")
	}
}
