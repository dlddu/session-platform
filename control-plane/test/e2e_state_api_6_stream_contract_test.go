//go:build e2e

// 검증 시나리오: state-api.md#시나리오 6
//
// 무엇을 단언하고 무엇이 범위 밖인지는 docs/test/e2e.md 의 매핑 행이 갖는다. 그 행이
// 담지 않는 것 하나만 적어 둔다 — 커서 계약 중 둘(`Last-Event-ID` 우선 · past-end
// reset)은 claude-code-workload.md#시나리오 4 의 저작 대기 행도 자기 몫으로 열거한다.
// 두 행을 나란히 놓아야 보이는 그 겹침이 중복이 아닌 이유는 실어 나르는 바이트가 달라서다.
package e2e_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	// control-plane/internal/session ErrInvalidInput / ErrInvalidState, written
	// out rather than imported for the reason e2e_f1 gives.
	s6InvalidInput = "invalid input"
	s6InvalidState = "session in invalid state for operation"
	// data-plane/cmd/agent/output_stream.go outputStreamHeartbeat, same.
	s6Heartbeat  = 15 * time.Second
	s6AlphaCmd   = "echo s6-alpha-$((20+3))\n"
	s6AlphaToken = "s6-alpha-23"
	s6BetaCmd    = "echo s6-beta-$((30+1))\n"
	s6BetaToken  = "s6-beta-31"
	s6GammaCmd   = "echo s6-gamma-$((40+1))\n"
	s6GammaToken = "s6-gamma-41"
	s6DeltaCmd   = "echo s6-delta-$((50+3))\n"
	s6DeltaToken = "s6-delta-53"
)

type s6Frame struct {
	Comment string
	ID      string
	Event   string
	Data    []byte
}

type s6OutputData struct {
	Offset        int64  `json:"offset"`
	PayloadBase64 string `json:"payloadBase64"`
	NextOffset    int64  `json:"nextOffset"`
}

type s6ResetData struct {
	NextOffset int64 `json:"nextOffset"`
}

// The shared `client` is deliberately not used: its fixed 90s timeout would cut
// a live stream at a point unrelated to what the test is waiting for, so each
// stream carries its own budget instead.
type s6Stream struct {
	resp   *http.Response
	cancel context.CancelFunc
	sc     *bufio.Scanner
	closed bool
}

func s6Open(t *testing.T, id, query, lastEventID string, budget time.Duration) *s6Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	url := baseURL() + "/api/v1/sessions/" + id + "/stream" + query
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new stream request %s: %v", url, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream %s: %v (is the SUT up? run `make e2e-up` or set E2E_BASE_URL)", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("open stream %s: status=%d body=%s", url, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		cancel()
		t.Fatalf("stream content-type=%q, want text/event-stream", ct)
	}
	s := &s6Stream{resp: resp, cancel: cancel, sc: bufio.NewScanner(resp.Body)}
	// One output frame carries up to outputStreamChunkBytes (64 KiB) of bytes as
	// base64, so the scanner's default 64 KiB token limit would split a data line
	// and turn a contract failure into a parse failure.
	s.sc.Buffer(make([]byte, 0, 256<<10), 1<<20)
	t.Cleanup(s.Close)
	return s
}

func (s *s6Stream) Close() {
	if s.closed {
		return
	}
	s.closed = true
	s.resp.Body.Close()
	s.cancel()
}

func (s *s6Stream) next(t *testing.T) s6Frame {
	t.Helper()
	var f s6Frame
	started := false
	for s.sc.Scan() {
		line := s.sc.Text()
		if line == "" {
			if started {
				return f
			}
			continue
		}
		started = true
		switch {
		case strings.HasPrefix(line, ":"):
			f.Comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "id:"):
			f.ID = strings.TrimSpace(line[len("id:"):])
		case strings.HasPrefix(line, "event:"):
			f.Event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			f.Data = append(f.Data, strings.TrimSpace(line[len("data:"):])...)
		default:
			t.Fatalf("unparsable SSE line %q", line)
		}
	}
	if err := s.sc.Err(); err != nil {
		t.Fatalf("stream read failed with %d frame bytes buffered: %v", len(f.Data), err)
	}
	t.Fatal("stream closed before the next frame arrived")
	return f
}

func (s *s6Stream) nextEvent(t *testing.T) (s6Frame, int) {
	t.Helper()
	comments := 0
	for {
		f := s.next(t)
		if f.Comment != "" {
			comments++
			continue
		}
		return f, comments
	}
}

func s6Output(t *testing.T, f s6Frame) (s6OutputData, []byte) {
	t.Helper()
	if f.Event != "output" {
		t.Fatalf("frame event=%q want output (data=%s)", f.Event, f.Data)
	}
	var d s6OutputData
	if err := json.Unmarshal(f.Data, &d); err != nil {
		t.Fatalf("decode output data: %v raw=%s", err, f.Data)
	}
	if f.ID != strconv.FormatInt(d.NextOffset, 10) {
		t.Fatalf("output event id=%q but data.nextOffset=%d — a reconnecting client resumes from the id, so the two must be one cursor", f.ID, d.NextOffset)
	}
	payload, err := base64.StdEncoding.DecodeString(d.PayloadBase64)
	if err != nil {
		t.Fatalf("decode payloadBase64: %v", err)
	}
	if int64(len(payload)) != d.NextOffset-d.Offset {
		t.Fatalf("event carried %d decoded bytes but moved the cursor %d (offset=%d nextOffset=%d)", len(payload), d.NextOffset-d.Offset, d.Offset, d.NextOffset)
	}
	return d, payload
}

func s6ErrorField(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, body)
	}
	return e.Error
}

// Local rather than shared for the reason a5Settled gives, and kept in step with it.
func s6Settled(t *testing.T, id string) readResp {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	prev := readShellAt(t, id, 0)
	for {
		time.Sleep(500 * time.Millisecond)
		cur := readShellAt(t, id, 0)
		if cur.Payload == prev.Payload && cur.NextOffset == prev.NextOffset {
			return cur
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s output never settled; last two reads ended at %d and %d", id, prev.NextOffset, cur.NextOffset)
		}
		prev = cur
	}
}

func s6ActiveWithOutput(t *testing.T, command, token string) (session, readResp) {
	t.Helper()
	s := createSession(t, uniqueName(t))
	writeShell(t, s.ID, command)
	eventuallyShellRead(t, s.ID, 0, func(p string) bool { return strings.Contains(p, token) })
	settled := s6Settled(t, s.ID)
	if settled.NextOffset <= 0 {
		t.Fatalf("session %s settled at offset 0 — there is no byte record to stream", s.ID)
	}
	return s, settled
}

func TestStreamContract_ActiveStreamsPassivelyAndCursorsAreContiguous(t *testing.T) {
	s, settled := s6ActiveWithOutput(t, s6AlphaCmd, s6AlphaToken)

	before := getSession(t, s.ID)
	if before.State != "active" || before.Pod == "" {
		t.Fatalf("precondition: state=%q pod=%q, want an active session holding a pod", before.State, before.Pod)
	}

	stream := s6Open(t, s.ID, "?offset=0", "", 60*time.Second)
	// Consuming the agent's opening comment is also what proves the frames below
	// were parsed as SSE framing and not as one JSON body.
	if opening := stream.next(t); opening.Comment == "" {
		t.Fatalf("first frame = %+v, want the opening comment", opening)
	}

	var acc []byte
	cursor := int64(0)
	for int64(len(acc)) < settled.NextOffset {
		f, _ := stream.nextEvent(t)
		d, payload := s6Output(t, f)
		if d.Offset != cursor {
			t.Fatalf("output event starts at %d, want %d — the byte record must arrive contiguously", d.Offset, cursor)
		}
		acc = append(acc, payload...)
		cursor = d.NextOffset
	}
	stream.Close()

	after := getSession(t, s.ID)
	if after.State != before.State {
		t.Fatalf("state moved %q -> %q across a stream; AC-E3 promotes nothing", before.State, after.State)
	}
	if after.Pod != before.Pod {
		t.Fatalf("pod moved %q -> %q across a stream; the stream must serve the retained pod", before.Pod, after.Pod)
	}
	if after.LastAccess != before.LastAccess {
		t.Fatalf("lastAccess moved %q -> %q across a stream; SSE is not activity (AC-B1)", before.LastAccess, after.LastAccess)
	}

	// The two surfaces carry the same bytes in different encodings: `read` hands
	// them back inside a JSON string (agent main.go), so a non-UTF-8 PTY byte
	// returns as U+FFFD and cannot be compared against the stream's raw bytes.
	// `echo` emits ASCII, so the comparison is exact here; the guard logs the
	// condition instead of quietly weakening the assertion.
	if !utf8.Valid(acc) {
		t.Logf("streamed bytes are not valid UTF-8; skipping the byte-for-byte comparison with read, which normalises them")
	} else if !strings.HasPrefix(string(acc), settled.Payload) {
		t.Fatalf("streamed bytes are not the read's bytes:\n stream=%q\n   read=%q", string(acc), settled.Payload)
	}
	if !strings.Contains(string(acc), s6AlphaToken) {
		t.Fatalf("streamed %d bytes without the marker %q", len(acc), s6AlphaToken)
	}
}

// Both cursors arrive at once because a native EventSource retries the original
// URL — query string and all — with the last accepted id added as a header.
func TestStreamContract_LastEventIDBeatsQueryCursor(t *testing.T) {
	s, firstHalf := s6ActiveWithOutput(t, s6BetaCmd, s6BetaToken)
	mid := firstHalf.NextOffset

	writeShell(t, s.ID, s6GammaCmd)
	eventuallyShellRead(t, s.ID, 0, func(p string) bool { return strings.Contains(p, s6GammaToken) })
	tail := s6Settled(t, s.ID)
	if tail.NextOffset <= mid {
		t.Fatalf("the second marker did not extend the record (%d -> %d)", mid, tail.NextOffset)
	}

	stream := s6Open(t, s.ID, "?offset=0", strconv.FormatInt(mid, 10), 60*time.Second)
	if opening := stream.next(t); opening.Comment == "" {
		t.Fatalf("first frame = %+v, want the opening comment", opening)
	}

	var acc []byte
	cursor := mid
	for cursor < tail.NextOffset {
		f, _ := stream.nextEvent(t)
		d, payload := s6Output(t, f)
		if d.Offset != cursor {
			t.Fatalf("output event starts at %d, want %d (Last-Event-ID=%d must win over ?offset=0)", d.Offset, cursor, mid)
		}
		acc = append(acc, payload...)
		cursor = d.NextOffset
	}

	if strings.Contains(string(acc), s6BetaToken) {
		t.Fatalf("bytes before the resumed cursor %d were replayed: %q", mid, string(acc))
	}
	if !strings.Contains(string(acc), s6GammaToken) {
		t.Fatalf("resumed stream never delivered the marker written after the cursor: %q", string(acc))
	}
}

func TestStreamContract_PastEndResetsAndInvalidCursorsAreRejected(t *testing.T) {
	s, settled := s6ActiveWithOutput(t, s6DeltaCmd, s6DeltaToken)

	stale := settled.NextOffset + 4096
	stream := s6Open(t, s.ID, "?offset="+strconv.FormatInt(stale, 10), "", 60*time.Second)
	if opening := stream.next(t); opening.Comment == "" {
		t.Fatalf("first frame = %+v, want the opening comment", opening)
	}
	f, _ := stream.nextEvent(t)
	if f.Event != "reset" {
		t.Fatalf("event=%q want reset for a cursor past the end (data=%s)", f.Event, f.Data)
	}
	var r s6ResetData
	if err := json.Unmarshal(f.Data, &r); err != nil {
		t.Fatalf("decode reset data: %v raw=%s", err, f.Data)
	}
	if f.ID != strconv.FormatInt(r.NextOffset, 10) {
		t.Fatalf("reset event id=%q but data.nextOffset=%d — the reset is only useful if the client can resume from its id", f.ID, r.NextOffset)
	}
	if r.NextOffset >= stale {
		t.Fatalf("reset handed back %d, which is not behind the stale cursor %d", r.NextOffset, stale)
	}
	if r.NextOffset < settled.NextOffset {
		t.Fatalf("reset went back to %d, behind bytes that exist (%d)", r.NextOffset, settled.NextOffset)
	}
	stream.Close()

	for _, cursor := range []string{"-1", "1.5", "abc", "9223372036854775808"} {
		resp, body := do(t, http.MethodGet, "/api/v1/sessions/"+s.ID+"/stream?offset="+cursor, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("stream ?offset=%s: status=%d want 400 body=%s", cursor, resp.StatusCode, body)
		}
		if got := s6ErrorField(t, body); got != s6InvalidInput {
			t.Fatalf("stream ?offset=%s: error=%q want %q", cursor, got, s6InvalidInput)
		}
	}
}

func TestStreamContract_SnapshotIsInvalidStateAndRestoresNothing(t *testing.T) {
	s, _ := s6ActiveWithOutput(t, s6AlphaCmd, s6AlphaToken)

	frozen, ok := snapshotSession(t, s.ID)
	if !ok {
		t.Skip("SUT predates the product snapshot endpoint — the snapshot stream branch is not exercisable here")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot", frozen.State)
	}
	if frozen.Pod != "" {
		t.Fatalf("snapshot session still names pod %q", frozen.Pod)
	}

	resp, body := do(t, http.MethodGet, "/api/v1/sessions/"+s.ID+"/stream", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("stream on a snapshot session: status=%d want 422 body=%s", resp.StatusCode, body)
	}
	if got := s6ErrorField(t, body); got != s6InvalidState {
		t.Fatalf("stream on a snapshot session: error=%q want %q", got, s6InvalidState)
	}

	after := getSession(t, s.ID)
	if after.State != "snapshot" {
		t.Fatalf("state after the refused stream = %q, want snapshot — the stream must not restore", after.State)
	}
	if after.Pod != "" {
		t.Fatalf("the refused stream left pod %q behind", after.Pod)
	}
}

func TestStreamContract_KeepaliveIsNeitherOutputNorActivity(t *testing.T) {
	s, settled := s6ActiveWithOutput(t, s6DeltaCmd, s6DeltaToken)
	before := getSession(t, s.ID)

	stream := s6Open(t, s.ID, "?offset="+strconv.FormatInt(settled.NextOffset, 10), "", 3*s6Heartbeat)
	if opening := stream.next(t); opening.Comment == "" {
		t.Fatalf("first frame = %+v, want the opening comment", opening)
	}
	if second := stream.next(t); second.Comment == "" {
		t.Fatalf("frame after the opening comment = %+v, want a keepalive comment — a settled shell must not produce output events", second)
	}
	stream.Close()

	afterStream := getSession(t, s.ID)
	if afterStream.State != before.State {
		t.Fatalf("state moved %q -> %q across a keepalive", before.State, afterStream.State)
	}
	if afterStream.Pod != before.Pod {
		t.Fatalf("pod moved %q -> %q across a keepalive", before.Pod, afterStream.Pod)
	}
	if afterStream.LastAccess != before.LastAccess {
		t.Fatalf("lastAccess moved %q -> %q across a keepalive; a heartbeat is not activity", before.LastAccess, afterStream.LastAccess)
	}

	r := readShellAt(t, s.ID, 0)
	if r.Path != "active" {
		t.Fatalf("read path after the keepalive = %q, want active", r.Path)
	}
	if r.NextOffset < settled.NextOffset {
		t.Fatalf("read after the keepalive ended at %d, behind the pre-stream record (%d)", r.NextOffset, settled.NextOffset)
	}
	afterRead := getSession(t, s.ID)
	if afterRead.LastAccess == before.LastAccess {
		t.Fatalf("read(0) after the keepalive left lastAccess at %q — the SSE frame is not activity, but the read that follows it is", before.LastAccess)
	}
}
