package k8s

import (
	"github.com/dlddu/session-platform/control-plane/internal/session"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"testing"
)

func TestSessionDBMountIsolation(t *testing.T) {
	for _, id := range []string{"parent", "other"} {
		c := corev1.Container{}
		s := []corev1.Container{{Name: CodexK3SMCPProxyContainerName}}
		init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexChat, "image", id)
		if err != nil {
			t.Fatal(err)
		}
		if s[0].VolumeMounts[0].SubPath != "db/"+id || !strings.Contains(init[0].Command[2], "/slot/db/"+id) {
			t.Fatal("wrong isolated path", s, init)
		}
	}
	for _, id := range []string{"", "../other", "a/b", "a;touch bad"} {
		if _, err := attachSessionDB(&corev1.Container{}, nil, session.WorkloadTypeCodexChat, "image", id); err == nil {
			t.Fatal("unsafe ID accepted", id)
		}
	}
	c := corev1.Container{}
	s := []corev1.Container{{Name: CodexK3SMCPProxyContainerName}}
	if init, err := attachSessionDB(&c, s, session.WorkloadTypeCodexTask, "image", "worker"); err != nil || len(init) != 0 || len(c.Env) != 0 || len(s[0].VolumeMounts) != 0 {
		t.Fatal("worker received parent DB")
	}
}
