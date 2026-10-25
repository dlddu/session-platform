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
	orch, cs := newMiniMaxOrchestrator(t, append(opts,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))...)
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
	for _, wt := range []session.WorkloadType{session.WorkloadTypeApprovalGated, session.WorkloadTypeMiniMaxCode} {
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
	set := startWithRepoAccess(t, "ra0x", session.WorkloadTypeApprovalGated, false)
	if got := containerNames(set.helper); got != k8s.SessionMCPContainerName+","+k8s.HelperCredentialProxyContainerName {
		t.Errorf("approval-gated helper containers = %s, want the session MCP and provider proxy only", got)
	}
}

func TestRepoAccess_SessionMCPAloneHoldsTheControlPlaneToken(t *testing.T) {
	for _, wt := range []session.WorkloadType{session.WorkloadTypeApprovalGated, session.WorkloadTypeMiniMaxCode} {
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

// AC-H5: approval-gated's helper gains the K3s MCP proxy, on its own Secret's
// token and without minimax-code's marketplace scratch.
func TestRepoAccess_ApprovalGatedHelperRunsAK3SMCPProxy(t *testing.T) {
	set := startWithRepoAccess(t, "ra2", session.WorkloadTypeApprovalGated, true)
	proxy := container(t, set.helper, k8s.MiniMaxK3SMCPProxyContainerName)
	if got, _ := envValue(proxy, "DATA_PLANE_WORKLOAD"); got != "k3s-mcp-proxy" {
		t.Errorf("workload = %q, want k3s-mcp-proxy", got)
	}
	secret, key, ok := secretKey(proxy, k8s.K3SMCPTokenEnvVar)
	if !ok || secret != "claude-provider" || key != "k3s-mcp-token" {
		t.Errorf("token selector = %s/%s (%v), want claude-provider/k3s-mcp-token", secret, key, ok)
	}
	if got, _ := envValue(proxy, k8s.SessionIDEnvVar); got != "ra2" {
		t.Errorf("k3s mcp proxy %s = %q, want ra2", k8s.SessionIDEnvVar, got)
	}
	if _, ok := envValue(proxy, k8s.MiniMaxMarketplaceDirEnvVar); ok || len(proxy.VolumeMounts) != 0 {
		t.Errorf("approval-gated k3s proxy got a marketplace: mounts %+v", proxy.VolumeMounts)
	}
	if name, _ := repoAccessTokenVolume(set.helper); name == "" {
		t.Fatal("no control plane token")
	}
	for _, c := range set.workload.Spec.Containers {
		if _, ok := envValue(c, k8s.K3SMCPProxyURLEnvVar); ok {
			t.Errorf("approval-gated workload container %s learns the K3s MCP proxy address", c.Name)
		}
	}
}

// The proxy is for the session MCP beside it: the boundary stays exactly
// approval-gated's two ports.
func TestRepoAccess_ApprovalGatedBoundaryIsUnchanged(t *testing.T) {
	orch, cs := newMiniMaxOrchestrator(t,
		k8s.WithRepoAccess(repoAccessURL),
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	started, err := orch.Start(context.Background(), "ra3", k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatal(err)
	}
	set := newPodSet(t, listPods(t, cs), started)
	policies := listNetworkPolicies(t, cs)
	egress := policies[set.helper.Name+"-workload-egress"]
	if len(egress.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2", len(egress.Spec.Egress))
	}
	assertPorts(t, "workload egress to helper", egress.Spec.Egress[1].Ports, 8091, 8092)
	assertPorts(t, "helper ingress", policies[set.helper.Name+"-helper-ingress"].Spec.Ingress[0].Ports, 8091, 8092)
}

// AC-H6: git_push builds its scratch repository under the session MCP's temp
// directory, which a read-only root filesystem would otherwise refuse.
func TestRepoAccess_SessionMCPHasAPrivateWritableTmp(t *testing.T) {
	for _, wt := range []session.WorkloadType{session.WorkloadTypeApprovalGated, session.WorkloadTypeMiniMaxCode} {
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
