//go:build integration

// drop 파일 전달이 켜졌을 때의 헬퍼 파드 모양(docs/prd/drop-file-share.md AC-I2).
package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const dropURL = "http://drop-external.drop.svc.cluster.local"

func startWithDrop(t *testing.T, sessionID string, workloadType session.WorkloadType, on bool) podSet {
	t.Helper()
	var opts []k8s.Option
	if on {
		opts = append(opts, k8s.WithDropURL(" "+dropURL+"/ "))
	}
	orch, cs := newMiniMaxOrchestrator(t, opts...)
	started, err := orch.Start(context.Background(), sessionID, k8s.WorkloadSpec{Type: workloadType})
	if err != nil {
		t.Fatalf("start %s session: %v", workloadType, err)
	}
	return newPodSet(t, listPods(t, cs), started)
}

func dropTokenVolume(pod corev1.Pod) (string, *corev1.ServiceAccountTokenProjection) {
	for _, v := range pod.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if sa := src.ServiceAccountToken; sa != nil && sa.Audience == k8s.DropTokenAudience {
				return v.Name, sa
			}
		}
	}
	return "", nil
}

func TestDrop_OffLeavesHelperPodsAsTheyWere(t *testing.T) {
	for _, wt := range gatedTypes {
		set := startWithDrop(t, "dr0"+string(wt[0]), wt, false)
		if name, _ := dropTokenVolume(set.helper); name != "" {
			t.Errorf("%s: drop token projected while DROP_URL is unset", wt)
		}
		mcp := container(t, set.helper, k8s.SessionMCPContainerName)
		for _, env := range []string{k8s.DropURLEnvVar, k8s.DropTokenFileEnvVar} {
			if _, ok := envValue(mcp, env); ok {
				t.Errorf("%s: session MCP has %s while DROP_URL is unset", wt, env)
			}
		}
	}
}

func TestDrop_SessionMCPAloneHoldsTheDropToken(t *testing.T) {
	for _, wt := range gatedTypes {
		set := startWithDrop(t, "dr1"+string(wt[0]), wt, true)
		mcp := container(t, set.helper, k8s.SessionMCPContainerName)
		if got, _ := envValue(mcp, k8s.DropURLEnvVar); got != dropURL {
			t.Errorf("%s: %s = %q, want %q", wt, k8s.DropURLEnvVar, got, dropURL)
		}
		volume, sa := dropTokenVolume(set.helper)
		if sa == nil {
			t.Fatalf("%s: no projected token for audience %q", wt, k8s.DropTokenAudience)
		}
		if sa.ExpirationSeconds == nil || *sa.ExpirationSeconds > 3600 {
			t.Errorf("%s: token expiry = %v, want at most an hour", wt, sa.ExpirationSeconds)
		}
		tokenFile, _ := envValue(mcp, k8s.DropTokenFileEnvVar)
		if tokenFile != "/var/run/secrets/drop/token" {
			t.Errorf("%s: %s = %q", wt, k8s.DropTokenFileEnvVar, tokenFile)
		}
		if set.helper.Spec.AutomountServiceAccountToken == nil || *set.helper.Spec.AutomountServiceAccountToken {
			t.Errorf("%s: helper pod automounts an apiserver token", wt)
		}
		if set.helper.Spec.ServiceAccountName != k8s.SessionMCPServiceAccountName {
			t.Errorf("%s: helper runs as %q — drop's allow list names session-mcp", wt, set.helper.Spec.ServiceAccountName)
		}
		for _, pod := range []corev1.Pod{set.workload, set.helper} {
			for _, c := range pod.Spec.Containers {
				isMCP := pod.Name == set.helper.Name && c.Name == k8s.SessionMCPContainerName
				mounted := false
				for _, m := range c.VolumeMounts {
					if m.Name == volume && pod.Name == set.helper.Name {
						mounted = true
						if !m.ReadOnly || tokenFile != m.MountPath+"/"+sa.Path {
							t.Errorf("%s: mount %+v does not serve %s read-only", wt, m, tokenFile)
						}
					}
				}
				if mounted != isMCP {
					t.Errorf("%s: container %s/%s mounts the drop token = %v", wt, pod.Name, c.Name, mounted)
				}
				for _, env := range []string{k8s.DropURLEnvVar, k8s.DropTokenFileEnvVar} {
					if _, ok := envValue(c, env); ok && !isMCP {
						t.Errorf("%s: container %s/%s receives %s", wt, pod.Name, c.Name, env)
					}
				}
			}
		}
	}
}

// drop 은 헬퍼 파드가 나가는 곳이라 워크로드 파드의 경계는 움직이지 않는다(AC-F2 그대로).
func TestDrop_WorkloadBoundaryIsUnchanged(t *testing.T) {
	orch, cs := newMiniMaxOrchestrator(t, k8s.WithDropURL(dropURL))
	started, err := orch.Start(context.Background(), "dr2", k8s.WorkloadSpec{Type: gatedType})
	if err != nil {
		t.Fatal(err)
	}
	set := newPodSet(t, listPods(t, cs), started)
	policies := listNetworkPolicies(t, cs)
	egress := policies[set.helper.Name+"-workload-egress"]
	if len(egress.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2", len(egress.Spec.Egress))
	}
	assertPorts(t, "workload egress to helper", egress.Spec.Egress[1].Ports, gatedHelperPorts...)
}
