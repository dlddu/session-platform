//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 2
//
// AC-F2's network boundary, enforced — not just present — on the deployed SUT.
// kind's kindnet enforces NetworkPolicy since v0.24.0, so a refused connection
// here is the policy at work. Every refusal is paired with a control: the same
// destination dialled from a pod the policy does not confine (the session's
// helper pod) must connect, or the refusal would prove nothing beyond "that
// address is dead in this cluster".
package e2e_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// $1 host, $2 port. Prints exactly "open" or "closed". bash's /dev/tcp dials
// without speaking any protocol, so the verdict is about the connection alone.
const f2DialProbe = `if timeout 4 bash -c 'exec 3<>"/dev/tcp/$1/$2"' _ "$1" "$2" 2>/dev/null; then echo open; else echo closed; fi`

func (f f6Session) dial(t *testing.T, pod, container, host, port string) string {
	t.Helper()
	return strings.TrimSpace(f.sh(t, pod, container, f2DialProbe, host, port))
}

// The control must connect from the unconfined pod first; only then does the
// confined pod's refusal say anything.
func (f f6Session) mustBeBlockedFromWorkload(t *testing.T, controlContainer, host, port string) {
	t.Helper()
	if got := f.dial(t, f.helper.Name, controlContainer, host, port); got != "open" {
		t.Fatalf("control: %s:%s from helper %s/%s = %s, want open — the workload-side refusal below would be vacuous",
			host, port, f.helper.Name, controlContainer, got)
	}
	if got := f.dial(t, f.work.Name, workloadContainer, host, port); got != "closed" {
		t.Fatalf("workload pod %s reached %s:%s (%s) — AC-F2's egress allowlist admits only kube-dns and its own helper",
			f.work.Name, host, port, got)
	}
}

func (f f6Session) sessionPolicies(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, err := f.cs.NetworkingV1().NetworkPolicies(f.ns).List(ctx, metav1.ListOptions{
		LabelSelector: "session-id=" + f.session.ID,
	})
	if err != nil {
		t.Fatalf("list network policies for session %s: %v", f.session.ID, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		names = append(names, p.Name)
	}
	return names
}

func TestApprovalGatedEgressBoundary_IsEnforcedOnTheDeployedSUT(t *testing.T) {
	s1, ok := newF6Session(t, map[string]any{"name": uniqueName(t), "workloadType": "approval-gated"})
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — enforcement is observed from inside the pods")
	}
	s2, _ := newF6Session(t, map[string]any{"name": uniqueName(t) + "-b", "workloadType": "approval-gated"})

	s1Helper := s1.helper.Status.PodIP
	s2Helper := s2.helper.Status.PodIP
	if s1Helper == "" || s2Helper == "" {
		t.Fatalf("helper pod IPs not assigned (s1=%q s2=%q)", s1Helper, s2Helper)
	}
	ns := s1.ns

	t.Run("a_kube_dns_resolves", func(t *testing.T) {
		out := strings.TrimSpace(s1.sh(t, s1.work.Name, workloadContainer,
			`getent hosts kubernetes.default.svc.cluster.local >/dev/null && echo resolved || echo failed`))
		if out != "resolved" {
			t.Fatalf("workload pod could not resolve a cluster name (%s) — the DNS leg of the allowlist is closed", out)
		}
	})

	t.Run("b_c_own_helper_ports_connect", func(t *testing.T) {
		for _, port := range []string{sessionMCPPortText, helperProxyPort} {
			if got := s1.dial(t, s1.work.Name, workloadContainer, s1Helper, port); got != "open" {
				t.Errorf("workload → own helper %s:%s = %s, want open", s1Helper, port, got)
			}
		}
	})

	t.Run("d_other_destinations_refused", func(t *testing.T) {
		s1.mustBeBlockedFromWorkload(t, sessionMCPContainer, "oidc-mock.oidc-mock.svc.cluster.local", "5556")
		s1.mustBeBlockedFromWorkload(t, sessionMCPContainer, "fetch-origin."+ns+".svc.cluster.local", "8080")
	})

	t.Run("d_public_ip_refused", func(t *testing.T) {
		if got := s1.dial(t, s1.helper.Name, sessionMCPContainer, "1.1.1.1", "443"); got != "open" {
			t.Skipf("this runner has no public egress (control from the helper = %s); nothing to refuse", got)
		}
		if got := s1.dial(t, s1.work.Name, workloadContainer, "1.1.1.1", "443"); got != "closed" {
			t.Fatalf("workload pod reached a public address (%s)", got)
		}
	})

	t.Run("e_provider_origin_refused", func(t *testing.T) {
		s1.mustBeBlockedFromWorkload(t, helperCredProxyContainer, "anthropic-fake."+ns+".svc.cluster.local", "8443")
	})

	t.Run("f_approval_gateway_refused", func(t *testing.T) {
		s1.mustBeBlockedFromWorkload(t, sessionMCPContainer, "pocket-aide.pocket-aide.svc.cluster.local", "8080")
	})

	t.Run("g_other_sessions_helper_refused", func(t *testing.T) {
		for _, port := range []string{sessionMCPPortText, helperProxyPort} {
			if got := s2.dial(t, s2.work.Name, workloadContainer, s2Helper, port); got != "open" {
				t.Fatalf("control: S2 workload → S2 helper %s:%s = %s, want open", s2Helper, port, got)
			}
			if got := s1.dial(t, s1.work.Name, workloadContainer, s2Helper, port); got != "closed" {
				t.Errorf("S1 workload reached S2's helper %s:%s (%s) — one session could borrow another's proxy or MCP",
					s2Helper, port, got)
			}
		}
	})

	t.Run("h_policies_live_and_die_with_the_session", func(t *testing.T) {
		if got := s1.sessionPolicies(t); len(got) != 2 {
			t.Fatalf("session %s carries %d network policies %v, want the egress/ingress pair", s1.session.ID, len(got), got)
		}
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+s1.session.ID, nil); resp.StatusCode >= 400 {
			t.Fatalf("delete %s: status=%d body=%s", s1.session.ID, resp.StatusCode, raw)
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			left := s1.sessionPolicies(t)
			if len(left) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("network policies %v outlived session %s", left, s1.session.ID)
			}
			time.Sleep(2 * time.Second)
		}
	})
}
