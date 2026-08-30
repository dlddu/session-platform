package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type commandRunnerFunc func(context.Context, []string, runnerOptions) error

func (f commandRunnerFunc) Run(ctx context.Context, argv []string, opts runnerOptions) error {
	return f(ctx, argv, opts)
}

type parsedSSEEvent struct {
	name string
	id   int
	data []byte
}

func openOutputStream(t *testing.T, srv *httptest.Server, offset int) (context.CancelFunc, *http.Response, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/stream?offset=%d", srv.URL, offset), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("GET /stream status = %d, want 200: %s", resp.StatusCode, body)
	}
	return cancel, resp, bufio.NewReader(resp.Body)
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) parsedSSEEvent {
	t.Helper()
	for {
		var event parsedSSEEvent
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read SSE event: %v", err)
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				break
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				id, err := strconv.Atoi(strings.TrimPrefix(line, "id: "))
				if err != nil {
					t.Fatalf("parse SSE id %q: %v", line, err)
				}
				event.id = id
			case strings.HasPrefix(line, "event: "):
				event.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.data = append(event.data, strings.TrimPrefix(line, "data: ")...)
			}
		}
		if event.name != "" {
			return event
		}
	}
}

func decodeOutputEvent(t *testing.T, event parsedSSEEvent) (outputStreamEvent, []byte) {
	t.Helper()
	if event.name != "output" {
		t.Fatalf("SSE event = %q, want output", event.name)
	}
	var output outputStreamEvent
	if err := json.Unmarshal(event.data, &output); err != nil {
		t.Fatalf("decode output event: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(output.PayloadBase64)
	if err != nil {
		t.Fatalf("decode output payload: %v", err)
	}
	if event.id != output.NextOffset {
		t.Fatalf("SSE id = %d, want nextOffset %d", event.id, output.NextOffset)
	}
	if output.NextOffset-output.Offset != len(payload) {
		t.Fatalf("cursor delta = %d, payload bytes = %d", output.NextOffset-output.Offset, len(payload))
	}
	return output, payload
}

// A text_delta must reach both SSE and the legacy cursor read while Claude is
// still running. Closing the passive SSE request must not cancel that run, and
// reconnecting from its event id must return exactly the later delta.
func TestClaudeStreamsPartialOutputBeforeExitAndReconnects(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	runner := commandRunnerFunc(func(ctx context.Context, _ []string, opts runnerOptions) error {
		defer close(returned)
		close(started)
		_, _ = io.WriteString(opts.Stdout, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}}`+"\n")
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _ = io.WriteString(opts.Stdout, `{"type":"stream_event","event":{"type":"message_stop"}}`+"\n")
		return nil
	})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	cancel, response, reader := openOutputStream(t, srv, 0)
	writeViaHTTP(t, srv, "stream this", http.StatusOK)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Claude invocation did not start")
	}

	first, payload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if got, want := string(payload), "partial"; got != want {
		t.Fatalf("live SSE payload = %q, want %q", got, want)
	}
	if first.Offset != 0 || first.NextOffset != len("partial") {
		t.Fatalf("live SSE cursors = (%d,%d), want (0,%d)", first.Offset, first.NextOffset, len("partial"))
	}
	if got, cursor := readViaHTTP(t, srv, 0); got != "partial" || cursor != first.NextOffset {
		t.Fatalf("pre-exit /read = (%q,%d), want (%q,%d)", got, cursor, "partial", first.NextOffset)
	}

	cancel()
	response.Body.Close()
	select {
	case <-returned:
		t.Fatal("closing the passive output stream canceled the Claude invocation")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Claude invocation did not return after release")
	}
	waitClaudeIdle(t, c)

	cancel, response, reader = openOutputStream(t, srv, first.NextOffset)
	defer cancel()
	defer response.Body.Close()
	second, payload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if got, want := string(payload), "\n"; got != want {
		t.Fatalf("reconnected SSE payload = %q, want %q", got, want)
	}
	if second.Offset != first.NextOffset || second.NextOffset != first.NextOffset+1 {
		t.Fatalf("reconnected SSE cursors = (%d,%d), want (%d,%d)", second.Offset, second.NextOffset, first.NextOffset, first.NextOffset+1)
	}
	if got, cursor := readViaHTTP(t, srv, 0); got != "partial\n" || cursor != second.NextOffset {
		t.Fatalf("final /read = (%q,%d), want (%q,%d)", got, cursor, "partial\n", second.NextOffset)
	}
}

func TestClaudeStreamingRedactionNeverExposesSplitCredential(t *testing.T) {
	const secret = "token-super-secret"
	var output scrollback
	sink := newClaudeOutputSink(&output, []string{secret}, 1<<20, 1<<20, nil)

	_, _ = io.WriteString(sink, "prefix token-su")
	if got, _ := output.Since(0); string(got) != "prefix " {
		t.Fatalf("first live output = %q, want only safe prefix", got)
	}
	_, _ = io.WriteString(sink, "per-secret suffix")
	if got, _ := output.Since(0); strings.Contains(string(got), secret) || string(got) != "prefix "+redactedLiteral+" suffix" {
		t.Fatalf("redacted live output = %q", got)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeStreamingOutputHoldsSplitUTF8Rune(t *testing.T) {
	var output scrollback
	sink := newClaudeOutputSink(&output, nil, 1<<20, 1<<20, nil)
	writer := newUTF8NormalizingWriter(sink)
	runeBytes := []byte("한")

	if _, err := writer.Write(runeBytes[:1]); err != nil {
		t.Fatal(err)
	}
	if got, cursor := output.Since(0); len(got) != 0 || cursor != 0 {
		t.Fatalf("incomplete rune became visible as (%q,%d)", got, cursor)
	}
	if _, err := writer.Write(runeBytes[1:]); err != nil {
		t.Fatal(err)
	}
	if got, cursor := output.Since(0); string(got) != "한" || cursor != len(runeBytes) {
		t.Fatalf("completed rune = (%q,%d), want (%q,%d)", got, cursor, "한", len(runeBytes))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeProjectedInvocationOutputCapAppliesAfterRedaction(t *testing.T) {
	limit := len(claudeRunOutputLimitMarker) + 7
	var output scrollback
	sink := newClaudeOutputSink(&output, []string{"x"}, limit, 1<<20, nil)
	for _, chunk := range []string{"x", strings.Repeat("x", limit)} {
		if n, err := io.WriteString(sink, chunk); err != nil || n != len(chunk) {
			t.Fatalf("write %q = (%d,%v), want (%d,nil)", chunk, n, err, len(chunk))
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	got, cursor := output.Since(0)
	if want := redactedLiteral[:7] + claudeRunOutputLimitMarker; string(got) != want {
		t.Fatalf("bounded projected output = %q, want %q", got, want)
	}
	if cursor != limit {
		t.Fatalf("bounded projected output cursor = %d, want %d", cursor, limit)
	}
}

func TestOutputStreamResetsStaleCursor(t *testing.T) {
	var output scrollback
	output.Append([]byte("hello"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOutputStream(w, r, &output)
	}))
	defer srv.Close()

	cancel, response, reader := openOutputStream(t, srv, 99)
	defer cancel()
	defer response.Body.Close()
	event := readSSEEvent(t, reader)
	if event.name != "reset" || event.id != len("hello") {
		t.Fatalf("stale-cursor SSE event = (%q,%d), want (reset,%d)", event.name, event.id, len("hello"))
	}
	var reset outputStreamReset
	if err := json.Unmarshal(event.data, &reset); err != nil {
		t.Fatal(err)
	}
	if reset.NextOffset != len("hello") {
		t.Fatalf("reset nextOffset = %d, want %d", reset.NextOffset, len("hello"))
	}
}

func TestClaudeStreamProjectorDoesNotDuplicateFinalResult(t *testing.T) {
	var output strings.Builder
	projector := newClaudeStreamProjector(&output)
	_, _ = io.WriteString(projector, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`+"\n")
	_, _ = io.WriteString(projector, `{"type":"stream_event","event":{"type":"message_stop"}}`+"\n")
	_, _ = io.WriteString(projector, `{"type":"result","result":"hello"}`+"\n")
	if err := projector.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "hello\n"; got != want {
		t.Fatalf("projected output = %q, want %q", got, want)
	}
}
