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
	init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexManager, "image", "codex-config", 8080)
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
	if init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexTask, "image", "codex-config", 8080); err != nil || len(init) != 0 || len(c.Env) != 0 || len(s[0].VolumeMounts) != 0 {
		t.Fatal("worker received the chat DB")
	}
}

// AC-N6: the raw SQL debug opt-in reaches the sidecar from the deployment's
// ConfigMap, optionally, and never from the session or the workload container.
func TestSessionDBRawSQLComesFromTheDeployment(t *testing.T) {
	c := corev1.Container{}
	s := []corev1.Container{{Name: K3SMCPProxyContainerName}}
	if _, err := attachSessionDB(&c, s, session.WorkloadTypeCodexManager, "image", "codex-config", 8080); err != nil {
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

// AC-N11: the sidecar that holds the slot's database is what starts the monitor
// lane's turn when the parent hands it a message, so it needs the lane's own
// address and the same opt-in the workload reads. The address is this pod's
// loopback — the lanes share one listener and a lane is a path prefix — so a
// sidecar can only ever wake the conversation beside it.
func TestSessionDBSidecarCanWakeTheMonitorLane(t *testing.T) {
	c := corev1.Container{}
	s := []corev1.Container{{Name: K3SMCPProxyContainerName}}
	if _, err := attachSessionDB(&c, s, session.WorkloadTypeCodexManager, "image", "codex-config", 8080); err != nil {
		t.Fatal(err)
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range s[0].Env {
		env[e.Name] = e
	}
	wake, ok := env[ManagerMonitorWriteURLEnvVar]
	if !ok {
		t.Fatalf("sidecar env = %+v, want %s", s[0].Env, ManagerMonitorWriteURLEnvVar)
	}
	if wake.Value != "http://127.0.0.1:8080/monitor/write" {
		t.Fatalf("%s = %q, want the monitor lane's write on this pod's loopback",
			ManagerMonitorWriteURLEnvVar, wake.Value)
	}
	optIn, ok := env[ManagerMonitorLaneEnvVar]
	if !ok || optIn.ValueFrom == nil || optIn.ValueFrom.ConfigMapKeyRef == nil {
		t.Fatalf("the sidecar did not get %s from the deployment's ConfigMap: %+v",
			ManagerMonitorLaneEnvVar, env[ManagerMonitorLaneEnvVar])
	}
	ref := optIn.ValueFrom.ConfigMapKeyRef
	if ref.Name != "codex-config" || ref.Key != CodexManagerMonitorLaneConfigKey || ref.Optional == nil || !*ref.Optional {
		t.Fatalf("%s = %+v, want an optional %s key — one opt-in, read the same way on both containers",
			ManagerMonitorLaneEnvVar, ref, CodexManagerMonitorLaneConfigKey)
	}
	// The monitor's conversation is the workload container's business; a sidecar
	// that knew where it lived could write into the tree it must not touch.
	if _, leaked := env[ManagerMonitorStateDirEnvVar]; leaked {
		t.Fatalf("the sidecar received %s", ManagerMonitorStateDirEnvVar)
	}
	// A type with no second lane gets no address to wake one.
	c = corev1.Container{}
	s = []corev1.Container{{Name: K3SMCPProxyContainerName}}
	if _, err := attachSessionDB(&c, s, session.WorkloadTypeCodexTask, "image", "codex-config", 8080); err != nil {
		t.Fatal(err)
	}
	for _, e := range s[0].Env {
		if e.Name == ManagerMonitorWriteURLEnvVar {
			t.Fatal("a worker's sidecar received a monitor lane address")
		}
	}
}
