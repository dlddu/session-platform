package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialProxyStreamsRedactedSSEBeforeUpstreamEOF(t *testing.T) {
	const token = "token-super-secret"
	const firstFrame = "event: message\ndata: ready\n\n"
	firstUpstreamChunk := firstFrame + "data: token-su"
	secondUpstreamChunk := "per-secret\n\n"
	var releaseOnce sync.Once
	release := make(chan struct{})
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(firstUpstreamChunk)+len(secondUpstreamChunk)))
		w.Header().Set("X-Upstream-Echo", "prefix-"+token)
		_, _ = io.WriteString(w, firstUpstreamChunk)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, secondUpstreamChunk)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, token, &logs, upstream.Client().Transport)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open proxied SSE before upstream EOF: %v", err)
	}
	defer response.Body.Close()
	if response.ContentLength != -1 || response.Header.Get("Content-Length") != "" {
		t.Fatalf("streaming response retained Content-Length: field=%d header=%q", response.ContentLength, response.Header.Get("Content-Length"))
	}
	if got := response.Header.Get("X-Upstream-Echo"); got != "prefix-"+redactedLiteral {
		t.Fatalf("streaming response header = %q, want redacted", got)
	}

	reader := bufio.NewReader(response.Body)
	type readResult struct {
		payload []byte
		err     error
	}
	firstRead := make(chan readResult, 1)
	go func() {
		payload := make([]byte, len(firstFrame))
		_, err := io.ReadFull(reader, payload)
		firstRead <- readResult{payload: payload, err: err}
	}()
	var first readResult
	select {
	case first = <-firstRead:
	case <-time.After(2 * time.Second):
		t.Fatal("safe upstream SSE chunk did not reach the client before EOF")
	}
	if first.err != nil || string(first.payload) != firstFrame {
		t.Fatalf("pre-EOF SSE frame = (%q,%v), want (%q,nil)", first.payload, first.err, firstFrame)
	}

	releaseUpstream()
	rest, readErr := io.ReadAll(reader)
	if readErr != nil {
		t.Fatalf("read completed proxied SSE: %v", readErr)
	}
	body := append(append([]byte(nil), first.payload...), rest...)
	want := firstFrame + "data: " + redactedLiteral + "\n\n"
	if string(body) != want || strings.Contains(string(body), token) || strings.Contains(string(body), "token-su") {
		t.Fatalf("proxied SSE body = %q, want %q without credential fragments", body, want)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("credential leaked in proxy logs: %s", logs.String())
	}
}

func TestCredentialProxyStreamingResponseRedactsTrailersAtEOF(t *testing.T) {
	const token = "trailer-super-secret"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Trailer", "X-Upstream-Echo")
		w.Header().Add("Trailer", "Authorization")
		_, _ = io.WriteString(w, "data: ok\n\n")
		w.Header().Set("X-Upstream-Echo", "prefix-"+token)
		w.Header().Set("Authorization", "Bearer "+token)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, token, &logs, upstream.Client().Transport)
	response, err := http.Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || string(body) != "data: ok\n\n" {
		t.Fatalf("streaming trailer response = (%q,%v)", body, readErr)
	}
	if got := response.Trailer.Get("X-Upstream-Echo"); got != "prefix-"+redactedLiteral {
		t.Fatalf("streaming response trailer = %q, want redacted", got)
	}
	if got := response.Trailer.Get("Authorization"); got != "" {
		t.Fatalf("authentication trailer survived: %q", got)
	}
}

func TestCredentialProxyStreamingBodyCapsRawBytesWithoutFlushingHeldSecret(t *testing.T) {
	const token = "token-super-secret"
	// The sixth byte proves overflow after five already-safe bytes.
	body := newCredentialProxyStreamingBody(
		io.NopCloser(strings.NewReader("abcdeX")),
		token,
		5,
		nil,
	)
	got, err := io.ReadAll(body)
	closeErr := body.Close()
	if !errors.Is(err, errCredentialProxyResponseTooLarge) {
		t.Fatalf("streaming overflow error = %v, want %v", err, errCredentialProxyResponseTooLarge)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if string(got) != "abcde" {
		t.Fatalf("streaming overflow safe prefix = %q, want %q", got, "abcde")
	}

	// A held credential prefix at the raw boundary is never exposed when the
	// one-byte overflow probe terminates the stream.
	body = newCredentialProxyStreamingBody(
		io.NopCloser(strings.NewReader("safe token-suX")),
		token,
		int64(len("safe token-su")),
		nil,
	)
	got, err = io.ReadAll(body)
	_ = body.Close()
	if !errors.Is(err, errCredentialProxyResponseTooLarge) {
		t.Fatalf("split-token overflow error = %v, want %v", err, errCredentialProxyResponseTooLarge)
	}
	if string(got) != "safe " || strings.Contains(string(got), "token-su") {
		t.Fatalf("split-token overflow leaked held suffix: %q", got)
	}
}
