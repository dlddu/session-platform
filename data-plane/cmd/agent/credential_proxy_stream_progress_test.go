package main

import (
	"errors"
	"io"
	"testing"
)

type emptyCredentialProxyBody struct {
	reads int
}

func (b *emptyCredentialProxyBody) Read([]byte) (int, error) {
	b.reads++
	return 0, nil
}

func (*emptyCredentialProxyBody) Close() error { return nil }

func TestCredentialProxyStreamingBodyBoundsNoProgress(t *testing.T) {
	source := &emptyCredentialProxyBody{}
	body := newCredentialProxyStreamingBody(source, "secret", 1024, nil)
	buffer := make([]byte, 1)
	n, err := body.Read(buffer)
	if n != 0 || !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("empty streaming reads = (%d,%v), want (0,%v)", n, err, io.ErrNoProgress)
	}
	if source.reads != credentialProxyEmptyReadLimit {
		t.Fatalf("empty source reads = %d, want bounded %d", source.reads, credentialProxyEmptyReadLimit)
	}
	if closeErr := body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}
