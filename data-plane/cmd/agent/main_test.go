package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestAgent wraps a shell in an agent holder so routes can serve it, without
// arming the exit-triggers-restart watch (tests kill shells deliberately).
func newTestAgent(sh *shellProc) *agent { return &agent{sh: sh} }

// startTestShell starts the real default shell and guarantees it is reaped.
func startTestShell(t *testing.T) *shellProc {
	t.Helper()
	sh, err := startShell(defaultShell)
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(func() {
		_ = sh.kill()
		<-sh.done
	})
	return sh
}

// AC-D1: the started shell is attached to a PTY — its stdin is a PTY slave —
// and exactly one shell exists (one startShell call spawns one process).
func TestStartShellAttachesPTY(t *testing.T) {
	sh := startTestShell(t)
	if !sh.alive.Load() {
		t.Fatal("shell not alive after start")
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", sh.pid))
	if err != nil {
		t.Fatalf("read shell stdin link: %v", err)
	}
	if !strings.HasPrefix(link, "/dev/pts/") {
		t.Fatalf("shell stdin = %q, want a PTY slave (/dev/pts/*) (AC-D1)", link)
	}
}

// /healthz mirrors shell liveness: 200 while the shell runs, 503 once it has
// exited — which is what makes the pod readiness probe mean "shell alive".
func TestHealthzReflectsShellLiveness(t *testing.T) {
	sh := startTestShell(t)
	h := routes(testLogger(), newTestAgent(sh))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz with live shell = %d, want 200", rec.Code)
	}

	_ = sh.kill()
	<-sh.done

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with dead shell = %d, want 503", rec.Code)
	}
}

// scrollback accumulates appends in order and Since serves offset deltas:
// 0 replays everything, a prior cursor yields only what came after it, and an
// offset at/past the end yields an empty payload with the current cursor.
func TestScrollbackSinceDeltas(t *testing.T) {
	var b scrollback
	b.Append([]byte("alpha\n"))
	b.Append([]byte("beta\n"))

	full, next := b.Since(0)
	if string(full) != "alpha\nbeta\n" {
		t.Fatalf("Since(0) = %q, want appends in order (AC-D3)", full)
	}
	if next != len("alpha\nbeta\n") {
		t.Fatalf("Since(0) cursor = %d, want %d", next, len("alpha\nbeta\n"))
	}

	b.Append([]byte("gamma\n"))
	delta, next2 := b.Since(next)
	if string(delta) != "gamma\n" {
		t.Fatalf("Since(%d) = %q, want only the new output", next, delta)
	}
	if next2 != next+len("gamma\n") {
		t.Fatalf("delta cursor = %d, want %d", next2, next+len("gamma\n"))
	}

	// Non-consuming: offset 0 still replays the full history.
	full2, _ := b.Since(0)
	if string(full2) != "alpha\nbeta\ngamma\n" {
		t.Fatalf("Since(0) after deltas = %q, want full history", full2)
	}

	// At/past the end: empty payload, cursor pinned to the current length.
	empty, n := b.Since(next2)
	if len(empty) != 0 || n != next2 {
		t.Fatalf("Since(end) = (%q, %d), want empty payload and cursor %d", empty, n, next2)
	}
	empty, n = b.Since(next2 + 100)
	if len(empty) != 0 || n != next2 {
		t.Fatalf("Since(past end) = (%q, %d), want empty payload and cursor %d", empty, n, next2)
	}
}

// readViaHTTP hits GET /read?offset=N and decodes the JSON result.
func readViaHTTP(t *testing.T, srv *httptest.Server, offset int) (string, int) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/read?offset=%d", srv.URL, offset))
	if err != nil {
		t.Fatalf("GET /read: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /read status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Payload    string `json:"payload"`
		NextOffset int    `json:"nextOffset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /read: %v", err)
	}
	return out.Payload, out.NextOffset
}

// writeViaHTTP posts a raw payload to /write, asserting the given status.
func writeViaHTTP(t *testing.T, srv *httptest.Server, payload string, wantStatus int) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/write", "application/octet-stream", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /write: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST /write status = %d, want %d", resp.StatusCode, wantStatus)
	}
}

// eventuallyRead polls /read?offset=N until the payload satisfies ok, failing
// after the deadline. Shell output timing is non-deterministic, so assertions
// are containment + eventually, never exact matches.
func eventuallyRead(t *testing.T, srv *httptest.Server, offset int, ok func(payload string) bool) (string, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var payload string
	var next int
	for time.Now().Before(deadline) {
		payload, next = readViaHTTP(t, srv, offset)
		if ok(payload) {
			return payload, next
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("read at offset %d never satisfied the condition; last payload = %q", offset, payload)
	return "", 0
}

// AC-D2 + AC-D3 happy path: a command written to /write is executed by the
// shell (the PTY echoes the input and the shell prints the result) and its
// output is recovered via /read.
func TestWriteThenReadRecoversOutput(t *testing.T) {
	sh := startTestShell(t)
	srv := httptest.NewServer(routes(testLogger(), newTestAgent(sh)))
	defer srv.Close()

	writeViaHTTP(t, srv, "echo agent-marker-$((20+22))\n", http.StatusOK)

	// The command substitution result only exists in the output once bash ran
	// the command — the echoed input line alone cannot contain "agent-marker-42".
	payload, next := eventuallyRead(t, srv, 0, func(p string) bool {
		return strings.Contains(p, "agent-marker-42")
	})
	if !strings.Contains(payload, "agent-marker-42") {
		t.Fatalf("read payload %q missing command output (AC-D2/D3)", payload)
	}
	if next < len(payload) {
		t.Fatalf("nextOffset = %d < payload length %d", next, len(payload))
	}
}

// AC-D3: a cursor read returns only the delta after the cursor, while offset 0
// keeps returning the full history (non-consuming).
func TestReadCursorReturnsDelta(t *testing.T) {
	sh := startTestShell(t)
	srv := httptest.NewServer(routes(testLogger(), newTestAgent(sh)))
	defer srv.Close()

	writeViaHTTP(t, srv, "echo first-$((40+2))\n", http.StatusOK)
	_, cursor := eventuallyRead(t, srv, 0, func(p string) bool {
		return strings.Contains(p, "first-42")
	})

	// No new output: reading at the cursor eventually stabilises to empty.
	if payload, _ := readViaHTTP(t, srv, cursor); strings.Contains(payload, "first-42") {
		t.Fatalf("cursor read replayed old output %q, want delta only", payload)
	}

	writeViaHTTP(t, srv, "echo second-$((40+3))\n", http.StatusOK)
	delta, _ := eventuallyRead(t, srv, cursor, func(p string) bool {
		return strings.Contains(p, "second-43")
	})
	if strings.Contains(delta, "first-42") {
		t.Fatalf("cursor read %q contains pre-cursor output, want only the delta (AC-D3)", delta)
	}

	// offset=0 still replays everything, in order.
	full, _ := readViaHTTP(t, srv, 0)
	first := strings.Index(full, "first-42")
	second := strings.Index(full, "second-43")
	if first == -1 || second == -1 || first > second {
		t.Fatalf("full read %q must contain first-42 then second-43 (order preserved)", full)
	}
}

// /read validates the offset parameter.
func TestReadRejectsBadOffset(t *testing.T) {
	sh := startTestShell(t)
	srv := httptest.NewServer(routes(testLogger(), newTestAgent(sh)))
	defer srv.Close()

	for _, off := range []string{"abc", "-1"} {
		resp, err := http.Get(srv.URL + "/read?offset=" + off)
		if err != nil {
			t.Fatalf("GET /read?offset=%s: %v", off, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("offset=%s status = %d, want 400", off, resp.StatusCode)
		}
	}
}

// /write refuses once the shell has exited (503, like /attach), while /read
// keeps serving the accumulated history — it is a record, not a pipe.
func TestWriteDeadShell503ReadStillServes(t *testing.T) {
	sh := startTestShell(t)
	srv := httptest.NewServer(routes(testLogger(), newTestAgent(sh)))
	defer srv.Close()

	_ = sh.kill()
	<-sh.done

	writeViaHTTP(t, srv, "echo nope\n", http.StatusServiceUnavailable)
	_, _ = readViaHTTP(t, srv, 0) // must still answer 200
}

// /attach upgrades to a WebSocket and survives an immediate open/close — the
// exact reachability handshake the control plane performs (J5-S1).
func TestAttachUpgradesAndCloses(t *testing.T) {
	sh := startTestShell(t)
	srv := httptest.NewServer(routes(testLogger(), newTestAgent(sh)))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/attach"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %d, want 101", resp.StatusCode)
	}
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	if err := conn.Close(); err != nil {
		t.Fatalf("close attach: %v", err)
	}
}
