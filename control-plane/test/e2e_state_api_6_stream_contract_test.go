//go:build e2e

// 검증 시나리오: state-api.md#시나리오 6
//
// docs/prd/state-api.md AC-E3 (passive live output stream), asserted on the
// deployed SUT.
//
// 이 파일이 사는 것은 **passive stream 의 상태 계약과 커서 계약**이다. 갈래별로 무엇을
// 단언하는지는 docs/test/e2e.md 의 매핑 행에 있고, 여기 적어 둘 것은 그 행이 담지 못하는
// **경계**다.
//
// claude-code-workload.md#시나리오 4 와의 경계: 그쪽은 **claude-code 세션의 라이브 왕복**
// (UTF-8 경계로 갈린 두 chunk, 중단 후 재연결의 무손실·무중복, raw stream-json 과의 중복
// 부재)이고, 여기는 **상태별 계약과 커서 오류 갈래**다. 커서 계약 중 둘(`Last-Event-ID`
// 우선, past-end reset)을 두 시나리오가 같은 문장으로 기술하지만 실어 나르는 바이트가
// 다르다 — 여기서는 shell 세션의 PTY 바이트를 쓰고, 그래서 UTF-8 경계는 여기서 사지
// 않는다(shell PTY 바이트는 임의라 경계 자체가 계약이 아니다).
//
// "SSE 는 activity 가 아니다"의 반대편, 즉 read 가 `lastAccess` 를 갱신한다는 것은
// state-api.md#시나리오 2 의 파일이 이미 사므로 다시 사지 않는다 — 여기서는 keepalive
// 뒤의 read(0) 이 그 일반 의미를 그대로 갖는다는 대조로만 쓴다.
//
// 범위 밖: 기대 결과의 `idle` 갈래("active/idle 은 기존 pod 에서만 stream 한다"의 idle
// 절반)는 이 SUT 에 idle 진입 트리거가 없어 단언하지 않는다 — docs/test/e2e.md
// §「남은 미검증 분기」에 등재했다(read·write·switch 의 idle 갈래와 같은 선행).
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
	// The wire strings the API answers with (control-plane/internal/session
	// ErrInvalidInput / ErrInvalidState). Written out rather than imported so
	// that rewording either fails this file instead of travelling into it.
	s6InvalidInput = "invalid input"
	s6InvalidState = "session in invalid state for operation"
	// The agent's SSE heartbeat period (data-plane/cmd/agent/output_stream.go
	// outputStreamHeartbeat), copied for the same reason.
	s6Heartbeat = 15 * time.Second
	// Markers are written as arithmetic so the PTY's echo of the command line
	// does not itself contain the token — a `Contains` hit is then output, not
	// the echoed request. The "beta must not be replayed" assertion below
	// depends on that distinction.
	s6AlphaCmd   = "echo s6-alpha-$((20+3))\n"
	s6AlphaToken = "s6-alpha-23"
	s6BetaCmd    = "echo s6-beta-$((30+1))\n"
	s6BetaToken  = "s6-beta-31"
	s6GammaCmd   = "echo s6-gamma-$((40+1))\n"
	s6GammaToken = "s6-gamma-41"
	s6DeltaCmd   = "echo s6-delta-$((50+3))\n"
	s6DeltaToken = "s6-delta-53"
)

// s6Frame is one SSE frame. Comment frames (`: keepalive`) are frames here
// rather than noise to skip, because whether one arrived — and whether an
// output event did not — is itself part of what this scenario buys.
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

// s6Stream is an open passive feed. The shared `client` is deliberately not
// used: its 90s timeout would cut a live stream at a fixed point regardless of
// what the test is waiting for, so each stream carries its own budget instead.
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

// next reads the next frame. A frame ends at a blank line.
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

// nextEvent returns the next non-comment frame and how many comment frames it
// skipped on the way.
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

// s6Output decodes an output event and checks the three things that make its
// cursor usable: the SSE `id` is the same cursor the payload carries, and the
// decoded byte count is exactly how far the cursor moved.
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

// s6Settled polls until two consecutive full reads agree, so that "the stream
// sent nothing more" is measured against a quiet shell rather than one still
// flushing. Kept local rather than in the shared harness for the same reason
// the sibling wire-validation file keeps its own: a scenario file should read
// on its own terms, and the harness carries only what the whole suite needs.
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

// s6ActiveWithOutput brings a fresh session to "active, holding a pod, with a
// settled marker in its output" — the precondition every case below shares.
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

// The active branch: the stream serves the retained pod's existing byte record
// and leaves the session exactly as it found it.
func TestStreamContract_ActiveStreamsPassivelyAndCursorsAreContiguous(t *testing.T) {
	s, settled := s6ActiveWithOutput(t, s6AlphaCmd, s6AlphaToken)

	before := getSession(t, s.ID)
	if before.State != "active" || before.Pod == "" {
		t.Fatalf("precondition: state=%q pod=%q, want an active session holding a pod", before.State, before.Pod)
	}

	stream := s6Open(t, s.ID, "?offset=0", "", 60*time.Second)
	// The agent opens with a comment before any event; reading it is also what
	// proves the frames below were parsed as SSE and not as a JSON body.
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

	// `read` hands its bytes back inside a JSON string, so a non-UTF-8 PTY byte
	// would return as U+FFFD and could not be compared against the stream's raw
	// bytes. A shell running `echo` emits ASCII, so the comparison is exact here;
	// the guard keeps a future non-UTF-8 byte from turning a real contract into a
	// spurious failure rather than silently weakening the assertion.
	if !utf8.Valid(acc) {
		t.Logf("streamed bytes are not valid UTF-8; skipping the byte-for-byte comparison with read, which normalises them")
	} else if !strings.HasPrefix(string(acc), settled.Payload) {
		t.Fatalf("streamed bytes are not the read's bytes:\n stream=%q\n   read=%q", string(acc), settled.Payload)
	}
	if !strings.Contains(string(acc), s6AlphaToken) {
		t.Fatalf("streamed %d bytes without the marker %q", len(acc), s6AlphaToken)
	}
}

// A native EventSource reconnect resends the last accepted id as a header while
// the URL still carries the original query cursor; the header has to win or the
// client re-reads bytes it already has.
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

// The cursor error branches: a cursor past the end is answered with an explicit
// reset rather than a silent wait, and a cursor that is not a non-negative
// integer is rejected before any stream is opened.
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

	// The 200 above is the control that keeps these 400s from being vacuous:
	// the same route on the same session answers a well-formed cursor.
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

// The snapshot branch: unlike read/write/switch, the stream does not restore —
// it refuses, and the frozen session keeps no pod.
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

// The keepalive branch: a heartbeat is neither output nor activity, but it does
// not change what the client's next read means.
func TestStreamContract_KeepaliveIsNeitherOutputNorActivity(t *testing.T) {
	s, settled := s6ActiveWithOutput(t, s6DeltaCmd, s6DeltaToken)
	before := getSession(t, s.ID)

	// Opening at the end of the record leaves the agent with nothing to send, so
	// the only frame it can write next is the heartbeat.
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
