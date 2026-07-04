package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
)

// fakeAgent is an httptest stand-in for the data plane agent's /write and
// /read endpoints, recording writes and serving scrollback deltas.
type fakeAgent struct {
	buf []byte
}

func (a *fakeAgent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.buf = append(a.buf, body...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /read", func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		n := len(a.buf)
		payload := ""
		if offset >= 0 && offset < n {
			payload = string(a.buf[offset:])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"payload": payload, "nextOffset": n})
	})
	return mux
}

// harness serves a fake agent on the loopback and registers a Running pod
// whose IP resolves to it, mirroring how the real client dials pod IP:port.
func harness(t *testing.T, podName string) (*agent.HTTPClient, *fakeAgent) {
	t.Helper()
	fa := &fakeAgent{}
	srv := httptest.NewServer(fa.handler())
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split httptest addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "sessions"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host},
	})
	return agent.NewHTTPClient(cs, "sessions", agent.WithPort(port)), fa
}

// Write resolves the pod IP per request and forwards the payload verbatim to
// the agent's /write (AC-D2).
func TestHTTPClientWriteForwardsPayload(t *testing.T) {
	c, fa := harness(t, "sess-ab12")

	if err := c.Write(context.Background(), "sess-ab12", "echo hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(fa.buf); got != "echo hello\n" {
		t.Fatalf("agent received %q, want the verbatim payload", got)
	}
}

// Read passes the offset through and returns the delta plus nextOffset
// (AC-D3) — offset 0 is the full history, the cursor read only the delta.
func TestHTTPClientReadCursor(t *testing.T) {
	c, fa := harness(t, "sess-ab12")
	fa.buf = []byte("alpha\n")

	full, next, err := c.Read(context.Background(), "sess-ab12", 0)
	if err != nil {
		t.Fatalf("read 0: %v", err)
	}
	if full != "alpha\n" || next != int64(len("alpha\n")) {
		t.Fatalf("read 0 = (%q, %d), want (alpha\\n, %d)", full, next, len("alpha\n"))
	}

	fa.buf = append(fa.buf, "beta\n"...)
	delta, next2, err := c.Read(context.Background(), "sess-ab12", next)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if delta != "beta\n" {
		t.Fatalf("cursor read = %q, want only the delta", delta)
	}
	if next2 != next+int64(len("beta\n")) {
		t.Fatalf("cursor = %d, want %d", next2, next+int64(len("beta\n")))
	}
}

// A pod the API server does not know is an error, not a silent no-op.
func TestHTTPClientUnknownPod(t *testing.T) {
	c := agent.NewHTTPClient(fake.NewSimpleClientset(), "sessions")
	if err := c.Write(context.Background(), "sess-gone", "x"); err == nil {
		t.Fatal("write to unknown pod succeeded, want error")
	}
	if _, _, err := c.Read(context.Background(), "sess-gone", 0); err == nil {
		t.Fatal("read from unknown pod succeeded, want error")
	}
}

// A pod without a routable IP (not Running yet) must be refused before any
// dial is attempted.
func TestHTTPClientPodNotRunning(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-cold", Namespace: "sessions"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	})
	c := agent.NewHTTPClient(cs, "sessions")
	if err := c.Write(context.Background(), "sess-cold", "x"); err == nil {
		t.Fatal("write to pending pod succeeded, want error")
	}
}

// Non-200 agent answers surface as errors with the status attached.
func TestHTTPClientAgentErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "shell exited", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-dead", Namespace: "sessions"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: host},
	})
	c := agent.NewHTTPClient(cs, "sessions", agent.WithPort(port))

	err := c.Write(context.Background(), "sess-dead", "x")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("write error = %v, want the agent's 503 surfaced", err)
	}
	if _, _, err := c.Read(context.Background(), "sess-dead", 0); err == nil {
		t.Fatal("read error = nil, want the agent's 503 surfaced")
	}
}

// The stub client honours the same cursor semantics the service tests rely on.
func TestStubClientCursorSemantics(t *testing.T) {
	c := agent.NewStubClient()
	ctx := context.Background()

	if err := c.Write(ctx, "p1", "one"); err != nil {
		t.Fatalf("write: %v", err)
	}
	full, next, err := c.Read(ctx, "p1", 0)
	if err != nil || full != "one" || next != 3 {
		t.Fatalf("read 0 = (%q, %d, %v), want (one, 3, nil)", full, next, err)
	}
	_ = c.Write(ctx, "p1", "two")
	delta, next2, _ := c.Read(ctx, "p1", next)
	if delta != "two" || next2 != 6 {
		t.Fatalf("cursor read = (%q, %d), want (two, 6)", delta, next2)
	}
	empty, n, _ := c.Read(ctx, "p1", 99)
	if empty != "" || n != 6 {
		t.Fatalf("past-end read = (%q, %d), want empty and current length", empty, n)
	}
	// pods are isolated
	other, n0, _ := c.Read(ctx, "p2", 0)
	if other != "" || n0 != 0 {
		t.Fatalf("unwritten pod read = (%q, %d), want empty", other, n0)
	}
}
