//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 8
//
// Where an approval-gated session's two external secrets land, asserted on the
// *deployed* SUT (docs/prd/approval-gated-workload.md AC-F6). AC-F6 is a
// placement claim with a purpose: the container that runs model and user code
// holds neither the approval gateway's key nor the provider's token, so the
// agent can neither call the provider directly nor approve its own requests.
//
// Everything the deployed SUT can currently answer is bought here:
//
//  1. the gateway triple (url, api key, notification userId) is projected into
//     the helper pod's session-mcp container and into no other container,
//  2. the provider credentials (base url, auth token, optional ca-cert) are
//     projected into the same pod's credential-proxy container and into no
//     other container — and that container declares the *helper* placement,
//     binding the pod network rather than loopback,
//  3. the workload container carries no Secret-backed credential at all: only
//     the helper pod's addresses — cross-checked against the IP the API server
//     actually assigned that pod — and a placeholder that is not a secret,
//  4. the environments the containers really resolved, which is the claim a pod
//     spec alone cannot make: each holder resolved its own secret, and neither
//     value is readable from any process environment in the workload container,
//  5. a create request cannot choose any of it — gateway fields, the userId and
//     provider credentials are unknown fields and no session is created,
//  6. no token string reaches the session lookup response, the session list, the
//     read response or the control plane's log — each of those four searched
//     only after a control proves the text is one the SUT really produced,
//  7. the model contract is claude-code's: platform-default resolves from the
//     optional Secret key, a concrete model is a literal, and neither helper
//     container is given the variable.
//
// What is deliberately missing, and why — all five are registered in
// docs/test/e2e.md § "남은 미검증 분기 (공백은 아님)":
//
//   - the workload pod being *unable* to reach the gateway address or the
//     provider origin directly, and another session's workload pod being unable
//     to reach this helper pod, are AC-F2 boundary claims that need a CNI which
//     enforces NetworkPolicy. kindnet does not, so a refused connection here
//     would prove nothing and an accepted one would not be a product defect.
//   - "no credential rides in the approval context" needs an approval round
//     trip, which is AC-F3 and blocked on its own precondition.
//   - the *scrollback* half of the read surface. This type runs the same
//     one-shot model as claude-code, so it has no resident shell and produces
//     no output until a prompt round trip is driven through the provider
//     stand-in and the session MCP — machinery AC-E2/E3 own and no e2e drives
//     for this type. Point 6 buys the read *response*, whose envelope names the
//     session; searching an empty payload would prove nothing.
//   - the *per-request* half of the control plane's log. Its only request
//     logger writes at Debug under a handler that defaults to Info, so a
//     window scoped to one session's lifetime comes back empty (CI measured
//     exactly that). Point 6 buys the log the control plane really emits, from
//     process start, with the startup line and a Secret-resolved value in it as
//     controls. Raising the level is a product decision, not a test one.
//
// Neighbours this file does not re-buy: AC-F1 owns the type axis and the shape
// of the pod set; AC-F4 owns the helper pod's ownership, lifetime and the PID
// namespace boundary between its two containers; AC-E6's file owns the same
// placement question for `claude-code`, whose sidecar arrangement is the one
// this type moves away from. The pod spec the orchestrator *submits* is owned
// in-process by control-plane/test/approval_gated_orchestrator_test.go (build
// tag `integration`), and the proxy's behaviour contract — header allowlist,
// 1xx redaction, the 64 MiB cap, tail-safe split-token redaction — is unit-owned
// by data-plane/cmd/agent/credential_proxy*_test.go and does not change with
// placement. What only the deployed cluster adds is the environment that was
// actually resolved from the Secret and the address that was actually assigned.
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
	// approvalGatewaySecretName is the platform-global Secret holding the
	// approval gateway triple; the deployment provisions it (deploy/).
	approvalGatewaySecretName = "approval-gateway-credentials"

	// The gateway triple, named as the helper pod's MCP container gets it.
	gatewayURLEnvVar    = "APPROVAL_GATEWAY_URL"
	gatewayAPIKeyEnvVar = "APPROVAL_GATEWAY_API_KEY"
	gatewayUserIDEnvVar = "APPROVAL_GATEWAY_USER_ID"

	// ...and the Secret keys behind them.
	gatewayURLKey    = "url"
	gatewayAPIKeyKey = "api-key"
	gatewayUserIDKey = "user-id"

	// sessionMCPURLEnv tells the workload pod's agent where its session MCP is;
	// proxyPlacementEnv tells the proxy which placement it was deployed in and
	// agentAddrEnv is the address it binds. The placement pair is the only thing
	// AC-F6 changes about the proxy, so it is asserted rather than assumed.
	sessionMCPURLEnv   = "SESSION_MCP_URL"
	proxyPlacementEnv  = "DATA_PLANE_PROXY_PLACEMENT"
	proxyAgentAddrEnv  = "DATA_PLANE_AGENT_ADDR"
	helperPlacement    = "helper"
	helperProxyBind    = "0.0.0.0:8091"
	helperProxyPort    = "8091"
	sessionMCPPortText = "8092"

	// The plugin bootstrap token this type deliberately does not carry — its
	// absence is the decision recorded in AC-F6's ✅ 2026-09-03 note, not an
	// oversight, so a future reintroduction should fail here.
	pluginTokenEnvVar = "K3S_MCP_TOKEN"

	// modelEnvVar and the label the control plane runs under, used to find the
	// pod whose log must not echo a token.
	modelEnvVar       = "CLAUDE_CODE_MODEL"
	controlPlaneLabel = "app=control-plane"
	// controlPlaneStartupLine is the message the control plane logs once, at
	// process start, with the configuration it was given. Finding it is how the
	// log search below knows it is reading a whole log rather than an empty
	// window — see f6ControlPlaneLog.
	controlPlaneStartupLine = "starting control plane"
)

// f6Session is one approval-gated session plus the cluster handles its
// assertions need. Creating it is the expensive part — create returns only once
// both pods report Ready — so each test makes one and hangs every assertion it
// can off that single pair.
type f6Session struct {
	session f4Session
	helper  corev1.Pod
	work    *corev1.Pod
	cs      kubernetes.Interface
	cfg     *rest.Config
	ns      string
}

// newF6Session creates an approval-gated session and fetches both of its pods.
// It reports ok=false when the run has no cluster access, so a suite pointed at
// a non-cluster SUT skips the cluster half instead of failing on it.
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

// container returns a named container of either pod, failing with the pod's
// actual container list so a rename reads as a rename.
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

// sh runs a /bin/sh script in one container of one pod. Values are passed as
// positional arguments rather than interpolated, so no credential is ever
// spliced into a command line this test builds.
func (f f6Session) sh(t *testing.T, pod, container, script string, args ...string) string {
	t.Helper()
	return f4Sh(t, f.cs, f.cfg, f.ns, pod, container, script, args...)
}

// secret reads one of the platform Secrets from the cluster. The test runner
// may do this — it holds the cluster's admin kubeconfig; neither session pod
// may, which is what the placement is for.
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

// mustEnv returns a container's environment entry, failing when it is absent —
// the callers below go on to look at its value, and a missing entry would
// otherwise read as an empty one.
func mustEnv(t *testing.T, c corev1.Container, name string) corev1.EnvVar {
	t.Helper()
	env, found := e6Env(c, name)
	if !found {
		t.Fatalf("container %q has no %s", c.Name, name)
	}
	return env
}

// f6RequireSecretRef asserts an entry is backed by the given key of a *named*
// Secret, with the expected optionality. Optional or not is the difference
// between "the deployment must provide this" and "absent means the documented
// fallback", which AC-F6 states key by key.
//
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

// f6NoSecretRefTo asserts a container reads none of the given keys of a Secret
// through any of its environment entries. Naming the entry is not enough: the
// same key can arrive under a different variable name, and the claim AC-F6
// makes is about the *key*, not its spelling.
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

// f6ReachProbe reports what a container can see of a secret value. `control`
// counts the process environments holding a value the container is supposed to
// have and `leak` those holding one it must not: the control must hit, or a
// zero `leak` would mean nothing more than an unreadable /proc.
//
// $1 is the control value and $2 the one that must not be there.
const f6ReachProbe = `printf 'control=%s\n' "$(grep -lF "$1" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"
printf 'leak=%s\n' "$(grep -lF "$2" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"`

// f6ControlPlaneLog returns every control plane replica's log from process
// start. The control plane is the one process that handles both Secrets' names
// on every create, so its log is the surface most able to echo one by accident.
//
// The window is the container's whole life, not the session's. A session-scoped
// window is empty here — the only per-request logger writes at Debug and the
// handler defaults to Info — and a search of an empty text proves nothing. The
// whole log does contain the one line the control plane always writes, the
// startup line that echoes the configuration it was given, which is what the
// caller uses as its control.
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

// The whole placement, read off one session's pod pair.
func TestApprovalGatedCredentialSplit_OnTheDeployedSUT(t *testing.T) {
	f, ok := newF6Session(t, map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated",
	})
	if !ok {
		return // the wire contract is asserted by create; the rest needs the cluster
	}
	mcp, proxy, work := f.mcp(t), f.proxy(t), f.workload(t)

	t.Run("GatewayTripleOnlyInTheMCPContainer", func(t *testing.T) {
		// Required, all three, and out of the gateway's own Secret: the MCP
		// container cannot ask for an approval without the address, cannot
		// authenticate without the key, and has nobody to notify without the
		// platform-wide userId.
		f6RequireSecretRef(t, mcp, approvalGatewaySecretName, gatewayURLEnvVar, gatewayURLKey, false)
		f6RequireSecretRef(t, mcp, approvalGatewaySecretName, gatewayAPIKeyEnvVar, gatewayAPIKeyKey, false)
		f6RequireSecretRef(t, mcp, approvalGatewaySecretName, gatewayUserIDEnvVar, gatewayUserIDKey, false)

		// Its neighbour in the same pod, and the container that runs the agent,
		// are given none of it — by name and by key.
		for _, c := range []corev1.Container{proxy, work} {
			e6RequireAbsent(t, c, gatewayURLEnvVar, gatewayAPIKeyEnvVar, gatewayUserIDEnvVar)
			f6NoSecretRefTo(t, c, approvalGatewaySecretName, gatewayURLKey, gatewayAPIKeyKey, gatewayUserIDKey)
		}
	})

	t.Run("ProviderCredentialsOnlyInTheProxyContainer", func(t *testing.T) {
		f6RequireSecretRef(t, proxy, credentialsSecretName, "ANTHROPIC_BASE_URL", "base-url", false)
		f6RequireSecretRef(t, proxy, credentialsSecretName, "ANTHROPIC_AUTH_TOKEN", "auth-token", false)
		// The trust anchor is optional and rides with the credential it is
		// paired with — same rule as AC-E6, same container as the token.
		f6RequireSecretRef(t, proxy, credentialsSecretName, "ANTHROPIC_CA_CERT", "ca-cert", true)

		// The MCP container fronts the approval gate; it never speaks to the
		// provider, so it gets no provider credential.
		e6RequireAbsent(t, mcp, "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CA_CERT")
		f6NoSecretRefTo(t, mcp, credentialsSecretName, "base-url", "auth-token", "ca-cert")

		// The placement itself: declared, and bound to the pod network rather
		// than loopback. This is the only thing AC-F6 changes about the proxy —
		// the behaviour contract is AC-E6's and is unit-owned.
		e6RequireLiteral(t, proxy, proxyPlacementEnv, helperPlacement)
		e6RequireLiteral(t, proxy, proxyAgentAddrEnv, helperProxyBind)
	})

	t.Run("WorkloadContainerHoldsAddressesAndAPlaceholderOnly", func(t *testing.T) {
		f6NoSecretRefTo(t, work, credentialsSecretName, "base-url", "auth-token", "ca-cert", "k3s-mcp-token")
		e6RequireAbsent(t, work, pluginTokenEnvVar, "ANTHROPIC_CA_CERT")
		e6RequireLiteral(t, work, "ANTHROPIC_AUTH_TOKEN", proxyPlaceholderToken)

		// The addresses are the helper pod's, and the pod IP is read from the
		// cluster rather than from the value under test: what this buys over
		// the in-process suite is that the control plane injected the address
		// the API server actually assigned.
		ip := f.helper.Status.PodIP
		if ip == "" {
			t.Fatalf("helper pod %s publishes no pod IP; the addresses cannot be cross-checked", f.helper.Name)
		}
		e6RequireLiteral(t, work, "ANTHROPIC_BASE_URL", "http://"+ip+":"+helperProxyPort)
		e6RequireLiteral(t, work, sessionMCPURLEnv, "http://"+ip+":"+sessionMCPPortText)

		// Not loopback: a loopback address here would mean the workload pod was
		// still expecting a sidecar, which is the arrangement AC-F6 moved away
		// from and the reason the proxy binds the pod network at all.
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

		// Each holder resolved its own secret. Without this the "not found"
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

		// The container that runs model and user code holds neither. Its own
		// placeholder is the control: the probe must find that, or a zero leak
		// count would only mean /proc was unreadable here.
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
		// The model contract is claude-code's, unchanged by the move.
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
		// The read *response*, not its payload: this type has no resident shell,
		// so the scrollback is empty until a prompt round trip that AC-E2/E3 own
		// (registered in docs/test/e2e.md). The envelope is what the SUT does
		// produce here, and it names the session it answers for.
		readResp, read := do(t, http.MethodPost, "/api/v1/sessions/"+f.session.ID+"/read",
			map[string]int64{"offset": 0})
		if readResp.StatusCode != http.StatusOK {
			t.Fatalf("read session: status=%d body=%s", readResp.StatusCode, read)
		}

		// Every surface carries a control — something that must be in it. A
		// negative over a text the SUT never wrote would hold just as well, and
		// that is exactly how this assertion failed the first time it ran.
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

		// A second control for the log alone. The control plane is given the two
		// Secrets' *names*, and out of one of them a single non-credential value
		// it resolves itself (the optional model key) — which it echoes on the
		// startup line. Finding that proves the log carries Secret-derived
		// material at all, so the four absences below are a property of what the
		// control plane logs rather than of a log that could never hold one.
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

// A concrete model is a literal on the workload container and outranks the
// Secret default — the half of the model contract a platform-default session
// cannot show. Same claim AC-E6 makes for `claude-code`, asserted here because
// AC-F6 states the contract is carried over unchanged.
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

// None of the placement is caller-choosable. The create DTO decodes strictly,
// so each of these is an unknown field: no session is created and the platform
// keeps deciding who gets notified and which gateway is asked.
//
// AC-E6's file asserts the same refusal for `claude-code`'s credential fields;
// what is bought here is the set AC-F6 names for *this* type — above all
// `userId`, which AC-F6 requires to be a platform-wide value a session cannot
// pick.
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
