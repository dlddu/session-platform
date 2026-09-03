//go:build integration

// The workload type a session was created with has to reach the cluster object,
// not just the control plane's own record. These run against the same fake
// clientset as client_orchestrator_test.go, so they assert the pod spec the
// orchestrator actually submits.
package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const claudeCodeImage = "ghcr.io/dlddu/session-platform-claude-code:dev"

// envOf returns the value of the named env var on the pod's session container.
func envOf(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func envVarOf(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, env := range c.Env {
		if env.Name == name {
			return env, true
		}
	}
	return corev1.EnvVar{}, false
}

func containerNamed(pod corev1.Pod, name string) (corev1.Container, bool) {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return container, true
		}
	}
	return corev1.Container{}, false
}

// The two types must not produce the same pod: image, the workload env var and
// the workload label all have to follow the type.
func TestClientOrchestrator_PodSpecBranchesOnWorkloadType(t *testing.T) {
	const shellImage = "ghcr.io/dlddu/session-platform-data-plane:dev"

	cases := []struct {
		workload       session.WorkloadType
		wantImage      string
		wantContainers int
	}{
		{session.WorkloadTypeShell, shellImage, 1},
		{session.WorkloadTypeClaudeCode, claudeCodeImage, 2},
	}
	for _, tc := range cases {
		orch, cs := newReadyOrchestrator(t,
			k8s.WithImage(shellImage),
			k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage))
		if _, err := orch.Start(context.Background(), "wt01", k8s.WorkloadSpec{Type: tc.workload}); err != nil {
			t.Fatalf("start (%s): %v", tc.workload, err)
		}
		pod := listPods(t, cs)[0]
		if len(pod.Spec.Containers) != tc.wantContainers {
			t.Fatalf("%s: containers = %d, want %d", tc.workload, len(pod.Spec.Containers), tc.wantContainers)
		}
		c := pod.Spec.Containers[0]
		if c.Image != tc.wantImage {
			t.Errorf("%s: image = %q, want %q", tc.workload, c.Image, tc.wantImage)
		}
		if got, ok := envOf(c, "DATA_PLANE_WORKLOAD"); !ok || got != string(tc.workload) {
			t.Errorf("%s: DATA_PLANE_WORKLOAD = %q (present=%v), want %q", tc.workload, got, ok, tc.workload)
		}
		if got := pod.Labels[k8s.LabelWorkloadType]; got != string(tc.workload) {
			t.Errorf("%s: %s label = %q, want %q", tc.workload, k8s.LabelWorkloadType, got, tc.workload)
		}
	}
}

// An unspecified type on the orchestrator boundary is the shell default, the
// same rule the service layer applies — so a caller that predates the type axis
// keeps getting exactly the pod it used to get.
func TestClientOrchestrator_UnspecifiedWorkloadTypeIsShell(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"))
	if _, err := orch.Start(context.Background(), "wt02", k8s.WorkloadSpec{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	pod := listPods(t, cs)[0]
	if got := pod.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeShell) {
		t.Errorf("%s label = %q, want %q", k8s.LabelWorkloadType, got, session.WorkloadTypeShell)
	}
	if got, _ := envOf(pod.Spec.Containers[0], "DATA_PLANE_WORKLOAD"); got != string(session.WorkloadTypeShell) {
		t.Errorf("DATA_PLANE_WORKLOAD = %q, want %q", got, session.WorkloadTypeShell)
	}
}

// Claude Code model selection is normalized at the orchestrator boundary and
// copied into the pod. Invalid settings are rejected before a pod is created,
// including a model on a shell workload.
func TestClientOrchestrator_ModelIsValidatedAndCopiedToPod(t *testing.T) {
	cases := []struct {
		name       string
		id         string
		spec       k8s.WorkloadSpec
		wantModel  string
		wantSecret bool
		wantErr    bool
	}{
		{
			name:       "claude-code defaults from platform secret",
			id:         "model-default",
			spec:       k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode},
			wantSecret: true,
		},
		{
			name: "explicit platform alias also uses platform secret",
			id:   "model-platform-alias",
			spec: k8s.WorkloadSpec{
				Type:  session.WorkloadTypeClaudeCode,
				Model: session.PlatformDefaultModel,
			},
			wantSecret: true,
		},
		{
			name:      "explicit claude-code model",
			id:        "model-explicit",
			spec:      k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode, Model: "claude-sonnet-4-5"},
			wantModel: "claude-sonnet-4-5",
		},
		{
			name:    "invalid claude-code model",
			id:      "model-invalid",
			spec:    k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode, Model: "bad model"},
			wantErr: true,
		},
		{
			name:    "shell rejects model",
			id:      "model-shell",
			spec:    k8s.WorkloadSpec{Type: session.WorkloadTypeShell, Model: "claude-sonnet-4-5"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch, cs := newReadyOrchestrator(t,
				k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
				k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
				k8s.WithClaudeCredentialsSecret("provider-credentials"))
			_, err := orch.Start(context.Background(), tc.id, tc.spec)
			if tc.wantErr {
				if !errors.Is(err, session.ErrInvalidInput) {
					t.Fatalf("err = %v, want ErrInvalidInput", err)
				}
				if pods := listPods(t, cs); len(pods) != 0 {
					t.Fatalf("created %d pods for invalid model, want 0", len(pods))
				}
				return
			}
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			env, ok := envVarOf(listPods(t, cs)[0].Spec.Containers[0], k8s.ClaudeCodeModelEnvVar)
			if !ok {
				t.Fatalf("missing %s", k8s.ClaudeCodeModelEnvVar)
			}
			if tc.wantSecret {
				if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
					t.Fatalf("%s = %+v, want SecretKeyRef", k8s.ClaudeCodeModelEnvVar, env)
				}
				ref := env.ValueFrom.SecretKeyRef
				if ref.Name != "provider-credentials" || ref.Key != k8s.ClaudeCodeModelSecretKey {
					t.Errorf("%s selector = %s/%s, want provider-credentials/%s",
						k8s.ClaudeCodeModelEnvVar, ref.Name, ref.Key, k8s.ClaudeCodeModelSecretKey)
				}
				if ref.Optional == nil || !*ref.Optional {
					t.Errorf("%s selector must be optional for CLI-default compatibility", k8s.ClaudeCodeModelEnvVar)
				}
				return
			}
			if env.Value != tc.wantModel || env.ValueFrom != nil {
				t.Errorf("%s = %+v, want literal %q", k8s.ClaudeCodeModelEnvVar, env, tc.wantModel)
			}
		})
	}
}

// The tool-running Claude container must never receive provider credential
// values. Only a hardened, separate-PID-namespace localhost proxy holds them; the
// main container sees a non-secret provider placeholder, the optional model key,
// and its separate required K3s MCP SecretKeyRef.
func TestClientOrchestrator_ClaudeProviderCredentialsAreIsolatedAndK3sTokenIsSecretBacked(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
		k8s.WithClaudeCredentialsSecret("provider-credentials"),
		k8s.WithCheckpointPrivileged(true),
	)
	if _, err := orch.Start(context.Background(), "credential-isolation", k8s.WorkloadSpec{
		Type: session.WorkloadTypeClaudeCode,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	pod := listPods(t, cs)[0]
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want agent + credential proxy", len(pod.Spec.Containers))
	}
	if pod.Spec.ServiceAccountName != k8s.DataPlaneServiceAccountName {
		t.Fatalf("service account = %q, want %q", pod.Spec.ServiceAccountName, k8s.DataPlaneServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || !*pod.Spec.AutomountServiceAccountToken {
		t.Fatal("session pod must mount its read-only Kubernetes service-account token")
	}
	if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
		t.Fatal("credential proxy must keep its separate PID namespace")
	}

	main, ok := containerNamed(pod, k8s.ContainerName)
	if !ok {
		t.Fatalf("missing main container %q", k8s.ContainerName)
	}
	for _, env := range main.Env {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := env.ValueFrom.SecretKeyRef
		isOptionalModel := env.Name == k8s.ClaudeCodeModelEnvVar &&
			ref.Name == "provider-credentials" && ref.Key == k8s.ClaudeCodeModelSecretKey &&
			ref.Optional != nil && *ref.Optional
		isRequiredK3SToken := env.Name == k8s.K3SMCPTokenEnvVar &&
			ref.Name == "provider-credentials" && ref.Key == k8s.ClaudeCodeK3SMCPTokenSecretKey &&
			(ref.Optional == nil || !*ref.Optional)
		if !isOptionalModel && !isRequiredK3SToken {
			t.Fatalf("main container has unexpected Secret env %s: %+v", env.Name, env)
		}
	}
	baseURL, ok := envVarOf(main, k8s.AnthropicBaseURLEnvVar)
	if !ok || baseURL.Value != k8s.ClaudeCredentialProxyURL || baseURL.ValueFrom != nil {
		t.Fatalf("main base URL = %q (present=%v), want %q", baseURL.Value, ok, k8s.ClaudeCredentialProxyURL)
	}
	placeholder, ok := envVarOf(main, k8s.AnthropicAuthTokenEnvVar)
	if !ok || placeholder.Value != "session-platform-proxy" || placeholder.ValueFrom != nil {
		t.Fatalf("main auth token = %q (present=%v), want non-secret proxy placeholder", placeholder.Value, ok)
	}
	model, ok := envVarOf(main, k8s.ClaudeCodeModelEnvVar)
	if !ok || model.ValueFrom == nil || model.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("main model is not Secret-backed: %+v", model)
	}
	k3sToken, ok := envVarOf(main, k8s.K3SMCPTokenEnvVar)
	if !ok || k3sToken.ValueFrom == nil || k3sToken.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("main K3s MCP token is not Secret-backed: %+v", k3sToken)
	}
	k3sTokenRef := k3sToken.ValueFrom.SecretKeyRef
	if k3sTokenRef.Name != "provider-credentials" || k3sTokenRef.Key != k8s.ClaudeCodeK3SMCPTokenSecretKey ||
		(k3sTokenRef.Optional != nil && *k3sTokenRef.Optional) {
		t.Fatalf("main K3s MCP token selector = %+v, want required provider-credentials/%s",
			k3sTokenRef, k8s.ClaudeCodeK3SMCPTokenSecretKey)
	}
	if main.SecurityContext != nil && main.SecurityContext.Privileged != nil && *main.SecurityContext.Privileged {
		t.Fatal("claude-code main container became privileged under the CRIU gate")
	}
	stateDir, ok := envVarOf(main, "CLAUDE_CODE_STATE_DIR")
	if !ok || stateDir.Value != "/session/state" || stateDir.ValueFrom != nil {
		t.Fatalf("claude state dir = %+v (present=%v), want /session/state", stateDir, ok)
	}
	if len(main.VolumeMounts) != 1 || main.VolumeMounts[0].MountPath != "/session" {
		t.Fatalf("claude state mount = %+v, want one /session mount", main.VolumeMounts)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].EmptyDir == nil ||
		pod.Spec.Volumes[0].Name != main.VolumeMounts[0].Name {
		t.Fatalf("claude state volume = %+v, want one matching emptyDir", pod.Spec.Volumes)
	}

	proxy, ok := containerNamed(pod, k8s.ClaudeCredentialsContainerName)
	if !ok {
		t.Fatalf("missing credential container %q", k8s.ClaudeCredentialsContainerName)
	}
	if len(proxy.VolumeMounts) != 0 {
		t.Fatalf("credential proxy must not mount Claude session state: %+v", proxy.VolumeMounts)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_WORKLOAD"); got != "credential-proxy" {
		t.Fatalf("proxy workload = %q, want credential-proxy", got)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_AGENT_ADDR"); got != "127.0.0.1:8091" {
		t.Fatalf("proxy listen address = %q, want loopback", got)
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{k8s.AnthropicBaseURLEnvVar, k8s.ClaudeCodeBaseURLSecretKey},
		{k8s.AnthropicAuthTokenEnvVar, k8s.ClaudeCodeAuthTokenSecretKey},
	} {
		env, ok := envVarOf(proxy, tc.name)
		if !ok || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("proxy env %s is not Secret-backed: %+v", tc.name, env)
		}
		selector := env.ValueFrom.SecretKeyRef
		if selector.Name != "provider-credentials" || selector.Key != tc.key {
			t.Fatalf("proxy env %s selector = %s/%s, want provider-credentials/%s", tc.name, selector.Name, selector.Key, tc.key)
		}
		if selector.Optional != nil && *selector.Optional {
			t.Fatalf("proxy credential env %s must remain required", tc.name)
		}
	}
	if _, ok := envVarOf(proxy, k8s.ClaudeCodeModelEnvVar); ok {
		t.Fatalf("credential proxy must not receive %s", k8s.ClaudeCodeModelEnvVar)
	}
	if _, ok := envVarOf(proxy, k8s.K3SMCPTokenEnvVar); ok {
		t.Fatalf("credential proxy must not receive %s", k8s.K3SMCPTokenEnvVar)
	}
	if proxy.ReadinessProbe == nil || proxy.ReadinessProbe.Exec == nil {
		t.Fatal("loopback credential proxy needs an in-container readiness probe")
	}
	security := proxy.SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
		t.Fatalf("credential proxy security context is not hardened: %+v", security)
	}
}

// A type with no image configured must fail loudly and provision nothing. The
// alternative — silently falling back to the shell image — would hand the
// caller a session that claims a workload it is not running, which is the exact
// "gate-behind-a-stub" shape the convergence model rejects.
func TestClientOrchestrator_UnconfiguredWorkloadTypeIsRefused(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"))
	_, err := orch.Start(context.Background(), "wt03", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode})
	if err == nil {
		t.Fatal("start with an unconfigured workload type succeeded, want an error")
	}
	if !strings.Contains(err.Error(), string(session.WorkloadTypeClaudeCode)) {
		t.Errorf("error %q does not name the workload type", err)
	}
	if pods := listPods(t, cs); len(pods) != 0 {
		t.Errorf("created %d pods for a refused workload type, want 0", len(pods))
	}
}

// A restore never changes the type, so the restore-target pod carries the same
// image, env and label as the pod that was frozen.
func TestClientOrchestrator_RestoreKeepsWorkloadType(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage))

	const model = "claude-sonnet-4-5"
	ref, err := orch.RestoreInto(context.Background(), "wt04", "s3://bucket/wt04.tar", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode, Model: model})
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	var pod *corev1.Pod
	pods := listPods(t, cs)
	for i := range pods {
		if pods[i].Name == ref.Name {
			pod = &pods[i]
			break
		}
	}
	if pod == nil {
		t.Fatalf("restore pod %q not found", ref.Name)
	}
	if got, _ := envOf(pod.Spec.Containers[0], k8s.ClaudeCodeModelEnvVar); got != model {
		t.Errorf("restore %s = %q, want %q", k8s.ClaudeCodeModelEnvVar, got, model)
	}
	if env, _ := envVarOf(pod.Spec.Containers[0], k8s.ClaudeCodeModelEnvVar); env.ValueFrom != nil {
		t.Errorf("restore %s = %+v, want literal explicit model", k8s.ClaudeCodeModelEnvVar, env)
	}
	if got := pod.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeClaudeCode) {
		t.Errorf("%s label = %q, want %q", k8s.LabelWorkloadType, got, session.WorkloadTypeClaudeCode)
	}
	if got := pod.Spec.Containers[0].Image; got != claudeCodeImage {
		t.Errorf("restore image = %q, want %q", got, claudeCodeImage)
	}
	if _, ok := pod.Annotations[k8s.AnnotationRestoreArchive]; !ok {
		t.Errorf("restore pod is missing %s", k8s.AnnotationRestoreArchive)
	}
	if _, ok := pod.Annotations[k8s.AnnotationRestoreCheckpoint]; ok {
		t.Errorf("claude-code restore pod unexpectedly carries CRIU annotation %s", k8s.AnnotationRestoreCheckpoint)
	}
}

func TestClientOrchestrator_RestorePlatformDefaultModelUsesSecret(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
		k8s.WithClaudeCredentialsSecret("provider-credentials"))

	ref, err := orch.RestoreInto(context.Background(), "wt05", "s3://bucket/wt05.tar", k8s.WorkloadSpec{
		Type:  session.WorkloadTypeClaudeCode,
		Model: session.PlatformDefaultModel,
	})
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	var restored *corev1.Pod
	pods := listPods(t, cs)
	for i := range pods {
		if pods[i].Name == ref.Name {
			restored = &pods[i]
			break
		}
	}
	if restored == nil {
		t.Fatalf("restore pod %q not found", ref.Name)
	}
	env, ok := envVarOf(restored.Spec.Containers[0], k8s.ClaudeCodeModelEnvVar)
	if !ok || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("restore %s = %+v, want SecretKeyRef", k8s.ClaudeCodeModelEnvVar, env)
	}
	selector := env.ValueFrom.SecretKeyRef
	if selector.Name != "provider-credentials" || selector.Key != k8s.ClaudeCodeModelSecretKey ||
		selector.Optional == nil || !*selector.Optional {
		t.Fatalf("restore %s selector = %+v, want optional provider-credentials/%s",
			k8s.ClaudeCodeModelEnvVar, selector, k8s.ClaudeCodeModelSecretKey)
	}
}
