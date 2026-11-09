package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-L1: the create wire contract takes an optional plugin list, echoes it,
// copies it to provisioning, and refuses anything that is not a valid
// selection for the type before a pod exists.
func TestCreateSessionPlugins(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	t.Run("omitted keeps the type default", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "pl-default", "workloadType": "claude-code"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.Plugins != nil || orch.PluginsFor(s.ID) != nil {
			t.Errorf("plugins = %#v, orchestrator = %#v, want both absent", s.Plugins, orch.PluginsFor(s.ID))
		}
	})

	for _, workload := range []string{"claude-code", "codex-chat", "minimax-code"} {
		t.Run(workload+" selection", func(t *testing.T) {
			want := []string{"development", "session-platform"}
			status, s := createRaw(t, srv.URL, map[string]any{
				"name": "pl-" + workload, "workloadType": workload, "plugins": want,
			})
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want 201", status)
			}
			if !reflect.DeepEqual(s.Plugins, want) {
				t.Errorf("plugins = %#v, want %#v", s.Plugins, want)
			}
			if got := orch.PluginsFor(s.ID); !reflect.DeepEqual(got, want) {
				t.Errorf("orchestrator plugins = %#v, want %#v", got, want)
			}
		})
	}

	rejected := []struct {
		name string
		body map[string]any
	}{
		{"null", map[string]any{"name": "x", "workloadType": "claude-code", "plugins": nil}},
		{"empty", map[string]any{"name": "x", "workloadType": "claude-code", "plugins": []string{}}},
		{"string", map[string]any{"name": "x", "workloadType": "claude-code", "plugins": "development"}},
		{"non-string item", map[string]any{"name": "x", "workloadType": "claude-code", "plugins": []any{1}}},
		{"duplicate", map[string]any{"name": "x", "workloadType": "codex-chat", "plugins": []string{"a", "a"}}},
		{"bad name", map[string]any{"name": "x", "workloadType": "codex-chat", "plugins": []string{"a/b"}}},
		{"shell", map[string]any{"name": "x", "plugins": []string{"development"}}},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			before := orch.RunningCount()
			status, _ := createRaw(t, srv.URL, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if after := orch.RunningCount(); after != before {
				t.Errorf("running pods %d -> %d: a rejected create provisioned", before, after)
			}
		})
	}
}

// AC-L1: the selection is as immutable as the model — a snapshot keeps it and
// the restore pod is provisioned with it, not with the type default.
func TestPluginsSurviveSnapshotRestoreOverHTTP(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	want := []string{"development"}
	status, created := createRaw(t, srv.URL, map[string]any{
		"name": "pl-round-trip", "workloadType": "claude-code", "plugins": want,
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", status)
	}
	post := func(path string) session.Session {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+created.ID+path, "application/json", nil)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		var s session.Session
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return s
	}
	if frozen := post("/snapshot"); frozen.State != session.StateSnapshot || !reflect.DeepEqual(frozen.Plugins, want) {
		t.Errorf("frozen state/plugins = %q/%#v, want snapshot/%#v", frozen.State, frozen.Plugins, want)
	}
	if restored := post("/switch"); restored.State != session.StateActive || !reflect.DeepEqual(restored.Plugins, want) {
		t.Errorf("restored state/plugins = %q/%#v, want active/%#v", restored.State, restored.Plugins, want)
	}
	if got := orch.PluginsFor(created.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("restored orchestrator plugins = %#v, want %#v", got, want)
	}
}

// AC-L1: the config catalog tells clients which types take a plugin list.
func TestConfigReportsTakesPlugins(t *testing.T) {
	srv, _, _ := newServerWithOrchestrator()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cfg struct {
		WorkloadTypes []struct {
			Type         session.WorkloadType `json:"type"`
			TakesPlugins *bool                `json:"takesPlugins"`
		} `json:"workloadTypes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkloadTypes) == 0 {
		t.Fatal("config lists no workload types")
	}
	for _, wt := range cfg.WorkloadTypes {
		if wt.TakesPlugins == nil {
			t.Errorf("%q has no takesPlugins", wt.Type)
			continue
		}
		if *wt.TakesPlugins != session.TakesPlugins(wt.Type) {
			t.Errorf("%q takesPlugins = %v, want %v", wt.Type, *wt.TakesPlugins, session.TakesPlugins(wt.Type))
		}
	}
}
