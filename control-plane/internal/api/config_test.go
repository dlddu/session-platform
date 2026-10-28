package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type runtimeConfigResponse struct {
	ClaudeCode struct {
		DefaultModel string   `json:"defaultModel"`
		Models       []string `json:"models"`
	} `json:"claudeCode"`
	WorkloadTypes []workloadTypeEntry `json:"workloadTypes"`
}

type workloadTypeEntry struct {
	Type         string   `json:"type"`
	Label        string   `json:"label"`
	Description  string   `json:"description"`
	TakesModel   bool     `json:"takesModel"`
	Input        string   `json:"input"`
	DefaultModel *string  `json:"defaultModel"`
	Models       []string `json:"models"`
}

func getRuntimeConfig(t *testing.T, url string) (runtimeConfigResponse, map[string]any) {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/config")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var typed runtimeConfigResponse
	var raw map[string]any
	if err := json.Unmarshal(body, &typed); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return typed, raw
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
	var got runtimeConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if got.ClaudeCode.DefaultModel != "~anthropic/claude-opus-latest" ||
		!reflect.DeepEqual(got.ClaudeCode.Models, []string{"claude-model-a", "claude-model-b"}) {
		t.Errorf("claudeCode = %+v, want the configured default and catalog", got.ClaudeCode)
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

func TestRuntimeConfigListsWorkloadTypes(t *testing.T) {
	srv := newServer(
		api.WithClaudeCodeModelConfig("claude-default", []string{"sonnet", "opus"}),
		api.WithWorkloadModelConfig(session.WorkloadTypeApprovalGated, "claude-default", []string{"sonnet", "opus"}),
	)
	defer srv.Close()

	got, raw := getRuntimeConfig(t, srv.URL)
	order := []string{}
	byType := map[string]workloadTypeEntry{}
	for _, e := range got.WorkloadTypes {
		order = append(order, e.Type)
		byType[e.Type] = e
	}
	if want := []string{"shell", "claude-code", "minimax-code", "approval-gated"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("workload type order = %v, want %v", order, want)
	}
	for _, e := range got.WorkloadTypes {
		if e.Label == "" || e.Description == "" {
			t.Errorf("%s: empty label/description", e.Type)
		}
		if e.Models == nil {
			t.Errorf("%s: models must be a non-null array", e.Type)
		}
	}

	shell := byType["shell"]
	if shell.TakesModel || shell.Input != "terminal" || shell.DefaultModel != nil || len(shell.Models) != 0 {
		t.Errorf("shell = %+v, want terminal, no model, null default, empty catalog", shell)
	}
	// null must be explicit, not an omitted key.
	types := raw["workloadTypes"].([]any)
	if v, ok := types[0].(map[string]any)["defaultModel"]; !ok || v != nil {
		t.Errorf("shell defaultModel = %v (present=%v), want explicit null", v, ok)
	}

	for _, typ := range []string{"claude-code", "approval-gated"} {
		e := byType[typ]
		if !e.TakesModel || e.Input != "prompt" || e.DefaultModel == nil || *e.DefaultModel != "claude-default" ||
			!reflect.DeepEqual(e.Models, []string{"sonnet", "opus"}) {
			t.Errorf("%s = %+v, want prompt with the Claude Secret's default and catalog", typ, e)
		}
	}
	mm := byType["minimax-code"]
	if !mm.TakesModel || mm.Input != "prompt" || mm.DefaultModel == nil || *mm.DefaultModel != "platform-default" || len(mm.Models) != 0 {
		t.Errorf("minimax-code = %+v, want prompt with platform-default and empty catalog", mm)
	}
}

func TestRuntimeConfigListsOnlyOfferedWorkloadTypes(t *testing.T) {
	srv := newServer(api.WithOfferedWorkloadTypes(session.WorkloadTypeShell, session.WorkloadTypeMiniMaxCode))
	defer srv.Close()

	got, _ := getRuntimeConfig(t, srv.URL)
	order := []string{}
	for _, e := range got.WorkloadTypes {
		order = append(order, e.Type)
	}
	if want := []string{"shell", "minimax-code"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("offered types = %v, want %v", order, want)
	}
	// The legacy field stays for existing clients even when claude-code is not offered.
	if got.ClaudeCode.DefaultModel != "platform-default" {
		t.Errorf("claudeCode.defaultModel = %q, want platform-default", got.ClaudeCode.DefaultModel)
	}
}
