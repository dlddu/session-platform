package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/api"
)

type runtimeConfigResponse struct {
	ClaudeCode struct {
		DefaultModel string   `json:"defaultModel"`
		Models       []string `json:"models"`
	} `json:"claudeCode"`
}

func TestRuntimeConfigExposesClaudeCodeModelCatalog(t *testing.T) {
	models := []string{"claude-model-a", "claude-model-b"}
	srv := newServer(api.WithClaudeCodeModelConfig("~anthropic/claude-opus-latest", models))
	defer srv.Close()
	models[0] = "mutated-after-option"

	resp, err := http.Get(srv.URL + "/api/v1/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	want := "{\"claudeCode\":{\"defaultModel\":\"~anthropic/claude-opus-latest\",\"models\":[\"claude-model-a\",\"claude-model-b\"]}}\n"
	if string(body) != want {
		t.Errorf("config body = %q, want %q", body, want)
	}

	// The catalog is a UI picker, not a breaking API allowlist.
	status, _ := createRaw(t, srv.URL, map[string]any{
		"name":         "catalog-soft-choice",
		"workloadType": "claude-code",
		"model":        "custom-model-outside-catalog",
	})
	if status != http.StatusCreated {
		t.Fatalf("model outside catalog status = %d, want 201", status)
	}
}

func TestRuntimeConfigUsesEmptyArrayWithoutCatalog(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	var got runtimeConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if got.ClaudeCode.Models == nil || len(got.ClaudeCode.Models) != 0 {
		t.Fatalf("models = %#v, want non-nil empty array", got.ClaudeCode.Models)
	}
	if got.ClaudeCode.DefaultModel != "platform-default" {
		t.Fatalf("defaultModel = %q, want platform-default", got.ClaudeCode.DefaultModel)
	}
}
