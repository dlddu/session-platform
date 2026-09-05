package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// StubClient is an in-memory Client for tests: Write appends the payload to a
// per-pod buffer and Read serves offset deltas from it — the same cursor
// semantics as the real agent's scrollback (AC-D3), minus the shell actually
// running anything.
type StubClient struct {
	mu   sync.Mutex
	bufs map[string][]byte
}

var _ Client = (*StubClient)(nil)

// NewStubClient returns an empty stub.
func NewStubClient() *StubClient {
	return &StubClient{bufs: map[string][]byte{}}
}

func (c *StubClient) Write(_ context.Context, pod, payload string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bufs[pod] = append(c.bufs[pod], payload...)
	return nil
}

func (c *StubClient) Read(_ context.Context, pod string, offset int64) (string, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := c.bufs[pod]
	n := int64(len(buf))
	if offset < 0 || offset >= n {
		return "", n, nil
	}
	return string(buf[offset:]), n, nil
}

func (c *StubClient) Stream(_ context.Context, pod string, offset int64) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := append([]byte(nil), c.bufs[pod]...)
	n := int64(len(buf))
	if offset < 0 {
		return nil, fmt.Errorf("negative stream offset")
	}
	var event bytes.Buffer
	if offset > n {
		body, _ := json.Marshal(struct {
			NextOffset int64 `json:"nextOffset"`
		}{NextOffset: n})
		_, _ = fmt.Fprintf(&event, "id: %d\nevent: reset\ndata: %s\n\n", n, body)
		return io.NopCloser(bytes.NewReader(event.Bytes())), nil
	}
	if offset == n {
		return io.NopCloser(bytes.NewBufferString(": stub stream idle\n\n")), nil
	}
	payload := buf[offset:]
	body, _ := json.Marshal(struct {
		Offset        int64  `json:"offset"`
		PayloadBase64 string `json:"payloadBase64"`
		NextOffset    int64  `json:"nextOffset"`
	}{
		Offset:        offset,
		PayloadBase64: base64.StdEncoding.EncodeToString(payload),
		NextOffset:    n,
	})
	_, _ = fmt.Fprintf(&event, "id: %d\nevent: output\ndata: %s\n\n", n, body)
	return io.NopCloser(bytes.NewReader(event.Bytes())), nil
}
