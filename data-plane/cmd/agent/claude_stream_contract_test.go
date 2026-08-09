package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The production runner seam writes a credential across two independent
// text_delta records. Neither live transport may expose even the retained
// prefix while the redactor is waiting to decide whether it is a full match.
func TestClaudeLiveEndpointsRedactCredentialSplitAcrossRunnerWrites(t *testing.T) {
	const secret = "token-super-secret"
	firstWritten := make(chan struct{})
	allowSecond := make(chan struct{})
	secondWritten := make(chan struct{})
	allowReturn := make(chan struct{})
	returned := make(chan struct{})
	runner := commandRunnerFunc(func(ctx context.Context, _ []string, opts runnerOptions) error {
		defer close(returned)
		_, _ = io.WriteString(opts.Stdout, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"prefix token-su"}}}`+"\n")
		close(firstWritten)
		select {
		case <-allowSecond:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _ = io.WriteString(opts.Stdout, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"per-secret suffix"}}}`+"\n")
		close(secondWritten)
		select {
		case <-allowReturn:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _ = io.WriteString(opts.Stdout, `{"type":"stream_event","event":{"type":"message_stop"}}`+"\n")
		return nil
	})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, []string{secret})

	cancel, response, reader := openOutputStream(t, srv, 0)
	defer cancel()
	defer response.Body.Close()
	writeViaHTTP(t, srv, "redact live output", http.StatusOK)
	select {
	case <-firstWritten:
	case <-time.After(5 * time.Second):
		t.Fatal("first split credential record was not written")
	}
	first, payload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if got, want := string(payload), "prefix "; got != want {
		t.Fatalf("first safe SSE payload = %q, want %q", got, want)
	}
	if got, cursor := readViaHTTP(t, srv, 0); got != "prefix " || cursor != first.NextOffset {
		t.Fatalf("first safe /read = (%q,%d), want (%q,%d)", got, cursor, "prefix ", first.NextOffset)
	}

	close(allowSecond)
	select {
	case <-secondWritten:
	case <-time.After(5 * time.Second):
		t.Fatal("second split credential record was not written")
	}
	second, payload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if got, want := string(payload), redactedLiteral+" suffix"; got != want || strings.Contains(got, secret) {
		t.Fatalf("completed redacted SSE payload = %q, want %q", got, want)
	}
	full, cursor := readViaHTTP(t, srv, 0)
	if want := "prefix " + redactedLiteral + " suffix"; full != want || cursor != second.NextOffset || strings.Contains(full, secret) {
		t.Fatalf("completed redacted /read = (%q,%d), want (%q,%d)", full, cursor, want, second.NextOffset)
	}
	select {
	case <-returned:
		t.Fatal("runner exited before the live split-credential assertions")
	default:
	}

	close(allowReturn)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not return after release")
	}
	waitClaudeIdle(t, c)
}

// A chunk quota may land inside a multibyte code point. The stream backs up to
// the previous boundary, so every Claude event id is also safe for JSON /read.
func TestClaudeOutputStreamChunksEndAtUTF8Boundary(t *testing.T) {
	source := []byte(strings.Repeat("a", outputStreamChunkBytes-1) + "한-tail")
	var output scrollback
	limit := len(source) + len(claudeOutputLimitMarker) + 100
	if full := output.appendClaudeBoundedAt(source, limit, claudeOutputLimitMarker); full {
		t.Fatal("test source unexpectedly filled cumulative output")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOutputStream(w, r, &output)
	}))
	defer srv.Close()

	cancel, response, reader := openOutputStream(t, srv, 0)
	defer cancel()
	defer response.Body.Close()
	first, firstPayload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if first.NextOffset != outputStreamChunkBytes-1 || !utf8.Valid(firstPayload) || !utf8.Valid(source[:first.NextOffset]) {
		t.Fatalf("first UTF-8 event next=%d bytes=%d valid=%t", first.NextOffset, len(firstPayload), utf8.Valid(firstPayload))
	}
	second, secondPayload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if second.Offset != first.NextOffset || second.NextOffset != len(source) || !utf8.Valid(secondPayload) {
		t.Fatalf("second UTF-8 event cursors=(%d,%d) bytes=%d valid=%t", second.Offset, second.NextOffset, len(secondPayload), utf8.Valid(secondPayload))
	}
	if got := append(append([]byte(nil), firstPayload...), secondPayload...); !bytes.Equal(got, source) {
		t.Fatal("UTF-8-safe SSE chunks did not reconstruct the stored output")
	}
}

func TestOutputStreamLastEventIDTakesPrecedence(t *testing.T) {
	var output scrollback
	output.Append([]byte("hello"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOutputStream(w, r, &output)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream?offset=99", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "2")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	event, payload := decodeOutputEvent(t, readSSEEvent(t, reader))
	if event.Offset != 2 || event.NextOffset != 5 || string(payload) != "llo" {
		t.Fatalf("Last-Event-ID output = (%d,%d,%q), want (2,5,%q)", event.Offset, event.NextOffset, payload, "llo")
	}
	cancel()
	response.Body.Close()

	badReq, err := http.NewRequest(http.MethodGet, srv.URL+"/stream?offset=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	badReq.Header.Set("Last-Event-ID", "-1")
	badResponse, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	defer badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid Last-Event-ID status = %d, want 400", badResponse.StatusCode)
	}
}

func TestClaudeCumulativeMarkerNeverSplitsUTF8(t *testing.T) {
	var output scrollback
	limit := len(claudeOutputLimitMarker) + 2
	if full := output.appendClaudeBoundedAt([]byte("한x"), limit, claudeOutputLimitMarker); !full {
		t.Fatal("overflow did not seal cumulative output")
	}
	got, cursor := output.Since(0)
	if !utf8.Valid(got) {
		t.Fatalf("cumulative cap emitted invalid UTF-8: %x", got)
	}
	if string(got) != claudeOutputLimitMarker {
		t.Fatalf("cumulative cap output = %q, want an unsplit terminal marker", got)
	}
	if cursor != len(claudeOutputLimitMarker) {
		t.Fatalf("cumulative cap cursor = %d, want %d", cursor, len(claudeOutputLimitMarker))
	}
}

func TestClaudeRedactorFinishIsInvocationBounded(t *testing.T) {
	const secret = "credential"
	limit := len(claudeRunOutputLimitMarker) + 2
	var output scrollback
	sink := newClaudeOutputSink(&output, []string{secret}, limit, 1<<20, nil)
	input := strings.Repeat("a", limit) + "creden"
	if n, err := io.WriteString(sink, input); err != nil || n != len(input) {
		t.Fatalf("write redactor tail = (%d,%v), want (%d,nil)", n, err, len(input))
	}
	if got, _ := output.Since(0); string(got) != "aa" {
		t.Fatalf("live bounded prefix = %q, want %q", got, "aa")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	got, cursor := output.Since(0)
	if want := "aa" + claudeRunOutputLimitMarker; string(got) != want {
		t.Fatalf("redactor-finish bounded output = %q, want %q", got, want)
	}
	if cursor != limit {
		t.Fatalf("redactor-finish cursor = %d, want %d", cursor, limit)
	}
}
