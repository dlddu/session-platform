//go:build integration

// AC-L1 on the real client-go orchestrator: a session's plugin selection
// reaches exactly the container that installs plugins for its type, and an
// omitted selection leaves each type on its platform default.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func TestPlugins_ClaudeCodeWorkloadContainer(t *testing.T) {
	newOrch := func(t *testing.T) (*k8s.ClientOrchestrator, *fake.Clientset) {
		return newReadyOrchestrator(t,
			k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
			k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
			k8s.WithClaudeCredentialsSecret("provider-credentials"),
			k8s.WithClaudeConfigConfigMap("provider-config"))
	}

	t.Run("selection", func(t *testing.T) {
		orch, cs := newOrch(t)
		if _, err := orch.Start(context.Background(), "pl-cc", k8s.WorkloadSpec{
			Type: session.WorkloadTypeClaudeCode, Plugins: []string{"development", "session-platform"},
		}); err != nil {
			t.Fatalf("start: %v", err)
		}
		pod := listPods(t, cs)[0]
		main := container(t, pod, k8s.ContainerName)
		if got, ok := envOf(main, k8s.ClaudeCodePluginsEnvVar); !ok || got != "development,session-platform" {
			t.Errorf("%s = %q (set %v), want development,session-platform", k8s.ClaudeCodePluginsEnvVar, got, ok)
		}
		for _, c := range pod.Spec.Containers {
			if c.Name == k8s.ContainerName {
				continue
			}
			if _, ok := envVarOf(c, k8s.ClaudeCodePluginsEnvVar); ok {
				t.Errorf("sidecar %s receives %s", c.Name, k8s.ClaudeCodePluginsEnvVar)
			}
		}
	})

	t.Run("omitted leaves the entrypoint default", func(t *testing.T) {
		orch, cs := newOrch(t)
		if _, err := orch.Start(context.Background(), "pl-cc-default", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode}); err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, ok := envVarOf(container(t, listPods(t, cs)[0], k8s.ContainerName), k8s.ClaudeCodePluginsEnvVar); ok {
			t.Errorf("%s set for a session that chose no plugins", k8s.ClaudeCodePluginsEnvVar)
		}
	})

	t.Run("restore keeps the selection", func(t *testing.T) {
		orch, cs := newOrch(t)
		if _, err := orch.RestoreInto(context.Background(), "pl-cc-restore", "archive-ref", k8s.WorkloadSpec{
			Type: session.WorkloadTypeClaudeCode, Plugins: []string{"development"},
		}); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if got, _ := envOf(container(t, listPods(t, cs)[0], k8s.ContainerName), k8s.ClaudeCodePluginsEnvVar); got != "development" {
			t.Errorf("restored %s = %q, want development", k8s.ClaudeCodePluginsEnvVar, got)
		}
	})

	for _, tc := range []struct {
		name string
		spec k8s.WorkloadSpec
	}{
		{"invalid name", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode, Plugins: []string{"a/b"}}},
		{"shell", k8s.WorkloadSpec{Type: session.WorkloadTypeShell, Plugins: []string{"development"}}},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			orch, cs := newOrch(t)
			_, err := orch.Start(context.Background(), "pl-bad", tc.spec)
			if !errors.Is(err, session.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if pods := listPods(t, cs); len(pods) != 0 {
				t.Fatalf("created %d pods for an invalid selection", len(pods))
			}
		})
	}
}

func TestPlugins_MiniMaxCodeHelperSeeder(t *testing.T) {
	t.Run("invalid selection provisions no helper pod", func(t *testing.T) {
		orch, cs := newMiniMaxOrchestrator(t)
		_, err := orch.Start(context.Background(), "pl-mm-bad", k8s.WorkloadSpec{
			Type: session.WorkloadTypeMiniMaxCode, Plugins: []string{"a/b"},
		})
		if !errors.Is(err, session.ErrInvalidInput) {
			t.Fatalf("err = %v, want ErrInvalidInput", err)
		}
		if pods := listPods(t, cs); len(pods) != 0 {
			t.Fatalf("created %d pods for an invalid selection", len(pods))
		}
	})

	t.Run("selection", func(t *testing.T) {
		orch, cs := newMiniMaxOrchestrator(t)
		started, err := orch.Start(context.Background(), "pl-mm", k8s.WorkloadSpec{
			Type: session.WorkloadTypeMiniMaxCode, Plugins: []string{"mcp-servers", "development"},
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		set := newPodSet(t, listPods(t, cs), started)
		proxy := container(t, set.helper, k8s.MiniMaxK3SMCPProxyContainerName)
		if got, ok := envOf(proxy, "MINIMAX_CODE_PLUGINS"); !ok || got != "mcp-servers,development" {
			t.Errorf("MINIMAX_CODE_PLUGINS = %q (set %v), want mcp-servers,development", got, ok)
		}
		if _, ok := envVarOf(container(t, set.workload, k8s.ContainerName), "MINIMAX_CODE_PLUGINS"); ok {
			t.Error("the workload container must not choose plugins; the helper seeder does")
		}
	})

	t.Run("omitted leaves the seeder default", func(t *testing.T) {
		_, set := startMiniMax(t, "pl-mm-default")
		if _, ok := envVarOf(container(t, set.helper, k8s.MiniMaxK3SMCPProxyContainerName), "MINIMAX_CODE_PLUGINS"); ok {
			t.Error("MINIMAX_CODE_PLUGINS set for a session that chose no plugins")
		}
	})
}

func TestPlugins_CodexSidecarSeeder(t *testing.T) {
	start := func(t *testing.T, id string, plugins []string) string {
		t.Helper()
		orch, cs := newReadyOrchestrator(t,
			k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
			k8s.WithWorkloadImage(session.WorkloadTypeCodexChat, codexImage),
			k8s.WithCodexCredentialsSecret("codex-k3s"),
			k8s.WithCodexConfigConfigMap("codex-cfg"),
			k8s.WithCodexPlugins("codex-session,extra"),
		)
		slot := 0
		if _, err := orch.Start(context.Background(), id, k8s.WorkloadSpec{Type: session.WorkloadTypeCodexChat, Plugins: plugins, AuthSlot: &slot}); err != nil {
			t.Fatalf("start: %v", err)
		}
		got, _ := envOf(container(t, listPods(t, cs)[0], k8s.CodexK3SMCPProxyContainerName), "MINIMAX_CODE_PLUGINS")
		return got
	}
	if got := start(t, "pl-cx", []string{"learning"}); got != "learning" {
		t.Errorf("selection: MINIMAX_CODE_PLUGINS = %q, want learning (replaces CODEX_PLUGINS)", got)
	}
	if got := start(t, "pl-cx-default", nil); got != "codex-session,extra" {
		t.Errorf("omitted: MINIMAX_CODE_PLUGINS = %q, want the deployment's codex-session,extra", got)
	}
}

// codex-task seeds from its own sidecar the same way, falling back to its own
// CODEX_TASK_PLUGINS rather than codex-chat's (AC-L1, AC-M1).
func TestPlugins_CodexTaskSidecarSeeder(t *testing.T) {
	start := func(t *testing.T, id string, plugins []string) string {
		t.Helper()
		orch, cs := newReadyOrchestrator(t,
			k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
			k8s.WithWorkloadImage(session.WorkloadTypeCodexTask, codexImage),
			k8s.WithCodexPlugins("codex-session,extra"),
			k8s.WithCodexTaskPlugins("task-bundle"),
		)
		slot := 0
		if _, err := orch.Start(context.Background(), id, k8s.WorkloadSpec{Type: session.WorkloadTypeCodexTask, Plugins: plugins, AuthSlot: &slot}); err != nil {
			t.Fatalf("start: %v", err)
		}
		got, _ := envOf(container(t, listPods(t, cs)[0], k8s.CodexK3SMCPProxyContainerName), "MINIMAX_CODE_PLUGINS")
		return got
	}
	if got := start(t, "pl-ct", []string{"learning"}); got != "learning" {
		t.Errorf("selection: MINIMAX_CODE_PLUGINS = %q, want learning", got)
	}
	if got := start(t, "pl-ct-default", nil); got != "task-bundle" {
		t.Errorf("omitted: MINIMAX_CODE_PLUGINS = %q, want codex-task's own task-bundle", got)
	}
}
