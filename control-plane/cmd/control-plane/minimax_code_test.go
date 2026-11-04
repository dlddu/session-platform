package main

import (
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
)

func TestLoadMiniMaxConfigDefaults(t *testing.T) {
	t.Setenv("DATA_PLANE_MINIMAX_CODE_IMAGE", "")
	t.Setenv("MINIMAX_CODE_CREDENTIALS_SECRET", "")
	t.Setenv("MINIMAX_CODE_CONFIG_CONFIGMAP", "")
	t.Setenv("MINIMAX_CODE_ARCHIVE_ENABLED", "")
	got := loadMiniMaxConfig()
	want := miniMaxConfig{credentialsSecret: "minimax-code-credentials", configConfigMap: "minimax-code-config"}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

func TestLoadMiniMaxConfigReadsEnvironment(t *testing.T) {
	t.Setenv("DATA_PLANE_MINIMAX_CODE_IMAGE", "ghcr.io/example/data-plane:1")
	t.Setenv("MINIMAX_CODE_CREDENTIALS_SECRET", "mm-secret")
	t.Setenv("MINIMAX_CODE_CONFIG_CONFIGMAP", "mm-config")
	t.Setenv("MINIMAX_CODE_ARCHIVE_ENABLED", "true")
	got := loadMiniMaxConfig()
	want := miniMaxConfig{image: "ghcr.io/example/data-plane:1", credentialsSecret: "mm-secret", configConfigMap: "mm-config", archiveEnabled: true}
	if got != want {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

func TestValidateMiniMaxSecret(t *testing.T) {
	distinct := miniMaxConfig{credentialsSecret: "minimax-code-credentials", configConfigMap: "minimax-code-config"}
	if err := validateMiniMaxSecret(distinct, "claude-code-credentials", "claude-code-config"); err != nil {
		t.Fatalf("distinct names: %v", err)
	}
	if err := validateMiniMaxSecret(miniMaxConfig{credentialsSecret: " shared ", configConfigMap: "minimax-code-config"}, "shared", "claude-code-config"); err == nil {
		t.Fatal("shared secret: want an error")
	}
	err := validateMiniMaxSecret(miniMaxConfig{credentialsSecret: "minimax-code-credentials", configConfigMap: " shared "}, "claude-code-credentials", "shared")
	if err == nil || !strings.Contains(err.Error(), "MINIMAX_CODE_CONFIG_CONFIGMAP") {
		t.Fatalf("shared configmap: err = %v, want one naming MINIMAX_CODE_CONFIG_CONFIGMAP", err)
	}
}

func TestMiniMaxArchiveOption(t *testing.T) {
	client := agent.NewHTTPClient(fake.NewSimpleClientset(), "sessions")

	if opt, err := miniMaxArchiveOption(miniMaxConfig{}, client, nil); err != nil || opt != nil {
		t.Fatalf("gate off: option %v, err %v; want neither", opt != nil, err)
	}
	if _, err := miniMaxArchiveOption(miniMaxConfig{archiveEnabled: true}, client, nil); err == nil {
		t.Fatal("gate on without an image: want an error")
	}
	opt, err := miniMaxArchiveOption(miniMaxConfig{archiveEnabled: true, image: "img"}, client, nil)
	if err != nil || opt == nil {
		t.Fatalf("gate on with an image: option %v, err %v; want an option", opt != nil, err)
	}
}
