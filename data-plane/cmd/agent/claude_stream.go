package main

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"unicode/utf8"
)

// claudeStreamProjector turns Claude Code's --output-format=stream-json stdout
// into the user-facing text stream retained by the session. Partial text events
// are emitted immediately; the later full assistant/result records are ignored
// so they cannot duplicate already projected text. A non-JSON line is passed
// through for backwards-compatible diagnostics and deterministic fake runners.
type claudeStreamProjector struct {
	dst io.Writer
	buf []byte

	messageHasText bool
	messageNewline bool
	projectedText  bool
}

func newClaudeStreamProjector(dst io.Writer) *claudeStreamProjector {
	return &claudeStreamProjector{dst: dst}
}

func (p *claudeStreamProjector) Write(chunk []byte) (int, error) {
	originalLen := len(chunk)
	p.buf = append(p.buf, chunk...)
	for {
		newline := bytes.IndexByte(p.buf, '\n')
		if newline < 0 {
			break
		}
		line := append([]byte(nil), p.buf[:newline]...)
		p.buf = p.buf[newline+1:]
		if err := p.projectLine(line, true); err != nil {
			return 0, err
		}
	}
	return originalLen, nil
}

func (p *claudeStreamProjector) Close() error {
	if len(p.buf) != 0 {
		line := append([]byte(nil), p.buf...)
		p.buf = nil
		if err := p.projectLine(line, false); err != nil {
			return err
		}
	}
	return p.finishMessage()
}

type claudeStreamEnvelope struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Event   struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
	} `json:"event"`
}

func (p *claudeStreamProjector) projectLine(line []byte, hadNewline bool) error {
	var envelope claudeStreamEnvelope
	if len(line) == 0 || json.Unmarshal(line, &envelope) != nil {
		if _, err := p.dst.Write(line); err != nil {
			return err
		}
		if hadNewline {
			_, err := p.dst.Write([]byte{'\n'})
			return err
		}
		return nil
	}

	if envelope.Type == "stream_event" {
		switch envelope.Event.Type {
		case "content_block_start":
			if envelope.Event.ContentBlock.Type == "text" && envelope.Event.ContentBlock.Text != "" {
				return p.writeText(envelope.Event.ContentBlock.Text)
			}
		case "content_block_delta":
			if envelope.Event.Delta.Type == "text_delta" && envelope.Event.Delta.Text != "" {
				return p.writeText(envelope.Event.Delta.Text)
			}
		case "message_stop":
			return p.finishMessage()
		}
		return nil
	}

	// A result is only a fallback for a CLI that produced no partial text (for
	// example an early structured error). Successful result records following
	// partial events contain the same response and must not be projected again.
	if envelope.Type == "result" && !p.projectedText && envelope.Result != "" {
		if err := p.writeText(envelope.Result); err != nil {
			return err
		}
		return p.finishMessage()
	}
	return nil
}

func (p *claudeStreamProjector) writeText(text string) error {
	if _, err := io.WriteString(p.dst, text); err != nil {
		return err
	}
	p.messageHasText = true
	p.projectedText = true
	p.messageNewline = text[len(text)-1] == '\n'
	return nil
}

func (p *claudeStreamProjector) finishMessage() error {
	if p.messageHasText && !p.messageNewline {
		if _, err := p.dst.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	p.messageHasText = false
	p.messageNewline = false
	return nil
}

// utf8NormalizingWriter makes every downstream Write a complete valid UTF-8
// sequence. It retains an incomplete rune across process Write boundaries and
// replaces malformed bytes, so every cursor the live stream emits is also safe
// to use with the legacy JSON /read response.
type utf8NormalizingWriter struct {
	dst     io.Writer
	pending []byte
}

func newUTF8NormalizingWriter(dst io.Writer) *utf8NormalizingWriter {
	return &utf8NormalizingWriter{dst: dst}
}

func (w *utf8NormalizingWriter) Write(p []byte) (int, error) {
	originalLen := len(p)
	w.pending = append(w.pending, p...)
	out := w.drain(false)
	if len(out) == 0 {
		return originalLen, nil
	}
	_, err := w.dst.Write(out)
	return originalLen, err
}

func (w *utf8NormalizingWriter) Close() error {
	out := w.drain(true)
	if len(out) == 0 {
		return nil
	}
	_, err := w.dst.Write(out)
	return err
}

func (w *utf8NormalizingWriter) drain(final bool) []byte {
	var out []byte
	consumed := 0
	for consumed < len(w.pending) {
		rest := w.pending[consumed:]
		if !utf8.FullRune(rest) && !final {
			break
		}
		r, size := utf8.DecodeRune(rest)
		if r == utf8.RuneError && size == 1 {
			out = utf8.AppendRune(out, utf8.RuneError)
			consumed++
			continue
		}
		out = append(out, rest[:size]...)
		consumed += size
	}
	if consumed != 0 {
		copy(w.pending, w.pending[consumed:])
		w.pending = w.pending[:len(w.pending)-consumed]
	}
	return out
}

// streamingRedactor never releases a suffix that could still grow into one of
// the configured credential literals. This is the streaming equivalent of
// bytes.ReplaceAll, including matches split across any number of Write calls.
type streamingRedactor struct {
	literals [][]byte
	pending  []byte
}

func newStreamingRedactor(literals []string) *streamingRedactor {
	r := &streamingRedactor{literals: make([][]byte, 0, len(literals))}
	for _, literal := range literals {
		if literal != "" {
			r.literals = append(r.literals, []byte(literal))
		}
	}
	return r
}

func (r *streamingRedactor) Push(p []byte) []byte {
	r.pending = append(r.pending, p...)
	return r.drain(false)
}

func (r *streamingRedactor) Finish() []byte {
	return r.drain(true)
}

func (r *streamingRedactor) drain(final bool) []byte {
	var out []byte
	for len(r.pending) != 0 {
		matchAt, matchLen := -1, 0
		for _, literal := range r.literals {
			if at := bytes.Index(r.pending, literal); at >= 0 &&
				(matchAt < 0 || at < matchAt || at == matchAt && len(literal) > matchLen) {
				matchAt, matchLen = at, len(literal)
			}
		}
		if matchAt >= 0 {
			out = append(out, r.pending[:matchAt]...)
			out = append(out, redactedLiteral...)
			r.pending = r.pending[matchAt+matchLen:]
			continue
		}
		if final {
			out = append(out, r.pending...)
			r.pending = nil
			break
		}

		keep := 0
		for _, literal := range r.literals {
			maxPrefix := min(len(literal)-1, len(r.pending))
			for n := maxPrefix; n > keep; n-- {
				if bytes.Equal(r.pending[len(r.pending)-n:], literal[:n]) {
					keep = n
					break
				}
			}
		}
		out = append(out, r.pending[:len(r.pending)-keep]...)
		if keep == 0 {
			r.pending = nil
		} else {
			r.pending = append(r.pending[:0], r.pending[len(r.pending)-keep:]...)
		}
		break
	}
	return out
}

// claudeOutputSink incrementally redacts projected, valid UTF-8 text, applies
// the invocation cap to the stored representation, and appends it to session
// scrollback while the process is still running. Bytes near the invocation cap
// are retained until Close so an overflow can replace only that unexposed tail
// with the existing marker; previously issued cursors are never rewritten.
type claudeOutputSink struct {
	mu sync.Mutex

	out             *scrollback
	redactor        *streamingRedactor
	runLimit        int
	scrollbackLimit int
	onSessionFull   func()

	runSeen     int
	runEmitted  int
	runPending  []byte
	truncated   bool
	closed      bool
	sessionFull bool
}

func newClaudeOutputSink(out *scrollback, redact []string, runLimit, scrollbackLimit int, onSessionFull func()) *claudeOutputSink {
	return &claudeOutputSink{
		out:             out,
		redactor:        newStreamingRedactor(redact),
		runLimit:        runLimit,
		scrollbackLimit: scrollbackLimit,
		onSessionFull:   onSessionFull,
	}
}

func (s *claudeOutputSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	originalLen := len(p)
	if originalLen == 0 || s.closed || s.truncated || s.sessionFull {
		return originalLen, nil
	}
	s.appendBoundedLocked(s.redactor.Push(p))
	return originalLen, nil
}

// appendBoundedLocked accepts only already-redacted valid UTF-8. The byte cap
// is therefore a bound on exactly the projected representation clients retain.
func (s *claudeOutputSink) appendBoundedLocked(p []byte) {
	if len(p) == 0 || s.truncated || s.sessionFull {
		return
	}
	if s.runLimit <= 0 {
		s.appendSession(p)
		return
	}

	s.runSeen += len(p)
	s.runPending = append(s.runPending, p...)
	markerBytes := min(len(claudeRunOutputLimitMarker), s.runLimit)
	dataLimit := s.runLimit - markerBytes
	if canEmit := dataLimit - s.runEmitted; canEmit > 0 {
		canEmit = min(canEmit, len(s.runPending))
		canEmit = validUTF8PrefixAtMost(s.runPending, canEmit)
		if canEmit > 0 {
			s.appendSession(s.runPending[:canEmit])
			s.runEmitted += canEmit
			s.runPending = append(s.runPending[:0], s.runPending[canEmit:]...)
		}
	}
	if s.runSeen > s.runLimit {
		s.runPending = nil
		s.appendSession([]byte(claudeRunOutputLimitMarker[:markerBytes]))
		s.truncated = true
	}
}

func (s *claudeOutputSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if !s.truncated && !s.sessionFull {
		s.appendBoundedLocked(s.redactor.Finish())
	}
	if !s.truncated && len(s.runPending) != 0 {
		s.appendSession(s.runPending)
		s.runPending = nil
	}
	s.closed = true
	return nil
}

func (s *claudeOutputSink) appendSession(p []byte) {
	if len(p) == 0 || s.sessionFull {
		return
	}
	if s.out.appendClaudeBoundedAt(p, s.scrollbackLimit, claudeOutputLimitMarker) {
		s.sessionFull = true
		if s.onSessionFull != nil {
			s.onSessionFull()
		}
	}
}

func validUTF8PrefixAtMost(p []byte, limit int) int {
	if limit >= len(p) {
		return len(p)
	}
	if limit <= 0 {
		return 0
	}
	for limit > 0 && !utf8.RuneStart(p[limit]) {
		limit--
	}
	return limit
}
