package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	outputStreamChunkBytes = 64 << 10
	outputStreamHeartbeat  = 15 * time.Second
)

type outputStreamEvent struct {
	Offset        int    `json:"offset"`
	PayloadBase64 string `json:"payloadBase64"`
	NextOffset    int    `json:"nextOffset"`
}

type outputStreamReset struct {
	NextOffset int `json:"nextOffset"`
}

// streamChunk returns one bounded, non-consuming delta and the generation
// channel that closes on the next append. A cursor past the current length is
// explicitly reset rather than left waiting for a buffer that may never grow
// back to that stale value.
func (b *scrollback) streamChunk(offset, limit int) (payload []byte, start, next int, reset bool, changed <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	changed = b.changed
	current := len(b.buf)
	if offset > current {
		return nil, current, current, true, changed
	}
	if offset == current {
		return nil, offset, offset, false, changed
	}
	start = offset
	end := current
	if limit > 0 && end-start > limit {
		end = start + limit
		// Claude output is valid UTF-8. Back off a continuation byte so a
		// stream-issued cursor remains a valid /read cursor as well. Shell PTY
		// bytes may be arbitrary; if no boundary exists in the chunk, retain the
		// byte-bounded fallback rather than stalling that stream forever.
		if safe := validUTF8PrefixAtMost(b.buf[start:], end-start); safe > 0 {
			end = start + safe
		}
	}
	payload = append([]byte(nil), b.buf[start:end]...)
	return payload, start, end, false, changed
}

// serveOutputStream exposes the same append-only byte record as /read over SSE,
// passively (AC-E3). It never cancels the workload when its request context
// ends, which is the one thing the AC does not spell out for the server side.
func serveOutputStream(w http.ResponseWriter, r *http.Request, out *scrollback) {
	offset := 0
	value := r.URL.Query().Get("offset")
	if lastEventID := r.Header.Get("Last-Event-ID"); lastEventID != "" {
		value = lastEventID
	}
	if value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
			return
		}
		offset = parsed
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(outputStreamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		payload, start, next, reset, changed := out.streamChunk(offset, outputStreamChunkBytes)
		if reset {
			if err := writeSSEEvent(w, "reset", next, outputStreamReset{NextOffset: next}); err != nil {
				return
			}
			flusher.Flush()
			offset = next
			continue
		}
		if len(payload) != 0 {
			event := outputStreamEvent{
				Offset:        start,
				PayloadBase64: base64.StdEncoding.EncodeToString(payload),
				NextOffset:    next,
			}
			if err := writeSSEEvent(w, "output", next, event); err != nil {
				return
			}
			flusher.Flush()
			offset = next
			continue
		}

		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w io.Writer, event string, id int, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: ", id, event); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}
