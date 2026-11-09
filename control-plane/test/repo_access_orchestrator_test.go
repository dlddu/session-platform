//go:build integration

// Helper pod shape with repo access on (docs/prd/repo-access.md, AC-H3/AC-H5).
package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const repoAccessURL = "http://control-plane.session-platform.svc.cluster.local"

func startWithRepoAccess(t *testing.T, sessionID string, workloadType session.WorkloadType, on bool) podSet {
	t.Helper()
	var opts []k8s.Option
	if on {
		opts = append(opts, k8s.WithRepoAccess(repoAccessURL+"/"))
	}
	orch, cs := newMiniMaxOrchestrator(t, opts...)
	started, err := orch.Start(context.Background(), sessionID, k8s.WorkloadSpec{Type: workloadType})
	if err != nil {
		t.Fatalf("start %s session: %v", workloadType, err)
	}
	return newPodSet(t, listPods(t, cs), started)
}

// repoAccessTokenVolume returns the helper volume projecting the control
// plane's audience, if any.
func repoAccessTokenVolume(pod corev1.Pod) (string, *corev1.ServiceAccountTokenProjection) {
	for _, v := range pod.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if sa := src.ServiceAccountToken; sa != nil && sa.Audience == k8s.RepoAccessTokenAudience {
				return v.Name, sa
			}
		}
	}
	return "", nil
}

func TestRepoAccess_OffLeavesHelperPodsAsTheyWere(t *testing.T) {
	for _, wt := range gatedTypes {
		set := startWithRepoAccess(t, "ra0"+string(wt[0]), wt, false)
		if name, _ := repoAccessTokenVolume(set.helper); name != "" {
			t.Errorf("%s: repo access token projected while the feature is off", wt)
		}
		mcp := container(t, set.helper, k8s.SessionMCPContainerName)
		for _, env := range []string{k8s.RepoAccessURLEnvVar, k8s.RepoAccessTokenFileEnvVar, k8s.K3SMCPProxyURLEnvVar} {
			if _, ok := envValue(mcp, env); ok {
				t.Errorf("%s: session MCP has %s while the feature is off", wt, env)
			}
		}
	}
}

func TestRepoAccess_SessionMCPAloneHoldsTheControlPlaneToken(t *testing.T) {
	for _, wt := range gatedTypes {
		set := startWithRepoAccess(t, "ra1"+string(wt[0]), wt, true)
		mcp := container(t, set.helper, k8s.SessionMCPContainerName)
		if got, _ := envValue(mcp, k8s.RepoAccessURLEnvVar); got != repoAccessURL {
			t.Errorf("%s: %s = %q, want %q", wt, k8s.RepoAccessURLEnvVar, got, repoAccessURL)
		}
		if got, _ := envValue(mcp, k8s.K3SMCPProxyURLEnvVar); got != "http://127.0.0.1:8093" {
			t.Errorf("%s: session MCP %s = %q, want the loopback proxy", wt, k8s.K3SMCPProxyURLEnvVar, got)
		}
		volume, sa := repoAccessTokenVolume(set.helper)
		if sa == nil {
			t.Fatalf("%s: no projected token for %q", wt, k8s.RepoAccessTokenAudience)
		}
		if sa.ExpirationSeconds == nil || *sa.ExpirationSeconds > 3600 {
			t.Errorf("%s: token expiry = %v, want at most an hour", wt, sa.ExpirationSeconds)
		}
		tokenFile, _ := envValue(mcp, k8s.RepoAccessTokenFileEnvVar)
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
					t.Errorf("%s: container %s/%s mounts the control plane token = %v", wt, pod.Name, c.Name, mounted)
				}
				if _, ok := envValue(c, k8s.RepoAccessURLEnvVar); ok && !isMCP {
					t.Errorf("%s: container %s/%s receives %s", wt, pod.Name, c.Name, k8s.RepoAccessURLEnvVar)
				}
			}
		}
	}
}

// AC-H6: git_push builds its scratch repository under the session MCP's temp
// directory, which a read-only root filesystem would otherwise refuse.
func TestRepoAccess_SessionMCPHasAPrivateWritableTmp(t *testing.T) {
	for _, wt := range gatedTypes {
		set := startWithRepoAccess(t, "ra4"+string(wt[0]), wt, true)
		mcp := container(t, set.helper, k8s.SessionMCPContainerName)
		volume := ""
		for _, m := range mcp.VolumeMounts {
			if m.MountPath == k8s.SessionMCPScratchDir {
				if m.ReadOnly {
					t.Errorf("%s: %s is mounted read-only", wt, k8s.SessionMCPScratchDir)
				}
				volume = m.Name
			}
		}
		if volume == "" {
			t.Fatalf("%s: session MCP mounts nothing at %s (mounts %+v)", wt, k8s.SessionMCPScratchDir, mcp.VolumeMounts)
		}
		for _, v := range set.helper.Spec.Volumes {
			if v.Name == volume && v.EmptyDir == nil {
				t.Errorf("%s: %s is backed by %+v, want an emptyDir", wt, k8s.SessionMCPScratchDir, v.VolumeSource)
			}
		}
		for _, pod := range []corev1.Pod{set.workload, set.helper} {
			for _, c := range pod.Spec.Containers {
				if pod.Name == set.helper.Name && c.Name == k8s.SessionMCPContainerName {
					continue
				}
				for _, m := range c.VolumeMounts {
					if pod.Name == set.helper.Name && m.Name == volume {
						t.Errorf("%s: container %s also mounts the session MCP scratch", wt, c.Name)
					}
				}
			}
		}
	}
}
