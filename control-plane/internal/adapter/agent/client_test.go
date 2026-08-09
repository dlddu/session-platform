package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const testCheckpointIDHeader = "X-Session-Checkpoint-ID"

// fakeAgent is an httptest stand-in for the data plane agent's /write, /read,
// /checkpoint and /restore endpoints, recording writes and serving scrollback
// deltas / checkpoint archives.
type fakeAgent struct {
	buf         []byte
	writeStatus int

	streamOffset      string
	streamStatus      int
	streamContentType string

	checkpointBody      []byte // archive returned by /checkpoint
	checkpointStatus    int    // non-200 to force a checkpoint error
	checkpointID        string
	checkpointRequestID string
	restoreBody         []byte // archive received by /restore
	restoreStatus       int    // non-200 to force a restore error
	abortCalls          int
	abortStatus         int // non-200 to force an abort error
	abortCheckpointID   string
}

func (a *fakeAgent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.buf = append(a.buf, body...)
		if a.writeStatus != 0 && a.writeStatus != http.StatusOK {
			http.Error(w, http.StatusText(a.writeStatus), a.writeStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /checkpoint", func(w http.ResponseWriter, r *http.Request) {
		a.checkpointRequestID = r.Header.Get(testCheckpointIDHeader)
		if a.checkpointStatus != 0 && a.checkpointStatus != http.StatusOK {
			http.Error(w, "checkpoint failed", a.checkpointStatus)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		responseID := a.checkpointID
		if responseID == "" {
			responseID = a.checkpointRequestID
		}
		w.Header().Set(testCheckpointIDHeader, responseID)
		_, _ = w.Write(a.checkpointBody)
	})
	mux.HandleFunc("POST /restore", func(w http.ResponseWriter, r *http.Request) {
		a.restoreBody, _ = io.ReadAll(r.Body)
		if a.restoreStatus != 0 && a.restoreStatus != http.StatusOK {
			http.Error(w, "restore failed", a.restoreStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restored"}`))
	})
	mux.HandleFunc("POST /checkpoint/abort", func(w http.ResponseWriter, r *http.Request) {
		a.abortCalls++
		a.abortCheckpointID = r.Header.Get(testCheckpointIDHeader)
		if a.abortStatus != 0 && a.abortStatus != http.StatusOK {
			http.Error(w, "abort failed", a.abortStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"aborted"}`))
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		a.streamOffset = r.URL.Query().Get("offset")
		if a.streamStatus != 0 && a.streamStatus != http.StatusOK {
			http.Error(w, "stream failed", a.streamStatus)
			return
		}
		contentType := a.streamContentType
		if contentType == "" {
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = fmt.Fprint(w, "id: 5\nevent: output\ndata: {\"offset\":2,\"payloadBase64\":\"YWJj\",\"nextOffset\":5}\n\n")
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
func TestHTTPClientStreamProxiesSSEAtCursor(t *testing.T) {
	c, fa := harness(t, "sess-ab12")
	body, err := c.Stream(context.Background(), "sess-ab12", 2)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer body.Close()
	wire, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read stream body: %v", err)
	}
	if fa.streamOffset != "2" {
		t.Fatalf("agent stream offset = %q, want 2", fa.streamOffset)
	}
	if got := string(wire); !strings.Contains(got, "event: output") || !strings.Contains(got, `"nextOffset":5`) {
		t.Fatalf("stream wire = %q", got)
	}
}

func TestHTTPClientStreamValidatesAgentResponse(t *testing.T) {
	c, fa := harness(t, "sess-ab12")
	fa.streamStatus = http.StatusBadRequest
	if _, err := c.Stream(context.Background(), "sess-ab12", 0); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatalf("bad stream cursor err = %v, want invalid input", err)
	}

	fa.streamStatus = http.StatusOK
	fa.streamContentType = "application/json"
	if _, err := c.Stream(context.Background(), "sess-ab12", 0); err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("unexpected stream content type err = %v", err)
	}
}

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

// Checkpoint resolves the pod and returns the agent's archive stream verbatim
// (the criu.AgentCheckpointer then persists it).
func TestCheckpointReturnsArchiveStream(t *testing.T) {
	c, fa := harness(t, "sess-cp")
	fa.checkpointBody = []byte("CRIU-ARCHIVE-TAR-BYTES")
	fa.checkpointID = "generation-1"

	rc, checkpointID, err := c.Checkpoint(context.Background(), "sess-cp")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	defer rc.Close()
	if checkpointID != "generation-1" {
		t.Fatalf("checkpoint ID = %q, want generation-1", checkpointID)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "CRIU-ARCHIVE-TAR-BYTES" {
		t.Fatalf("checkpoint archive = %q, want the agent body verbatim", got)
	}
}

func TestCheckpointWithGenerationSendsDurableID(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	c, fa := harness(t, "sess-cp")
	fa.checkpointBody = []byte("archive")

	rc, gotID, err := c.CheckpointWithGeneration(context.Background(), "sess-cp", generation)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	defer rc.Close()
	if fa.checkpointRequestID != generation {
		t.Fatalf("request generation = %q, want %q", fa.checkpointRequestID, generation)
	}
	if gotID != generation {
		t.Fatalf("response generation = %q, want %q", gotID, generation)
	}
}

// A non-200 from the agent surfaces as a Checkpoint error rather than a bogus
// (empty) archive.
func TestCheckpointSurfacesAgentError(t *testing.T) {
	c, fa := harness(t, "sess-cp")
	fa.checkpointStatus = http.StatusServiceUnavailable
	if _, _, err := c.Checkpoint(context.Background(), "sess-cp"); err == nil {
		t.Fatal("checkpoint succeeded despite agent 503; want error")
	}
}

func TestAbortCheckpointCallsAgent(t *testing.T) {
	c, fa := harness(t, "sess-cp")
	if err := c.AbortCheckpoint(context.Background(), "sess-cp", "generation-1"); err != nil {
		t.Fatalf("abort checkpoint: %v", err)
	}
	if fa.abortCalls != 1 {
		t.Fatalf("abort calls = %d, want 1", fa.abortCalls)
	}
	if fa.abortCheckpointID != "generation-1" {
		t.Fatalf("abort checkpoint ID = %q, want generation-1", fa.abortCheckpointID)
	}
}

func TestAbortCheckpointSurfacesAgentError(t *testing.T) {
	c, fa := harness(t, "sess-cp")
	fa.abortStatus = http.StatusConflict
	err := c.AbortCheckpoint(context.Background(), "sess-cp", "generation-1")
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("abort error = %v, want agent 409", err)
	}
}

// Restore streams the archive to the agent's /restore.
func TestRestoreStreamsArchiveToAgent(t *testing.T) {
	c, fa := harness(t, "sess-r")
	if err := c.Restore(context.Background(), "sess-r", strings.NewReader("RESTORE-ARCHIVE")); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if string(fa.restoreBody) != "RESTORE-ARCHIVE" {
		t.Fatalf("agent received %q, want the streamed archive", fa.restoreBody)
	}
}

// A non-200 from /restore surfaces as an error.
func TestRestoreSurfacesAgentError(t *testing.T) {
	c, fa := harness(t, "sess-r")
	fa.restoreStatus = http.StatusInternalServerError
	if err := c.Restore(context.Background(), "sess-r", strings.NewReader("x")); err == nil {
		t.Fatal("restore succeeded despite agent 500; want error")
	}
}

func TestHTTPClientWritePreservesAdmissionErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "queue full", status: http.StatusTooManyRequests, want: session.ErrWorkloadQueueFull},
		{name: "prompt too large", status: http.StatusRequestEntityTooLarge, want: session.ErrWorkloadPromptTooLarge},
		{name: "output full", status: http.StatusInsufficientStorage, want: session.ErrWorkloadOutputFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, fa := harness(t, "sess-limited")
			fa.writeStatus = tc.status
			err := c.Write(context.Background(), "sess-limited", "prompt")
			if !errors.Is(err, tc.want) {
				t.Fatalf("write error = %v, want errors.Is(_, %v)", err, tc.want)
			}
		})
	}
}
