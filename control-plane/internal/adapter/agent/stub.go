package agent

import (
	"context"
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

// compile-time assertion that StubClient satisfies the port.
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
