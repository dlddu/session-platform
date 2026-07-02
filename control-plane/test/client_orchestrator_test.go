//go:build integration

// This file exercises the *real* client-go PodOrchestrator against a fake
// clientset (no cluster needed), asserting the create/label/1:1/delete contract
// and the shell-agent pod spec (AC-D1) that the in-memory stub can only
// approximate. What the fake cannot verify — an actual PTY shell running inside
// the pod — is asserted at runtime by the kind e2e suite (e2e_shell_test.go).
// The HTTP happy-path scenarios in integration_test.go still run against the
// stub adapters.
package integration_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
)

const testNS = "sessions"

// testPodIP is the pod IP the fake clientset stamps on created pods, standing
// in for the kubelet-assigned address Start records into PodRef.
const testPodIP = "10.244.7.42"

// newReadyOrchestrator returns a ClientOrchestrator backed by a fake clientset
// that immediately marks created pods Running+Ready with a pod IP. The fake has
// no kubelet to transition pods, so without this the orchestrator's readiness
// wait would block until timeout. The short poll/timeout keeps the test fast.
func newReadyOrchestrator(t *testing.T, opts ...k8s.Option) (*k8s.ClientOrchestrator, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = testPodIP
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		})
		// Fall through (handled=false) to the tracker, which persists the pod we
		// just mutated, so a later Get observes it Ready.
		return false, pod, nil
	})
	opts = append([]k8s.Option{k8s.WithReadiness(time.Millisecond, 5*time.Second)}, opts...)
	orch := k8s.NewClientOrchestrator(cs, testNS, opts...)
	return orch, cs
}

func listPods(t *testing.T, cs *fake.Clientset) []corev1.Pod {
	t.Helper()
	pods, err := cs.CoreV1().Pods(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return pods.Items
}

// Scenario 1 (AC-A1/A2): Start creates exactly one pod in the orchestrator's
// namespace, labelled 1:1 to the session.
func TestClientOrchestrator_StartCreatesOnePodWithLabel(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	ref, err := orch.Start(context.Background(), "a1b2")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ref.Namespace != testNS {
		t.Fatalf("ref namespace=%q want %q", ref.Namespace, testNS)
	}
	pods := listPods(t, cs)
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods))
	}
	p := pods[0]
	if p.Name != ref.Name {
		t.Fatalf("pod name=%q want %q (Start ref)", p.Name, ref.Name)
	}
	if p.Namespace != testNS {
		t.Fatalf("pod namespace=%q want %q", p.Namespace, testNS)
	}
	if got := p.Labels[k8s.LabelSessionID]; got != "a1b2" {
		t.Fatalf("%s label=%q want %q (AC-A2 1:1)", k8s.LabelSessionID, got, "a1b2")
	}
	if len(p.Spec.Containers) != 1 || p.Spec.Containers[0].Image == "" {
		t.Fatalf("expected one container with an image, got %+v", p.Spec.Containers)
	}
}

// Scenario 2 (AC-A2): N sessions => N unique pods, each labelled to its session.
func TestClientOrchestrator_NSessionsNUniquePods(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	ids := []string{"aa01", "bb02", "cc03", "dd04"}
	names := map[string]bool{}
	for _, id := range ids {
		ref, err := orch.Start(context.Background(), id)
		if err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		names[ref.Name] = true
	}
	if len(names) != len(ids) {
		t.Fatalf("expected %d unique pod names, got %d: %v", len(ids), len(names), names)
	}
	pods := listPods(t, cs)
	if len(pods) != len(ids) {
		t.Fatalf("expected %d pods, got %d", len(ids), len(pods))
	}
	for _, p := range pods {
		if p.Labels[k8s.LabelSessionID] == "" {
			t.Fatalf("pod %s missing %s label", p.Name, k8s.LabelSessionID)
		}
	}
}

// AC-D1 (pod spec side): the data plane image's entrypoint owns the PTY shell,
// so the orchestrator must not override the container command; the agent port
// is declared and readiness is the agent's /healthz — making "pod Ready" mean
// "shell alive". The runtime side (an actual PTY shell in the pod) is asserted
// by the kind e2e suite.
func TestClientOrchestrator_PodSpecRunsShellAgent(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	if _, err := orch.Start(context.Background(), "d1a1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	pods := listPods(t, cs)
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods))
	}
	c := pods[0].Spec.Containers[0]

	if len(c.Command) != 0 || len(c.Args) != 0 {
		t.Fatalf("container overrides the image entrypoint (command=%v args=%v); the agent must own the shell (AC-D1)", c.Command, c.Args)
	}
	for _, env := range c.Env {
		if env.Name == "DATA_PLANE_SHELL" {
			t.Fatalf("DATA_PLANE_SHELL env present without an override: %q", env.Value)
		}
	}

	var port *corev1.ContainerPort
	for i := range c.Ports {
		if c.Ports[i].ContainerPort == k8s.AgentPort {
			port = &c.Ports[i]
		}
	}
	if port == nil {
		t.Fatalf("agent port %d not declared on the container (ports=%v)", k8s.AgentPort, c.Ports)
	}

	rp := c.ReadinessProbe
	if rp == nil || rp.HTTPGet == nil {
		t.Fatal("expected an HTTP readiness probe against the agent (pod Ready must mean shell alive, AC-D1)")
	}
	if rp.HTTPGet.Path != "/healthz" {
		t.Fatalf("readiness path=%q want /healthz", rp.HTTPGet.Path)
	}
	if got := rp.HTTPGet.Port.IntValue(); got != k8s.AgentPort {
		t.Fatalf("readiness port=%d want %d", got, k8s.AgentPort)
	}
}

// AC-D1 (shell override): WithShell propagates DATA_PLANE_SHELL into the pod so
// the agent launches the configured shell instead of /bin/bash.
func TestClientOrchestrator_PodSpecPropagatesShellOverride(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithShell("/bin/zsh"))
	if _, err := orch.Start(context.Background(), "d1b2"); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := listPods(t, cs)[0].Spec.Containers[0]
	var got string
	for _, env := range c.Env {
		if env.Name == "DATA_PLANE_SHELL" {
			got = env.Value
		}
	}
	if got != "/bin/zsh" {
		t.Fatalf("DATA_PLANE_SHELL env=%q want /bin/zsh", got)
	}
}

// AC-D1 (transport): Start records the Ready pod's IP so the control plane can
// dial the session agent without re-fetching the pod.
func TestClientOrchestrator_StartRecordsPodIP(t *testing.T) {
	orch, _ := newReadyOrchestrator(t)
	ref, err := orch.Start(context.Background(), "d1c3")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ref.IP != testPodIP {
		t.Fatalf("ref.IP=%q want %q (pod IP recorded at Ready)", ref.IP, testPodIP)
	}
}

// AC-D1 (reachability): Reach opens the agent's /attach WebSocket stream and
// closes it — success against a live endpoint, an error against a dead one,
// and an error for refs without an IP.
func TestClientOrchestrator_ReachOpensAttachStream(t *testing.T) {
	upgraded := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/attach" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgraded <- struct{}{}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return // peer closed — Reach hangs up right after opening
			}
		}
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split httptest addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	orch, _ := newReadyOrchestrator(t, k8s.WithAgentPort(port))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := orch.Reach(ctx, k8s.PodRef{Name: "sess-ok", IP: host}); err != nil {
		t.Fatalf("reach against live agent: %v", err)
	}
	select {
	case <-upgraded:
	default:
		t.Fatal("agent never saw the attach stream open")
	}

	// A dead endpoint (fresh unused port) must surface as an error.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dead port: %v", err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	dead.Close()
	orchDead, _ := newReadyOrchestrator(t, k8s.WithAgentPort(deadPort))
	if err := orchDead.Reach(ctx, k8s.PodRef{Name: "sess-dead", IP: "127.0.0.1"}); err == nil {
		t.Fatal("reach against dead endpoint succeeded; want error (AC-D1 gate)")
	}

	// A ref without an IP (e.g. rebuilt from stored state) cannot be dialled.
	if err := orch.Reach(ctx, k8s.PodRef{Name: "sess-noip"}); err == nil {
		t.Fatal("reach without pod IP succeeded; want error")
	}
}

// Scenario 3 (AC-A3): Stop deletes the pod and is idempotent.
func TestClientOrchestrator_StopDeletesPodIdempotently(t *testing.T) {
	orch, cs := newReadyOrchestrator(t)
	ref, err := orch.Start(context.Background(), "ee05")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if n := len(listPods(t, cs)); n != 1 {
		t.Fatalf("expected 1 pod after start, got %d", n)
	}
	if err := orch.Stop(context.Background(), ref); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if n := len(listPods(t, cs)); n != 0 {
		t.Fatalf("expected 0 pods after stop, got %d", n)
	}
	// Deleting an already-gone pod is not an error (AC-A3 reclaim is idempotent).
	if err := orch.Stop(context.Background(), ref); err != nil {
		t.Fatalf("stop (idempotent) returned error: %v", err)
	}
}
