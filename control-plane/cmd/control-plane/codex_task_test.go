package main

import (
	"slices"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func TestLoadCodexTaskConfigDefaults(t *testing.T) {
	cfg, err := loadCodexTaskConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.image != "" || cfg.credentialsSecret != "codex-task-credentials" || cfg.configConfigMap != "codex-task-config" ||
		cfg.plugins != "codex-session" || cfg.archiveEnabled || cfg.defaultModel != session.PlatformDefaultModel ||
		cfg.pool.slots != 0 || cfg.pool.storageSize != "16Mi" || cfg.pool.storageClass != "" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadAuthPoolEnvRejectsBadSlots(t *testing.T) {
	for _, v := range []string{"-1", "two", "1.5"} {
		t.Setenv("CODEX_TASK_AUTH_SLOTS", v)
		if _, err := loadCodexTaskConfig(); err == nil {
			t.Errorf("CODEX_TASK_AUTH_SLOTS=%q accepted", v)
		}
	}
	t.Setenv("CODEX_TASK_AUTH_SLOTS", "3")
	t.Setenv("CODEX_TASK_AUTH_STORAGE_CLASS", "local-path")
	cfg, err := loadCodexTaskConfig()
	if err != nil || cfg.pool.slots != 3 || cfg.pool.storageClass != "local-path" {
		t.Fatalf("cfg = %+v, %v", cfg.pool, err)
	}
}

func TestLoadClaudeAuthConfig(t *testing.T) {
	cfg, err := loadClaudeAuthConfig()
	if err != nil || cfg.mode != k8s.ClaudeCodeAuthGateway {
		t.Fatalf("default = %+v, %v", cfg, err)
	}
	t.Setenv("CLAUDE_CODE_AUTH_MODE", "openrouter")
	if _, err := loadClaudeAuthConfig(); err == nil {
		t.Fatal("unknown auth mode accepted")
	}
	t.Setenv("CLAUDE_CODE_AUTH_MODE", "subscription")
	t.Setenv("CLAUDE_CODE_AUTH_SLOTS", "2")
	cfg, err = loadClaudeAuthConfig()
	if err != nil || cfg.mode != k8s.ClaudeCodeAuthSubscription || cfg.pool.slots != 2 {
		t.Fatalf("subscription = %+v, %v", cfg, err)
	}
}

// AC-E7: a subscription offers its own picker; the gateway keeps the shared one.
func TestClaudeCodeModelConfigFollowsAuthMode(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SUBSCRIPTION_DEFAULT_MODEL", "sonnet")
	t.Setenv("CLAUDE_CODE_SUBSCRIPTION_MODELS", `["opus","sonnet"]`)
	cfg, err := loadClaudeAuthConfig()
	if err != nil {
		t.Fatal(err)
	}
	if d, m := cfg.claudeCodeModelConfig("~gw/model", []string{"~gw/model"}); d != "~gw/model" || !slices.Equal(m, []string{"~gw/model"}) {
		t.Fatalf("gateway picker = %q %v", d, m)
	}
	cfg.mode = k8s.ClaudeCodeAuthSubscription
	if d, m := cfg.claudeCodeModelConfig("~gw/model", []string{"~gw/model"}); d != "sonnet" || !slices.Equal(m, []string{"opus", "sonnet"}) {
		t.Fatalf("subscription picker = %q %v", d, m)
	}
	t.Setenv("CLAUDE_CODE_SUBSCRIPTION_MODELS", `not-json`)
	if _, err := loadClaudeAuthConfig(); err == nil {
		t.Fatal("malformed CLAUDE_CODE_SUBSCRIPTION_MODELS accepted")
	}
}

func TestValidateCodexTaskConfigRefusesSharedObjects(t *testing.T) {
	cfg, _ := loadCodexTaskConfig()
	if err := validateCodexTaskConfig(cfg, map[string]string{"CODEX_CONFIG_CONFIGMAP": "codex-config"}); err != nil {
		t.Fatalf("distinct names refused: %v", err)
	}
	cfg.configConfigMap = "codex-config"
	if err := validateCodexTaskConfig(cfg, map[string]string{"CODEX_CONFIG_CONFIGMAP": "codex-config"}); err == nil {
		t.Fatal("codex-task sharing codex-chat's ConfigMap was accepted")
	}
}

// AC-M1, AC-E7, AC-N1: a pooled type is offered only with a slot.
func TestOfferedWorkloadTypesNeedSlots(t *testing.T) {
	offered := func(c config, w session.WorkloadType) bool { return slices.Contains(c.offeredWorkloadTypes(), w) }
	task := config{codexTask: codexTaskConfig{image: "img"}}
	if offered(task, session.WorkloadTypeCodexTask) {
		t.Error("codex-task offered without slots")
	}
	task.codexTask.pool.slots = 1
	if !offered(task, session.WorkloadTypeCodexTask) {
		t.Error("codex-task not offered with an image and a slot")
	}
	cc := config{dataPlaneClaudeCodeImage: "img", claudeAuth: claudeAuthConfig{mode: k8s.ClaudeCodeAuthGateway}}
	if !offered(cc, session.WorkloadTypeClaudeCode) {
		t.Error("gateway claude-code not offered")
	}
	cc.claudeAuth.mode = k8s.ClaudeCodeAuthSubscription
	if offered(cc, session.WorkloadTypeClaudeCode) {
		t.Error("subscription claude-code offered without slots")
	}
	cc.claudeAuth.pool.slots = 2
	if !offered(cc, session.WorkloadTypeClaudeCode) {
		t.Error("subscription claude-code not offered with slots")
	}
}

func TestAuthPoolOptions(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cfg := config{
		dataPlaneClaudeCodeImage: "img",
		claudeAuth:               claudeAuthConfig{mode: k8s.ClaudeCodeAuthGateway, pool: authPoolEnv{slots: 2, storageSize: "16Mi"}},
		codexTask:                codexTaskConfig{image: "img", pool: authPoolEnv{slots: 1, storageSize: "16Mi"}},
	}
	opts, err := authPoolOptions(cfg, cs, "ns")
	if err != nil || len(opts) != 1 {
		t.Fatalf("gateway claude + codex-task: %d options, %v; want codex-task's only", len(opts), err)
	}
	cfg.claudeAuth.mode = k8s.ClaudeCodeAuthSubscription
	if opts, err = authPoolOptions(cfg, cs, "ns"); err != nil || len(opts) != 2 {
		t.Fatalf("subscription: %d options, %v", len(opts), err)
	}
	cfg.codexTask.pool.storageSize = "lots"
	if _, err := authPoolOptions(cfg, cs, "ns"); err == nil {
		t.Fatal("bad storage size accepted")
	}
}

func TestCodexTaskArchiveGateNeedsImage(t *testing.T) {
	if opt, err := codexTaskArchiveOption(codexTaskConfig{}, nil, nil); opt != nil || err != nil {
		t.Fatalf("gate off: %v, %v", opt, err)
	}
	if _, err := codexTaskArchiveOption(codexTaskConfig{archiveEnabled: true}, nil, nil); err == nil {
		t.Fatal("gate on without an image accepted")
	}
}
