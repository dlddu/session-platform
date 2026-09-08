//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 8
//
// AC-F6 (docs/prd/approval-gated-workload.md), asserted on the deployed SUT.
// What this file buys, what it deliberately leaves out and why, and which
// neighbour owns each thing it does not re-buy are all in docs/test/e2e.md —
// this file's row of § "시나리오 ↔ e2e 파일 매핑" and its four rows of
// § "남은 미검증 분기 (공백은 아님)".
//
// Every secret value compared below is read from the cluster Secret rather than
// written here, so this file holds no copy of a credential.
package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	approvalGatewaySecretName = "approval-gateway-credentials"

	gatewayURLEnvVar    = "APPROVAL_GATEWAY_URL"
	gatewayAPIKeyEnvVar = "APPROVAL_GATEWAY_API_KEY"
	gatewayUserIDEnvVar = "APPROVAL_GATEWAY_USER_ID"

	gatewayURLKey    = "url"
	gatewayAPIKeyKey = "api-key"
	gatewayUserIDKey = "user-id"

	sessionMCPURLEnv   = "SESSION_MCP_URL"
	proxyPlacementEnv  = "DATA_PLANE_PROXY_PLACEMENT"
	proxyAgentAddrEnv  = "DATA_PLANE_AGENT_ADDR"
	helperPlacement    = "helper"
	helperProxyBind    = "0.0.0.0:8091"
	helperProxyPort    = "8091"
	sessionMCPPortText = "8092"

	pluginTokenEnvVar = "K3S_MCP_TOKEN"

	modelEnvVar             = "CLAUDE_CODE_MODEL"
	controlPlaneLabel       = "app=control-plane"
	controlPlaneStartupLine = "starting control plane"
)

// Creating one is the expensive part — create returns only once both pods report
// Ready — so each test makes one and hangs every assertion it can off that pair.
type f6Session struct {
	session f4Session
	helper  corev1.Pod
	work    *corev1.Pod
	cs      kubernetes.Interface
	cfg     *rest.Config
	ns      string
}

// Reports ok=false when the run has no cluster access, so a suite pointed at a
// non-cluster SUT skips the cluster half instead of failing on it.
func newF6Session(t *testing.T, body map[string]any) (f6Session, bool) {
	t.Helper()
	resp, raw := do(t, http.MethodPost, "/api/v1/sessions", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create approval-gated session: status=%d body=%s", resp.StatusCode, raw)
	}
	s := f6Decode(t, raw)
	t.Cleanup(func() {
		if r, body := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); r.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", s.ID, r.StatusCode, body)
		}
	})
	if s.Pod == "" {
		t.Fatal("approval-gated session has no workload pod (AC-A2)")
	}
	cs, cfg, ok := kubeClient(t)
	if !ok {
		return f6Session{session: s}, false
	}
	ns := sessionNamespace()
	return f6Session{
		session: s,
		helper:  f4TheHelperPod(t, cs, ns, s),
		work:    getPodEventually(t, cs, ns, s.Pod),
		cs:      cs,
		cfg:     cfg,
		ns:      ns,
	}, true
}

func f6Decode(t *testing.T, raw []byte) f4Session {
	t.Helper()
	var s f4Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode session: %v body=%s", err, raw)
	}
	if s.WorkloadType != "approval-gated" {
		t.Fatalf("workloadType=%q want approval-gated", s.WorkloadType)
	}
	return s
}

func (f f6Session) container(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	c, found := containerByName(pod, name)
	if !found {
		t.Fatalf("pod %s has no %q container: %v", pod.Name, name, containerNames(pod))
	}
	return c
}

func (f f6Session) mcp(t *testing.T) corev1.Container {
	t.Helper()
	return f.container(t, &f.helper, sessionMCPContainer)
}

func (f f6Session) proxy(t *testing.T) corev1.Container {
	t.Helper()
	return f.container(t, &f.helper, helperCredProxyContainer)
}

func (f f6Session) workload(t *testing.T) corev1.Container {
	t.Helper()
	return f.container(t, f.work, workloadContainer)
}

// Values are passed as positional arguments rather than interpolated, so no
// credential is ever spliced into a command line this test builds.
func (f f6Session) sh(t *testing.T, pod, container, script string, args ...string) string {
	t.Helper()
	return f4Sh(t, f.cs, f.cfg, f.ns, pod, container, script, args...)
}

// The test runner may read these — it holds the cluster's admin kubeconfig;
// neither session pod may, which is what the placement is for.
func (f f6Session) secret(t *testing.T, name string) map[string]string {
	t.Helper()
	s, err := f.cs.CoreV1().Secrets(f.ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read %s/%s: %v", f.ns, name, err)
	}
	values := make(map[string]string, len(s.Data))
	for k, v := range s.Data {
		values[k] = strings.TrimSpace(string(v))
	}
	return values
}

// Fails when the entry is absent: the callers below go on to look at its value,
// and a missing entry would otherwise read as an empty one.
func mustEnv(t *testing.T, c corev1.Container, name string) corev1.EnvVar {
	t.Helper()
	env, found := e6Env(c, name)
	if !found {
		t.Fatalf("container %q has no %s", c.Name, name)
	}
	return env
}

// The Secret's name is a parameter because AC-F6's whole claim is that two
// different Secrets land in two different containers: a helper that carried one
// Secret name as a constant — as AC-E6's e6RequireSecretRef does, correctly for
// a type with only one — cannot state this AC's half of it.
func f6RequireSecretRef(t *testing.T, c corev1.Container, secretName, envName, key string, optional bool) {
	t.Helper()
	env := mustEnv(t, c, envName)
	ref := e6SecretRef(env)
	if ref == nil {
		t.Fatalf("container %q %s is not Secret-backed (value=%q)", c.Name, envName, env.Value)
	}
	if ref.Name != secretName || ref.Key != key {
		t.Fatalf("container %q %s reads %s/%s, want %s/%s",
			c.Name, envName, ref.Name, ref.Key, secretName, key)
	}
	if got := ref.Optional != nil && *ref.Optional; got != optional {
		t.Fatalf("container %q %s optional=%v, want %v", c.Name, envName, got, optional)
	}
}

// Naming the entry is not enough: the same key can arrive under a different
// variable name, and the claim AC-F6 makes is about the *key*, not its spelling.
func f6NoSecretRefTo(t *testing.T, c corev1.Container, secretName string, keys ...string) {
	t.Helper()
	banned := make(map[string]bool, len(keys))
	for _, k := range keys {
		banned[k] = true
	}
	for _, env := range c.Env {
		ref := e6SecretRef(env)
		if ref == nil || ref.Name != secretName {
			continue
		}
		if banned[ref.Key] {
			t.Fatalf("container %q reads %s/%s through %s — that key belongs to another container (AC-F6)",
				c.Name, ref.Name, ref.Key, env.Name)
		}
	}
}

// `control` counts the process environments holding a value the container is
// supposed to have ($1) and `leak` those holding one it must not ($2): the
// control must hit, or a zero `leak` would mean nothing more than an unreadable
// /proc.
const f6ReachProbe = `printf 'control=%s\n' "$(grep -lF "$1" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"
printf 'leak=%s\n' "$(grep -lF "$2" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"`

// The window is the container's whole life, not the session's: a session-scoped
// window comes back empty, and a search of an empty text proves nothing. Why it
// is empty is registered in docs/test/e2e.md § "남은 미검증 분기 (공백은 아님)".
func f6ControlPlaneLog(t *testing.T, cs kubernetes.Interface, ns string) string {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: controlPlaneLabel})
	if err != nil {
		t.Fatalf("list control plane pods in %s: %v", ns, err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("no pod matches %s in %s; the log surface AC-F6 names cannot be read", controlPlaneLabel, ns)
	}
	var all strings.Builder
	for i := range pods.Items {
		stream, err := cs.CoreV1().Pods(ns).
			GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{}).
			Stream(context.Background())
		if err != nil {
			t.Fatalf("stream logs of %s/%s: %v", ns, pods.Items[i].Name, err)
		}
		body, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			t.Fatalf("read logs of %s/%s: %v", ns, pods.Items[i].Name, err)
		}
		all.Write(body)
	}
	return all.String()
}

func TestApprovalGatedCredentialSplit_OnTheDeployedSUT(t *testing.T) {
	f, ok := newF6Session(t, map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated",
	})
	if !ok {
		return // the wire contract is asserted by create; the rest needs the cluster
	}
	mcp, proxy, work := f.mcp(t), f.proxy(t), f.workload(t)

	t.Run("GatewayTripleOnlyInTheMCPContainer", func(t *testing.T) {
		f6RequireSecretRef(t, mcp, approvalGatewaySecretName, gatewayURLEnvVar, gatewayURLKey, false)
		f6RequireSecretRef(t, mcp, approvalGatewaySecretName, gatewayAPIKeyEnvVar, gatewayAPIKeyKey, false)
		f6RequireSecretRef(t, mcp, approvalGatewaySecretName, gatewayUserIDEnvVar, gatewayUserIDKey, false)

		for _, c := range []corev1.Container{proxy, work} {
			e6RequireAbsent(t, c, gatewayURLEnvVar, gatewayAPIKeyEnvVar, gatewayUserIDEnvVar)
			f6NoSecretRefTo(t, c, approvalGatewaySecretName, gatewayURLKey, gatewayAPIKeyKey, gatewayUserIDKey)
		}
	})

	t.Run("ProviderCredentialsOnlyInTheProxyContainer", func(t *testing.T) {
		f6RequireSecretRef(t, proxy, credentialsSecretName, "ANTHROPIC_BASE_URL", "base-url", false)
		f6RequireSecretRef(t, proxy, credentialsSecretName, "ANTHROPIC_AUTH_TOKEN", "auth-token", false)
		f6RequireSecretRef(t, proxy, credentialsSecretName, "ANTHROPIC_CA_CERT", "ca-cert", true)

		e6RequireAbsent(t, mcp, "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CA_CERT")
		f6NoSecretRefTo(t, mcp, credentialsSecretName, "base-url", "auth-token", "ca-cert")

		e6RequireLiteral(t, proxy, proxyPlacementEnv, helperPlacement)
		e6RequireLiteral(t, proxy, proxyAgentAddrEnv, helperProxyBind)
	})

	t.Run("WorkloadContainerHoldsAddressesAndAPlaceholderOnly", func(t *testing.T) {
		f6NoSecretRefTo(t, work, credentialsSecretName, "base-url", "auth-token", "ca-cert", "k3s-mcp-token")
		e6RequireAbsent(t, work, pluginTokenEnvVar, "ANTHROPIC_CA_CERT")
		e6RequireLiteral(t, work, "ANTHROPIC_AUTH_TOKEN", proxyPlaceholderToken)

		ip := f.helper.Status.PodIP
		if ip == "" {
			t.Fatalf("helper pod %s publishes no pod IP; the addresses cannot be cross-checked", f.helper.Name)
		}
		e6RequireLiteral(t, work, "ANTHROPIC_BASE_URL", "http://"+ip+":"+helperProxyPort)
		e6RequireLiteral(t, work, sessionMCPURLEnv, "http://"+ip+":"+sessionMCPPortText)

		for _, name := range []string{"ANTHROPIC_BASE_URL", sessionMCPURLEnv} {
			if v := mustEnv(t, work, name).Value; strings.Contains(v, "127.0.0.1") || strings.Contains(v, "localhost") {
				t.Fatalf("workload container %s = %q — the helper pod is not loopback (AC-F6)", name, v)
			}
		}
	})

	t.Run("NeitherSecretIsResolvedInTheWorkloadContainer", func(t *testing.T) {
		gateway := f.secret(t, approvalGatewaySecretName)
		provider := f.secret(t, credentialsSecretName)
		apiKey, token := gateway[gatewayAPIKeyKey], provider["auth-token"]
		if apiKey == "" || token == "" {
			t.Fatal("a platform Secret has no value for the key under test; the split is untestable against this deployment")
		}
		if apiKey == token {
			t.Fatal("the gateway key and the provider token are the same string; the probes below could not tell the containers apart")
		}

		// Each holder resolved its own secret: without this the "not found"
		// results below would prove nothing — an empty variable is absent from
		// every environment.
		if got := strings.TrimSpace(f.sh(t, f.helper.Name, sessionMCPContainer,
			`printf %s "$APPROVAL_GATEWAY_API_KEY"`)); got != apiKey {
			t.Fatalf("the MCP container did not resolve the gateway key from the Secret (got %d bytes, want %d)",
				len(got), len(apiKey))
		}
		if got := strings.TrimSpace(f.sh(t, f.helper.Name, helperCredProxyContainer,
			`printf %s "$ANTHROPIC_AUTH_TOKEN"`)); got != token {
			t.Fatalf("the proxy container did not resolve the provider token from the Secret (got %d bytes, want %d)",
				len(got), len(token))
		}

		for _, tc := range []struct{ what, value string }{
			{"the approval gateway key", apiKey},
			{"the provider token", token},
		} {
			probe := e6ProbeFields(t, f.sh(t, f.work.Name, workloadContainer, f6ReachProbe, proxyPlaceholderToken, tc.value))
			if probe["control"] == "0" {
				t.Fatalf("the probe found the placeholder in no process environment, so it cannot read /proc here and its leak=%q result for %s proves nothing",
					probe["leak"], tc.what)
			}
			if probe["leak"] != "0" {
				t.Fatalf("%s is readable from %s process environments in the workload container — keeping both out of this pod is what the helper pod is for (AC-F6)",
					tc.what, probe["leak"])
			}
		}
	})

	t.Run("PlatformDefaultModelResolvesFromTheOptionalKey", func(t *testing.T) {
		f6RequireSecretRef(t, work, credentialsSecretName, modelEnvVar, "model", true)
		e6RequireAbsent(t, mcp, modelEnvVar)
		e6RequireAbsent(t, proxy, modelEnvVar)

		want := f.secret(t, credentialsSecretName)["model"]
		if want == "" {
			t.Skip("this deployment leaves the optional model key empty; the omission branch is unit-owned")
		}
		if got := strings.TrimSpace(f.sh(t, f.work.Name, workloadContainer,
			`printf %s "$CLAUDE_CODE_MODEL"`)); got != want {
			t.Fatalf("workload container resolved CLAUDE_CODE_MODEL=%q, want the Secret's %q", got, want)
		}
	})

	t.Run("NoTokenReachesTheSessionAPIOrTheControlPlaneLog", func(t *testing.T) {
		gateway := f.secret(t, approvalGatewaySecretName)
		provider := f.secret(t, credentialsSecretName)

		lookupResp, lookup := do(t, http.MethodGet, "/api/v1/sessions/"+f.session.ID, nil)
		if lookupResp.StatusCode != http.StatusOK {
			t.Fatalf("session lookup: status=%d body=%s", lookupResp.StatusCode, lookup)
		}
		listResp, list := do(t, http.MethodGet, "/api/v1/sessions", nil)
		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("list sessions: status=%d body=%s", listResp.StatusCode, list)
		}
		// The read *response*, not its payload — the scrollback half is
		// registered in docs/test/e2e.md § "남은 미검증 분기 (공백은 아님)".
		readResp, read := do(t, http.MethodPost, "/api/v1/sessions/"+f.session.ID+"/read",
			map[string]int64{"offset": 0})
		if readResp.StatusCode != http.StatusOK {
			t.Fatalf("read session: status=%d body=%s", readResp.StatusCode, read)
		}

		// Every surface carries a control — something that must be in it. A
		// negative over a text the SUT never wrote would hold just as well.
		surfaces := []struct{ name, text, control, why string }{
			{"session lookup response", string(lookup), f.session.ID,
				"a lookup response names the session it answers for"},
			{"session list response", string(list), f.session.ID,
				"the list must carry the session that is active right now"},
			{"read response", string(read), f.session.ID,
				"a read envelope names its session"},
			{"control plane log", f6ControlPlaneLog(t, f.cs, f.ns), controlPlaneStartupLine,
				"a control plane log reaches back to the process start"},
		}
		for _, s := range surfaces {
			if !strings.Contains(s.text, s.control) {
				t.Fatalf("%s does not contain %q — %s, so a token missing from it would prove nothing",
					s.name, s.control, s.why)
			}
		}

		// A second control for the log alone: a Secret-resolved value it echoes
		// on the startup line proves the log carries Secret-derived material at
		// all, so the absences below are a property of what the control plane
		// logs rather than of a log that could never hold one.
		if model := provider["model"]; model != "" {
			if log := surfaces[len(surfaces)-1].text; !strings.Contains(log, model) {
				t.Fatalf("the control plane log echoes no value resolved from %s; the absences below would say less than AC-F6 asks",
					credentialsSecretName)
			}
		}

		secrets := []struct{ what, value string }{
			{"the gateway api key", gateway[gatewayAPIKeyKey]},
			{"the gateway url", gateway[gatewayURLKey]},
			{"the notification userId", gateway[gatewayUserIDKey]},
			{"the provider token", provider["auth-token"]},
		}
		for _, sec := range secrets {
			if sec.value == "" {
				t.Fatalf("the deployment leaves %s empty; its absence from every surface would be vacuous", sec.what)
			}
		}
		for _, s := range surfaces {
			for _, sec := range secrets {
				if strings.Contains(s.text, sec.value) {
					t.Fatalf("%s contains %s (AC-F6)", s.name, sec.what)
				}
			}
		}
	})
}

func TestApprovalGatedCredentialSplit_ConcreteModelBeatsThePlatformDefault(t *testing.T) {
	const concrete = "claude-e2e-alternate"
	f, ok := newF6Session(t, map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated", "model": concrete,
	})
	if !ok {
		return
	}
	work := f.workload(t)
	e6RequireLiteral(t, work, modelEnvVar, concrete)
	e6RequireAbsent(t, f.mcp(t), modelEnvVar)
	e6RequireAbsent(t, f.proxy(t), modelEnvVar)

	if got := strings.TrimSpace(f.sh(t, f.work.Name, workloadContainer,
		`printf %s "$CLAUDE_CODE_MODEL"`)); got != concrete {
		t.Fatalf("workload container resolved CLAUDE_CODE_MODEL=%q, want the literal %q — the Secret default must not win",
			got, concrete)
	}
	if f.session.State != "active" {
		t.Fatalf("state=%q want active", f.session.State)
	}
}

func TestApprovalGatedCredentialSplit_PlacementCannotBeChosenByTheCreateRequest(t *testing.T) {
	for _, field := range []string{
		"userId", "approvalGatewayUrl", "approvalGatewayApiKey", "authToken", "baseUrl",
	} {
		t.Run(field, func(t *testing.T) {
			name := uniqueName(t)
			resp, body := do(t, http.MethodPost, "/api/v1/sessions", map[string]any{
				"name": name, "workloadType": "approval-gated", field: "caller-supplied-value",
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("create with %q: status=%d body=%s, want 400", field, resp.StatusCode, body)
			}
			listResp, list := do(t, http.MethodGet, "/api/v1/sessions", nil)
			if listResp.StatusCode != http.StatusOK {
				t.Fatalf("list sessions: status=%d body=%s", listResp.StatusCode, list)
			}
			if strings.Contains(string(list), name) {
				t.Fatalf("the rejected create with %q still produced a session named %q", field, name)
			}
		})
	}
}
