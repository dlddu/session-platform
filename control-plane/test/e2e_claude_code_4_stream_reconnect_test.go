//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 4
//
// 이 파일이 사는 것, 이웃 시나리오와의 경계, 그리고 헤드라인을 사기 위해 대역을 먼저 넓힌
// 경위는 docs/test/e2e.md 의 매핑 행과 저작 슬라이스 노트가 갖는다.
package e2e_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Sizes are written out rather than computed so that a change to either side
// fails here instead of quietly agreeing with itself.
const (
	cc4AlphaPrompt = "e2e-reply:alpha:25000:12"
	cc4AlphaPrefix = "alpha:"
	cc4AlphaRunes  = 25000
	cc4AlphaBytes  = 6 + 3*cc4AlphaRunes

	// Their byte counts matter as much as the big one's: a threshold that is merely
	// *reachable* would let a wait return while the reply is still arriving.
	cc4SmallRunes = 40

	cc4BetaPrompt = "e2e-reply:beta:40:4"
	cc4BetaPrefix = "beta:"
	cc4BetaBytes  = 5 + 3*cc4SmallRunes

	cc4GammaPrompt = "e2e-reply:gamma:40:4"
	cc4GammaPrefix = "gamma:"
	cc4GammaBytes  = 6 + 3*cc4SmallRunes

	cc4Filler = "가"
)

// The agent's stream chunk limit (data-plane/cmd/agent/output_stream.go
// outputStreamChunkBytes), copied rather than imported so that changing it there
// fails this file instead of travelling into it silently.
const cc4ChunkLimit = 64 << 10

// Where the first chunk has to end; the derivation is in the mapping row.
const cc4FirstChunkEnd = 65535

// At most one per invocation; data-plane/cmd/agent/claude_stream.go finishMessage owns the rule.
const cc4MessageTerminator = 1

func cc4AlphaReply() []byte {
	return []byte(cc4AlphaPrefix + strings.Repeat(cc4Filler, cc4AlphaRunes))
}

// The settle loop is not belt and braces: the message-terminating newline lands
// after the last delta, so a wait that stops at the byte threshold can hand back a
// buffer that is still moving — and every assertion here compares a fixed buffer
// against a live stream, so a moving one turns a contract into a race.
func cc4Settled(t *testing.T, id, prompt string, atLeast int) readResp {
	t.Helper()
	writePromptOK(t, id, prompt)
	r := eventuallyClaudeOutput(t, id, 5*time.Minute, func(p string) bool {
		return len(p) >= atLeast
	})
	deadline := time.Now().Add(time.Minute)
	for {
		time.Sleep(2 * time.Second)
		again := readShellAt(t, id, 0)
		if again.NextOffset == r.NextOffset {
			return again
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s output never stopped growing (%d -> %d)", id, r.NextOffset, again.NextOffset)
		}
		r = again
	}
}

func TestClaudeStream_ChunkCutsBackOffToACodePointBoundary(t *testing.T) {
	s := claudeSession(t)

	full := cc4Settled(t, s.ID, cc4AlphaPrompt, cc4AlphaBytes)
	buf := []byte(full.Payload)

	// Bytes, not a substring search: that is what rules out a re-encoded body upstream.
	if want := cc4AlphaReply(); !bytes.Equal(buf[:cc4AlphaBytes], want) {
		t.Fatalf("first %d bytes of the buffer are not the alpha reply (got prefix %q)",
			cc4AlphaBytes, string(buf[:min(64, len(buf))]))
	}
	if got := strings.Count(full.Payload, cc4AlphaPrefix); got != 1 {
		t.Fatalf("alpha reply projected %d times, want exactly 1 — a result record must not "+
			"replay text already emitted as partial deltas", got)
	}
	if extra := int(full.NextOffset) - cc4AlphaBytes; extra < 0 || extra > cc4MessageTerminator {
		t.Fatalf("buffer is %d bytes, want %d plus at most %d terminating byte",
			full.NextOffset, cc4AlphaBytes, cc4MessageTerminator)
	}

	if buf[cc4FirstChunkEnd]&0xC0 == 0x80 {
		t.Fatalf("byte %d should begin a rune; the boundary assertion below would be meaningless",
			cc4FirstChunkEnd)
	}
	if buf[cc4ChunkLimit]&0xC0 != 0x80 {
		t.Fatalf("byte %d should be a continuation byte — with this body the %d-byte limit must "+
			"fall mid-rune, otherwise this test proves nothing about the back-off",
			cc4ChunkLimit, cc4ChunkLimit)
	}

	st := s6Open(t, s.ID, "?offset=0", "", time.Minute)
	first, _ := st.nextEvent(t)
	d1, p1 := s6Output(t, first)
	if d1.Offset != 0 || d1.NextOffset != cc4FirstChunkEnd {
		t.Fatalf("first chunk covered [%d,%d), want [0,%d) — the %d-byte limit must be pulled back "+
			"to the last complete rune", d1.Offset, d1.NextOffset, cc4FirstChunkEnd, cc4ChunkLimit)
	}
	if !utf8.Valid(p1) {
		t.Fatal("first chunk is not valid UTF-8; the stream issued a cursor that /read cannot use")
	}

	second, _ := st.nextEvent(t)
	d2, p2 := s6Output(t, second)
	if d2.Offset != cc4FirstChunkEnd {
		t.Fatalf("second chunk starts at %d, want %d — chunks must be contiguous", d2.Offset, cc4FirstChunkEnd)
	}
	if !utf8.Valid(p2) {
		t.Fatal("second chunk is not valid UTF-8")
	}
	got := append(append([]byte(nil), p1...), p2...)
	if len(got) > len(buf) {
		t.Fatalf("the stream delivered %d bytes but the settled buffer holds %d", len(got), len(buf))
	}
	if !bytes.Equal(got, buf[:len(got)]) {
		t.Fatal("the two chunks concatenated do not reproduce the buffer they came from")
	}
	t.Logf("alpha reply %d bytes delivered as [0,%d)+[%d,%d)", full.NextOffset,
		d1.NextOffset, d2.Offset, d2.NextOffset)
}

func TestClaudeStream_LastEventIDBeatsQueryAndResumesWithoutReplay(t *testing.T) {
	s := claudeSession(t)
	cc4Settled(t, s.ID, cc4AlphaPrompt, cc4AlphaBytes)

	opening := s6Open(t, s.ID, "?offset=0", "", time.Minute)
	first, _ := opening.nextEvent(t)
	d1, _ := s6Output(t, first)
	opening.Close()
	if d1.NextOffset != cc4FirstChunkEnd {
		t.Fatalf("first chunk ended at %d, want %d", d1.NextOffset, cc4FirstChunkEnd)
	}

	resumed := s6Open(t, s.ID, "?offset=0", strconv.FormatInt(d1.NextOffset, 10), 5*time.Minute)
	next, _ := resumed.nextEvent(t)
	d2, _ := s6Output(t, next)
	if d2.Offset != d1.NextOffset {
		t.Fatalf("resumed at %d, want %d — Last-Event-ID must beat the query cursor",
			d2.Offset, d1.NextOffset)
	}

	writePromptOK(t, s.ID, cc4BetaPrompt)
	seen := bytes.Buffer{}
	seen.Write(mustPayload(t, next))
	deadline := time.Now().Add(5 * time.Minute)
	for !strings.Contains(seen.String(), cc4BetaPrefix) {
		if time.Now().After(deadline) {
			t.Fatalf("beta reply never reached the resumed stream; %d bytes seen", seen.Len())
		}
		f, _ := resumed.nextEvent(t)
		seen.Write(mustPayload(t, f))
	}
	if strings.Contains(seen.String(), cc4AlphaPrefix) {
		t.Fatal("the resumed stream replayed the alpha reply's opening bytes — a resume must not " +
			"re-send what the client acknowledged")
	}
	if got := strings.Count(seen.String(), cc4BetaPrefix); got != 1 {
		t.Fatalf("beta reply appeared %d times on the resumed stream, want exactly 1", got)
	}
}

func mustPayload(t *testing.T, f s6Frame) []byte {
	t.Helper()
	_, payload := s6Output(t, f)
	return payload
}

func TestClaudeStream_PastEndResetsWithoutTouchingLastAccess(t *testing.T) {
	s := claudeSession(t)
	full := cc4Settled(t, s.ID, cc4GammaPrompt, cc4GammaBytes)

	before := getSession(t, s.ID)
	st := s6Open(t, s.ID, "?offset="+strconv.FormatInt(full.NextOffset+1000, 10), "", time.Minute)
	f, _ := st.nextEvent(t)
	if f.Event != "reset" {
		t.Fatalf("past-end cursor answered with event=%q, want reset (data=%s)", f.Event, f.Data)
	}
	if strings.Contains(string(f.Data), "payloadBase64") {
		t.Fatalf("reset carried a payload: %s", f.Data)
	}
	if f.ID != strconv.FormatInt(full.NextOffset, 10) {
		t.Fatalf("reset issued id=%q, want the current length %d", f.ID, full.NextOffset)
	}
	st.Close()

	if after := getSession(t, s.ID); after.LastAccess != before.LastAccess {
		t.Fatalf("reset moved lastAccess %s -> %s; the signal itself is not activity",
			before.LastAccess, after.LastAccess)
	}
}

func TestClaudeStream_ReadZeroReturnsFullHistoryAndLastCursorIsEmpty(t *testing.T) {
	s := claudeSession(t)
	cc4Settled(t, s.ID, cc4GammaPrompt, cc4GammaBytes)
	full := cc4Settled(t, s.ID, cc4BetaPrompt, cc4GammaBytes+cc4BetaBytes)

	gamma := strings.Index(full.Payload, cc4GammaPrefix)
	beta := strings.Index(full.Payload, cc4BetaPrefix)
	if gamma < 0 || beta < 0 {
		t.Fatalf("read(0) is missing a reply (gamma=%d beta=%d) payload=%q", gamma, beta, full.Payload)
	}
	if gamma > beta {
		t.Fatalf("replies are out of order in the buffer: gamma at %d, beta at %d — read(0) "+
			"accumulates in the order the prompts were written", gamma, beta)
	}

	tail := readShellAt(t, s.ID, full.NextOffset)
	if tail.Payload != "" {
		t.Fatalf("read at the last cursor returned %d bytes, want an empty delta", len(tail.Payload))
	}
	if tail.NextOffset != full.NextOffset {
		t.Fatalf("empty delta moved the cursor %d -> %d", full.NextOffset, tail.NextOffset)
	}
}
