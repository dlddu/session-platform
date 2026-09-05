//go:build e2e

// 검증 AC: AC-E6
//
// Where the platform's secrets land, asserted on the deployed SUT
// (docs/prd/claude-code-workload.md AC-E6). The AC's title is a placement
// claim — provider credentials in the sidecar, the plugin token in the main
// container, the platform model as an optional key — and placement is only
// worth stating because of what it buys: the container that runs model and
// user code cannot reach the provider token at all.
//
// Two neighbours already own parts of AC-E6 and this file deliberately does not
// re-buy them:
//
//   - the pod spec the orchestrator *submits* is asserted in-process against a
//     fake clientset by control-plane/test/workload_type_orchestrator_test.go
//     (build tag `integration`),
//   - the proxy's behaviour contract — header allowlist, 1xx redaction, the
//     64 MiB raw cap, tail-safe split-token redaction, refusing to start on an
//     unparseable ca-cert — is unit-owned by data-plane/cmd/agent/
//     credential_proxy*_test.go.
//
// What only e2e can buy is the deployed ground truth: the pod the real API
// server admitted, the environment those containers actually resolved from the
// Secret, the real authorizer's answer for the data-plane identity, and the
// public API's silence about both tokens. Every secret value compared below is
// read from the cluster Secret rather than written here, so this file holds no
// copy of a credential and cannot drift from the deployment.
package e2e_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// credentialsContainer is the credential-proxy sidecar of a claude-code
	// session pod — the only container that may hold provider credentials.
	credentialsContainer = "claude-credentials"
	// credentialsSecretName is the platform-global Secret the orchestrator
	// references; the deployment provisions it (deploy/, k8s/ example).
	credentialsSecretName = "claude-code-credentials"
	// The loopback endpoint and non-secret placeholder the main container gets
	// in place of the provider's address and token.
	proxyURLForMainContainer = "http://127.0.0.1:8091"
	proxyPlaceholderToken    = "session-platform-proxy"
	// dataPlaneServiceAccount is the session pod's identity: read-only cluster
	// discovery through the built-in `view` role, which excludes Secrets.
	dataPlaneServiceAccount = "data-plane"
)

// e6Session is one claude-code session under test plus the cluster handles its
// assertions need. Creating it is the expensive part (the pod must reach Ready,
// which means entrypoint.sh finished its plugin bootstrap), so each test makes
// one and hangs every assertion it can off that single pod.
type e6Session struct {
	session typedSession
	pod     *corev1.Pod
	cs      kubernetes.Interface
	cfg     *rest.Config
	ns      string
}

// newE6Session creates a claude-code session and fetches its pod. It reports
// ok=false when the run has no cluster access, so a suite pointed at a
// non-cluster SUT skips the cluster half instead of failing on it.
func newE6Session(t *testing.T, body map[string]any) (e6Session, bool) {
	t.Helper()
	status, s := createTyped(t, body)
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", s.ID, resp.StatusCode, raw)
		}
	})
	if s.Pod == "" {
		t.Fatal("claude-code session has no dedicated pod")
	}
	cs, cfg, ok := kubeClient(t)
	if !ok {
		return e6Session{session: s}, false
	}
	ns := sessionNamespace()
	return e6Session{session: s, pod: getPodEventually(t, cs, ns, s.Pod), cs: cs, cfg: cfg, ns: ns}, true
}

// container returns a named container of the session pod, failing with the
// pod's actual container list so a rename reads as a rename.
func (e e6Session) container(t *testing.T, name string) corev1.Container {
	t.Helper()
	c, found := containerByName(e.pod, name)
	if !found {
		t.Fatalf("pod %s has no %q container: %v", e.pod.Name, name, containerNames(e.pod))
	}
	return c
}

// sh runs a /bin/sh script in one of the pod's containers. Secret values are
// passed as positional arguments rather than interpolated into the script, so
// no credential is ever spliced into a command line this test builds.
func (e e6Session) sh(t *testing.T, container, script string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := append([]string{"/bin/sh", "-c", script, "sh"}, args...)
	stdout, stderr, err := execInContainer(ctx, e.cs, e.cfg, e.ns, e.pod.Name, container, command)
	if err != nil {
		t.Fatalf("exec in %s/%s [%s]: %v (stdout=%q stderr=%q)", e.ns, e.pod.Name, container, err, stdout, stderr)
	}
	return stdout
}

// credentialsSecret reads the platform Secret from the cluster. The test runner
// may do this — it holds the cluster's admin kubeconfig; the data plane may
// not, which is exactly what the authorization subtest asserts.
func (e e6Session) credentialsSecret(t *testing.T) map[string]string {
	t.Helper()
	secret, err := e.cs.CoreV1().Secrets(e.ns).Get(context.Background(), credentialsSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read %s/%s: %v", e.ns, credentialsSecretName, err)
	}
	values := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		values[k] = strings.TrimSpace(string(v))
	}
	return values
}

// e6Env finds an environment entry by name.
func e6Env(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, env := range c.Env {
		if env.Name == name {
			return env, true
		}
	}
	return corev1.EnvVar{}, false
}

// e6SecretRef is the Secret key an environment entry reads, or nil for a
// literal.
func e6SecretRef(env corev1.EnvVar) *corev1.SecretKeySelector {
	if env.ValueFrom == nil {
		return nil
	}
	return env.ValueFrom.SecretKeyRef
}

// e6RequireSecretRef asserts that an entry is backed by the given key of the
// credentials Secret, with the expected optionality. Optional or not is the
// difference between "the deployment must provide this" and "absent means the
// documented fallback", which AC-E6 states key by key.
func e6RequireSecretRef(t *testing.T, c corev1.Container, envName, key string, optional bool) {
	t.Helper()
	env, found := e6Env(c, envName)
	if !found {
		t.Fatalf("container %q has no %s", c.Name, envName)
	}
	ref := e6SecretRef(env)
	if ref == nil {
		t.Fatalf("container %q %s is not Secret-backed (value=%q)", c.Name, envName, env.Value)
	}
	if ref.Name != credentialsSecretName || ref.Key != key {
		t.Fatalf("container %q %s reads %s/%s, want %s/%s",
			c.Name, envName, ref.Name, ref.Key, credentialsSecretName, key)
	}
	if got := ref.Optional != nil && *ref.Optional; got != optional {
		t.Fatalf("container %q %s optional=%v, want %v", c.Name, envName, got, optional)
	}
}

// e6RequireLiteral asserts an entry carries a plain value and no Secret ref at
// all — the point of the placement, not an accident of how it is spelled.
func e6RequireLiteral(t *testing.T, c corev1.Container, envName, want string) {
	t.Helper()
	env, found := e6Env(c, envName)
	if !found {
		t.Fatalf("container %q has no %s", c.Name, envName)
	}
	if e6SecretRef(env) != nil {
		t.Fatalf("container %q %s is Secret-backed; it must be the literal %q", c.Name, envName, want)
	}
	if env.Value != want {
		t.Fatalf("container %q %s = %q, want %q", c.Name, envName, env.Value, want)
	}
}

// e6RequireAbsent asserts a container was given no such environment entry.
func e6RequireAbsent(t *testing.T, c corev1.Container, envNames ...string) {
	t.Helper()
	for _, name := range envNames {
		if env, found := e6Env(c, name); found {
			t.Fatalf("container %q must not carry %s (value=%q, valueFrom=%v)",
				c.Name, name, env.Value, env.ValueFrom)
		}
	}
}

// e6Allowed asks the cluster's own authorizer whether user may perform verb on
// resource. Reading the RBAC objects would only re-state the manifests; a
// SubjectAccessReview is the authorizer's answer.
func e6Allowed(t *testing.T, cs kubernetes.Interface, user, ns, verb, resource string) bool {
	t.Helper()
	review, err := cs.AuthorizationV1().SubjectAccessReviews().Create(context.Background(),
		&authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User: user,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: ns, Verb: verb, Resource: resource, Version: "v1",
				},
			},
		}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("SubjectAccessReview(%s can %s %s): %v", user, verb, resource, err)
	}
	return review.Status.Allowed
}

// e6ProbeFields parses the `key=value` lines an in-pod probe prints.
func e6ProbeFields(t *testing.T, out string) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, found := strings.Cut(strings.TrimRight(line, "\r"), "="); found {
			fields[key] = value
		}
	}
	return fields
}

// e6TokenReachProbe reports what the tool-running container can see of the
// provider token. `control` counts the process environments holding the
// non-secret placeholder and `leak` those holding the real token: the control
// must hit, or a zero `leak` would mean nothing more than an unreadable /proc.
// $1 is the real token and $2 the placeholder — passed as arguments so neither
// is spliced into a command line.
const e6TokenReachProbe = `printf 'own=%s\n' "$ANTHROPIC_AUTH_TOKEN"
printf 'control=%s\n' "$(grep -lF "$2" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"
printf 'leak=%s\n' "$(grep -lF "$1" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"`

func TestCredentialPlacementOnTheDeployedSUT(t *testing.T) {
	e, ok := newE6Session(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code",
	})
	if !ok {
		return // the wire contract is asserted by create; the rest needs the cluster
	}
	main := e.container(t, workloadContainer)
	proxy := e.container(t, credentialsContainer)

	t.Run("ProviderCredentialsOnlyInTheProxySidecar", func(t *testing.T) {
		e6RequireSecretRef(t, proxy, "ANTHROPIC_BASE_URL", "base-url", false)
		e6RequireSecretRef(t, proxy, "ANTHROPIC_AUTH_TOKEN", "auth-token", false)
		// The trust anchor is optional and rides with the credential it is
		// paired with: a deployment whose provider is a public endpoint omits
		// the key and keeps the system pool.
		e6RequireSecretRef(t, proxy, "ANTHROPIC_CA_CERT", "ca-cert", true)

		// The main container is told where the proxy is and given a placeholder
		// that is not a secret. Nothing it holds reaches the Secret.
		e6RequireLiteral(t, main, "ANTHROPIC_BASE_URL", proxyURLForMainContainer)
		e6RequireLiteral(t, main, "ANTHROPIC_AUTH_TOKEN", proxyPlaceholderToken)
		e6RequireAbsent(t, main, "ANTHROPIC_CA_CERT")
		for _, env := range main.Env {
			ref := e6SecretRef(env)
			if ref == nil {
				continue
			}
			switch ref.Key {
			case "base-url", "auth-token", "ca-cert":
				t.Fatalf("main container reads provider key %q through %s — those belong to the sidecar only",
					ref.Key, env.Name)
			}
		}
	})

	t.Run("PluginTokenAndBootstrapAddressesOnlyInTheMainContainer", func(t *testing.T) {
		// Required: the plugin bootstrap cannot run without it.
		e6RequireSecretRef(t, main, "K3S_MCP_TOKEN", "k3s-mcp-token", false)
		// Optional address-class keys: absent, entrypoint.sh keeps its
		// production defaults, which is why k8s/ needs no change for them.
		e6RequireSecretRef(t, main, "K3S_MCP_URL", "k3s-mcp-url", true)
		e6RequireSecretRef(t, main, "CLAUDE_CODE_PLUGIN_MARKETPLACE_URL", "plugin-marketplace-url", true)

		// The proxy skips the bootstrap entirely, so it is given none of them.
		e6RequireAbsent(t, proxy, "K3S_MCP_TOKEN", "K3S_MCP_URL", "CLAUDE_CODE_PLUGIN_MARKETPLACE_URL")
	})

	t.Run("PlatformModelIsAnOptionalSecretOnTheMainContainerOnly", func(t *testing.T) {
		// A session created without `model` carries the platform-default alias,
		// which resolves at pod start from the optional key.
		e6RequireSecretRef(t, main, "CLAUDE_CODE_MODEL", "model", true)
		e6RequireAbsent(t, proxy, "CLAUDE_CODE_MODEL")

		want := e.credentialsSecret(t)["model"]
		if want == "" {
			t.Skip("this deployment leaves the optional model key empty; the --model omission branch is unit-owned")
		}
		if got := strings.TrimSpace(e.sh(t, workloadContainer, `printf %s "$CLAUDE_CODE_MODEL"`)); got != want {
			t.Fatalf("main container resolved CLAUDE_CODE_MODEL=%q, want the Secret's %q", got, want)
		}
	})

	t.Run("ToolContainerCannotReachTheProviderToken", func(t *testing.T) {
		token := e.credentialsSecret(t)["auth-token"]
		if token == "" {
			t.Fatal("the platform Secret has no auth-token; the isolation claim is untestable against this deployment")
		}

		// The sidecar resolved the real token: the probe must find it in the
		// container that is supposed to hold it, or a later "not found" in the
		// main container would prove nothing.
		if got := strings.TrimSpace(e.sh(t, credentialsContainer, `printf %s "$ANTHROPIC_AUTH_TOKEN"`)); got != token {
			t.Fatalf("the proxy sidecar did not resolve the platform token from the Secret (got %d bytes, want %d)",
				len(got), len(token))
		}
		// ...and it was given no plugin token, which lives one container over.
		if got := strings.TrimSpace(e.sh(t, credentialsContainer, `printf %s "${K3S_MCP_TOKEN-UNSET}"`)); got != "UNSET" {
			t.Fatal("the proxy sidecar carries K3S_MCP_TOKEN; the plugin token belongs to the main container only")
		}

		probe := e6ProbeFields(t, e.sh(t, workloadContainer, e6TokenReachProbe, token, proxyPlaceholderToken))
		if probe["own"] != proxyPlaceholderToken {
			t.Fatalf("main container ANTHROPIC_AUTH_TOKEN = %q, want the non-secret placeholder %q",
				probe["own"], proxyPlaceholderToken)
		}
		if probe["control"] == "0" {
			t.Fatalf("the probe found the placeholder in no process environment, so it cannot read /proc here and its leak=%q result proves nothing",
				probe["leak"])
		}
		if probe["leak"] != "0" {
			t.Fatalf("the platform token is readable from %s process environments in the tool-running container; keeping it out of reach is what the sidecar is for",
				probe["leak"])
		}
		// That isolation holds because the containers share only the network
		// namespace. A pod that opted into a shared PID namespace would put the
		// proxy's /proc entries back within reach.
		if e.pod.Spec.ShareProcessNamespace != nil && *e.pod.Spec.ShareProcessNamespace {
			t.Fatal("the session pod shares its PID namespace; the proxy's process environment would be readable from the tool container")
		}
	})

	t.Run("DataPlaneIdentityReadsTheClusterButNotSecrets", func(t *testing.T) {
		if e.pod.Spec.ServiceAccountName != dataPlaneServiceAccount {
			t.Fatalf("pod runs as %q, want the dedicated %q identity",
				e.pod.Spec.ServiceAccountName, dataPlaneServiceAccount)
		}
		if e.pod.Spec.AutomountServiceAccountToken != nil && !*e.pod.Spec.AutomountServiceAccountToken {
			t.Fatal("the session pod mounts no ServiceAccount token; AC-E6 gives it read-only cluster discovery")
		}

		user := "system:serviceaccount:" + e.ns + ":" + dataPlaneServiceAccount
		for _, verb := range []string{"get", "list", "watch"} {
			if !e6Allowed(t, e.cs, user, e.ns, verb, "pods") {
				t.Fatalf("%s cannot %s pods; the data plane is meant to have read-only discovery", user, verb)
			}
			if e6Allowed(t, e.cs, user, e.ns, verb, "secrets") {
				t.Fatalf("%s can %s secrets — that the identity cannot is the whole point of holding credentials in a sidecar",
					user, verb)
			}
		}
	})

	t.Run("NeitherTokenIsEchoedByThePublicAPI", func(t *testing.T) {
		data := e.credentialsSecret(t)
		_, body := do(t, http.MethodGet, "/api/v1/sessions/"+e.session.ID, nil)
		read := readShellAt(t, e.session.ID, 0)
		for _, surface := range []struct{ name, text string }{
			{"session lookup response", string(body)},
			{"session output", read.Payload},
		} {
			for _, key := range []string{"auth-token", "k3s-mcp-token"} {
				if value := data[key]; value != "" && strings.Contains(surface.text, value) {
					t.Fatalf("%s contains the %s value", surface.name, key)
				}
			}
		}
	})
}

// A concrete model is a literal on the main container and outranks the Secret
// default — the half of the model contract a platform-default session cannot
// show.
func TestCredentialPlacement_ConcreteModelLiteralBeatsThePlatformDefault(t *testing.T) {
	const concrete = "claude-e2e-alternate"
	e, ok := newE6Session(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code", "model": concrete,
	})
	if e.session.Model != concrete {
		t.Fatalf("created session model=%q, want %q", e.session.Model, concrete)
	}
	if !ok {
		return
	}
	main := e.container(t, workloadContainer)
	e6RequireLiteral(t, main, "CLAUDE_CODE_MODEL", concrete)
	e6RequireAbsent(t, e.container(t, credentialsContainer), "CLAUDE_CODE_MODEL")

	if got := strings.TrimSpace(e.sh(t, workloadContainer, `printf %s "$CLAUDE_CODE_MODEL"`)); got != concrete {
		t.Fatalf("main container resolved CLAUDE_CODE_MODEL=%q, want the literal %q — the Secret default must not win",
			got, concrete)
	}
}

// Credentials cannot be chosen by the caller. The create DTO decodes strictly,
// so a credential-shaped field is an unknown field and no session is created.
func TestCredentialPlacement_CredentialFieldsCannotBeSetByTheCreateRequest(t *testing.T) {
	for _, field := range []string{"authToken", "baseUrl", "anthropicAuthToken", "k3sMcpToken"} {
		t.Run(field, func(t *testing.T) {
			name := uniqueName(t)
			status, _ := createTyped(t, map[string]any{
				"name": name, "workloadType": "claude-code", field: "caller-supplied-value",
			})
			if status != http.StatusBadRequest {
				t.Fatalf("create with %q: status=%d, want 400", field, status)
			}
			resp, body := do(t, http.MethodGet, "/api/v1/sessions", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("list sessions: status=%d body=%s", resp.StatusCode, body)
			}
			if strings.Contains(string(body), name) {
				t.Fatalf("the rejected create with %q still produced a session named %q", field, name)
			}
		})
	}
}
