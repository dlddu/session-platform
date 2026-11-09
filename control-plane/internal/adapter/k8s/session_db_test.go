package k8s

import (
	"github.com/dlddu/session-platform/control-plane/internal/session"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestSessionDBMountIsSlotScoped(t *testing.T) {
	c := corev1.Container{}
	s := []corev1.Container{{Name: CodexK3SMCPProxyContainerName}}
	init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexChat, "image")
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
	s = []corev1.Container{{Name: CodexK3SMCPProxyContainerName}}
	if init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexTask, "image"); err != nil || len(init) != 0 || len(c.Env) != 0 || len(s[0].VolumeMounts) != 0 {
		t.Fatal("worker received the chat DB")
	}
}
