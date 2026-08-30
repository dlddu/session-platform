package main

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

const (
	credentialProxyStreamReadBytes = 32 << 10
	credentialProxyEmptyReadLimit  = 100
)

var (
	errCredentialProxyResponseTooLarge = errors.New("credential proxy upstream response is too large")
	errCredentialProxyResponseRead     = errors.New("credential proxy could not read upstream response")
)

// credentialProxyStreamingBody redacts an identity-encoded upstream SSE body
// without waiting for EOF. The shared redactor retains any suffix that could
// still become the credential on the next Read, so a split token is never
// transiently returned to ReverseProxy. The size limit counts raw upstream
// bytes, matching the buffered non-streaming response path.
type credentialProxyStreamingBody struct {
	source    io.ReadCloser
	redactor  *streamingRedactor
	remaining int64
	onEOF     func()

	readBuffer  []byte
	ready       []byte
	terminalErr error
	closed      bool
}

func newCredentialProxyStreamingBody(source io.ReadCloser, token string, limit int64, onEOF func()) io.ReadCloser {
	return &credentialProxyStreamingBody{
		source:     source,
		redactor:   newStreamingRedactor([]string{token}),
		remaining:  limit,
		onEOF:      onEOF,
		readBuffer: make([]byte, credentialProxyStreamReadBytes),
	}
}

func (b *credentialProxyStreamingBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	emptyReads := 0
	for len(b.ready) == 0 && b.terminalErr == nil {
		readBytes := len(b.readBuffer)
		// Read one byte beyond the remaining raw allowance so an oversized
		// stream is detected without first buffering its entire body.
		if b.remaining < int64(readBytes) {
			readBytes = int(b.remaining) + 1
		}
		n, readErr := b.source.Read(b.readBuffer[:readBytes])
		if n > 0 {
			allowed := n
			emptyReads = 0
			if int64(allowed) > b.remaining {
				allowed = int(b.remaining)
			}
			if allowed > 0 {
				b.ready = append(b.ready, b.redactor.Push(b.readBuffer[:allowed])...)
				b.remaining -= int64(allowed)
			}
			if allowed < n {
				// Never Finish the redactor on an overflow or transport error:
				// its held suffix might be part of the credential. Any already
				// safe prefix is returned before this terminal error.
				b.terminalErr = errCredentialProxyResponseTooLarge
				break
			}
		}

		switch {
		case errors.Is(readErr, io.EOF):
			b.ready = append(b.ready, b.redactor.Finish()...)
			if b.onEOF != nil {
				b.onEOF()
				b.onEOF = nil
			}
			b.terminalErr = io.EOF
		case readErr != nil:
			b.terminalErr = errCredentialProxyResponseRead
		case n == 0:
			emptyReads++
			if emptyReads >= credentialProxyEmptyReadLimit {
				b.terminalErr = io.ErrNoProgress
			}
		}
	}

	if len(b.ready) != 0 {
		n := copy(p, b.ready)
		b.ready = b.ready[n:]
		return n, nil
	}
	return 0, b.terminalErr
}

func (b *credentialProxyStreamingBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.ready = nil
	b.onEOF = nil
	b.redactor.pending = nil
	return b.source.Close()
}

func credentialProxyStreamsResponse(response *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func scrubCredentialProxyResponseMetadata(response *http.Response, token string) {
	for _, name := range credentialProxyAuthHeaders {
		response.Header.Del(name)
		response.Trailer.Del(name)
	}
	redactCredentialProxyHeaders(response.Header, token)
	redactCredentialProxyHeaders(response.Trailer, token)
}
