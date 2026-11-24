package k8s

import (
	"github.com/dlddu/session-platform/control-plane/internal/session"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestSessionDBMountIsSlotScoped(t *testing.T) {
	c := corev1.Container{}
	s := []corev1.Container{{Name: K3SMCPProxyContainerName}}
	init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexManager, "image", "codex-config")
	if err != nil {
		t.Fatal(err)
	}
	if s[0].VolumeMounts[0].SubPath != "db" {
		t.Fatalf("sidecar DB subPath = %q, want slot-level %q", s[0].VolumeMounts[0].SubPath, "db")
	}
	if !strings.Contains(init[0].Command[2], "mkdir -p /slot/auth /slot/db;") {
		t.Fatalf("slot-init does not prepare the slot-level db/: %q", init[0].Command[2])
	}
	c = corev1.Container{}
	s = []corev1.Container{{Name: K3SMCPProxyContainerName}}
	if init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexTask, "image", "codex-config"); err != nil || len(init) != 0 || len(c.Env) != 0 || len(s[0].VolumeMounts) != 0 {
		t.Fatal("worker received the chat DB")
	}
}

// AC-N6: the raw SQL debug opt-in reaches the sidecar from the deployment's
// ConfigMap, optionally, and never from the session or the workload container.
func TestSessionDBRawSQLComesFromTheDeployment(t *testing.T) {
	c := corev1.Container{}
	s := []corev1.Container{{Name: K3SMCPProxyContainerName}}
	if _, err := attachSessionDB(&c, s, session.WorkloadTypeCodexManager, "image", "codex-config"); err != nil {
		t.Fatal(err)
	}
	var env *corev1.EnvVar
	for i := range s[0].Env {
		if s[0].Env[i].Name == SessionDBRawSQLEnvVar {
			env = &s[0].Env[i]
		}
	}
	if env == nil {
		t.Fatalf("sidecar env = %+v, want %s", s[0].Env, SessionDBRawSQLEnvVar)
	}
	ref := env.ValueFrom.ConfigMapKeyRef
	if ref.Name != "codex-config" || ref.Key != CodexSessionDBRawSQLConfigKey || ref.Optional == nil || !*ref.Optional {
		t.Fatalf("%s = %+v, want an optional %s key", SessionDBRawSQLEnvVar, ref, CodexSessionDBRawSQLConfigKey)
	}
	for _, e := range c.Env {
		if e.Name == SessionDBRawSQLEnvVar {
			t.Fatal("the workload container received the raw SQL switch")
		}
	}
}
