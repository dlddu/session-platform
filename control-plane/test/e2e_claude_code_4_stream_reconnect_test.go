//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 4
//
// docs/prd/claude-code-workload.md AC-E3, driven against the in-cluster provider
// stand-in registered as `CLAUDE-PROVIDER` in docs/test/e2e.md.
//
// The scenario's headline is a pair of SSE output chunks split on a UTF-8
// boundary, and until this PR the SUT could not produce one: the stand-in
// answered every prompt with the same 32 ASCII bytes, so the buffer never
// reached the 64 KiB chunk limit and — since every byte of ASCII is a
// code-point boundary — a cut that ignored runes would have looked identical to
// one that respected them. The stand-in now shapes its reply from a directive
// in the prompt, which is what lets the assertions below be about the platform
// rather than about the fixture.
//
// What this file deliberately does NOT assert, and why:
//
//   - that the reply's *meaning* follows the prompt. The stand-in reflects the
//     directive's label and size, nothing more; conversational continuity is
//     claude-code-workload.md#시나리오 5, which stays on the exception list for
//     exactly that reason.
//   - the 16 MiB per-invocation truncation marker: still far above what a
//     directive is allowed to ask for (MAX_RUNES caps the body well under it).
package e2e_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The directives this file sends to the stand-in, and the replies they produce.
// Sizes are written out rather than computed so that a change to either side
// fails here instead of quietly agreeing with itself.
const (
	cc4AlphaPrompt = "e2e-reply:alpha:25000:12"
	cc4AlphaPrefix = "alpha:"
	cc4AlphaRunes  = 25000
	// len("alpha:") + 3 bytes per rune.
	cc4AlphaBytes = 6 + 3*cc4AlphaRunes

	cc4BetaPrompt = "e2e-reply:beta:40:4"
	cc4BetaPrefix = "beta:"

	cc4GammaPrompt = "e2e-reply:gamma:40:4"
	cc4GammaPrefix = "gamma:"

	// The filler rune the stand-in repeats (U+AC00), three bytes wide.
	cc4Filler = "가"
)

// The agent's stream chunk limit (data-plane/cmd/agent/output_stream.go
// outputStreamChunkBytes), copied rather than imported so that changing it there
// fails this file instead of travelling into it silently.
const cc4ChunkLimit = 64 << 10

// Where the first chunk of the alpha reply has to end.
//
// Runes start at byte 6, 9, 12, … so a rune begins at 65535 (65535-6 is
// divisible by 3) and byte 65536 is one byte into it. A chunker that took the
// limit literally would hand out 65536 and split that rune; scrollback.streamChunk
// backs off to the last complete rune instead, which is this number. With an
// ASCII body both would be 65536 and the assertion would buy nothing.
const cc4FirstChunkEnd = 65535

// The projector terminates a message with a newline when the assistant text does
// not already end in one (data-plane/cmd/agent/claude_stream.go finishMessage),
// so an invocation contributes its reply plus at most that one byte.
const cc4MessageTerminator = 1

func cc4AlphaReply() []byte {
	return []byte(cc4AlphaPrefix + strings.Repeat(cc4Filler, cc4AlphaRunes))
}

// cc4Settled writes one prompt and waits for its whole reply to land, returning
// the full buffer. It waits on a byte count rather than on the marker alone
// because the point of the big reply is its length.
func cc4Settled(t *testing.T, id, prompt string, atLeast int) readResp {
	t.Helper()
	writePromptOK(t, id, prompt)
	return eventuallyClaudeOutput(t, id, 5*time.Minute, func(p string) bool {
		return len(p) >= atLeast
	})
}

// The whole scenario in one place: a reply large enough to be chunked, cut at a
// boundary the platform had to choose, and projected exactly once.
func TestClaudeStream_ChunkCutsBackOffToACodePointBoundary(t *testing.T) {
	s := claudeSession(t)

	full := cc4Settled(t, s.ID, cc4AlphaPrompt, cc4AlphaBytes)
	buf := []byte(full.Payload)

	// The reply arrived intact. Comparing the bytes — not a substring search —
	// is what rules out a truncated or re-encoded body upstream.
	if want := cc4AlphaReply(); !bytes.Equal(buf[:cc4AlphaBytes], want) {
		t.Fatalf("first %d bytes of the buffer are not the alpha reply (got prefix %q)",
			cc4AlphaBytes, string(buf[:min(64, len(buf))]))
	}
	// One invocation, one projection. A `result` record replaying the same text
	// would double the buffer, so the count is the assertion the scenario's last
	// clause asks for: raw stream-json final/result does not duplicate the delta.
	if got := strings.Count(full.Payload, cc4AlphaPrefix); got != 1 {
		t.Fatalf("alpha reply projected %d times, want exactly 1 — a result record must not "+
			"replay text already emitted as partial deltas", got)
	}
	if extra := int(full.NextOffset) - cc4AlphaBytes; extra < 0 || extra > cc4MessageTerminator {
		t.Fatalf("buffer is %d bytes, want %d plus at most %d terminating byte",
			full.NextOffset, cc4AlphaBytes, cc4MessageTerminator)
	}

	// Non-vacuity, proved from the bytes themselves: the limit really does land
	// inside a character here, so backing off was a decision and not a no-op.
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
	if got := append(append([]byte(nil), p1...), p2...); !bytes.Equal(got, buf[:len(got)]) {
		t.Fatal("the two chunks concatenated do not reproduce the buffer they came from")
	}
	t.Logf("alpha reply %d bytes delivered as [0,%d)+[%d,%d)", full.NextOffset,
		d1.NextOffset, d2.Offset, d2.NextOffset)
}

// A reconnecting client resumes from the id it last saw, even when the query
// string says otherwise — and the bytes it already has are not sent again.
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

	// The query asks for the very beginning; the header asks to continue. The
	// header has to win, or a reconnect would replay everything already drawn.
	resumed := s6Open(t, s.ID, "?offset=0", strconv.FormatInt(d1.NextOffset, 10), 5*time.Minute)
	next, _ := resumed.nextEvent(t)
	d2, _ := s6Output(t, next)
	if d2.Offset != d1.NextOffset {
		t.Fatalf("resumed at %d, want %d — Last-Event-ID must beat the query cursor",
			d2.Offset, d1.NextOffset)
	}

	// Everything the resumed stream delivers from here on, including a second
	// prompt's reply, must be new bytes only.
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

// mustPayload decodes an output frame, failing on a reset — the caller here is
// only ever positioned inside the buffer.
func mustPayload(t *testing.T, f s6Frame) []byte {
	t.Helper()
	_, payload := s6Output(t, f)
	return payload
}

// A cursor past the end is answered with a signal, not data — and answering it
// is not access.
func TestClaudeStream_PastEndResetsWithoutTouchingLastAccess(t *testing.T) {
	s := claudeSession(t)
	full := cc4Settled(t, s.ID, cc4GammaPrompt, len(cc4GammaPrefix))

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

// read(0) is the recovery path the SPA takes when its decoder is no longer
// trustworthy: it must return the whole history in order, and the cursor it
// hands back must then be empty.
func TestClaudeStream_ReadZeroReturnsFullHistoryAndLastCursorIsEmpty(t *testing.T) {
	s := claudeSession(t)
	cc4Settled(t, s.ID, cc4GammaPrompt, len(cc4GammaPrefix))
	full := cc4Settled(t, s.ID, cc4BetaPrompt, len(cc4GammaPrefix)+len(cc4BetaPrefix))

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
