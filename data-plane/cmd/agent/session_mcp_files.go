// The MCP container's half of AC-F5: an approved response too large to ride the
// tool result is written to the session's shared volume, and the tool answers
// with its path. The workload pod's agent holds the same volume at the same
// path (the control plane injects SESSION_SHARED_DIR only where it mounts it),
// so "the agent reads it as a file" needs nothing carried in the response.
//
// What this file deliberately does not do is name files after anything the
// agent chose. The request id is minted by this server for the approval's
// external identifier, so using it as the filename keeps every path segment out
// of the caller's reach — a URL-derived name would put one in.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// spillSubdir keeps fetched artifacts in one place under the volume, so a
	// later writer of a different kind can take its own subtree without either
	// having to know about the other.
	spillSubdir = "web_fetch_get"
	// spillFileSuffix is deliberately not derived from the response's content
	// type: an extension would be a claim about the bytes this server cannot make.
	spillFileSuffix = ".body"
	// maxSpilledBodyBytes bounds one artifact. It is the same 64 MiB ceiling
	// AC-F6 already puts on a raw upstream response, and two orders below the
	// volume's default size, so a single fetch cannot fill the session's volume.
	maxSpilledBodyBytes = int64(64 << 20)
)

type spilledBody struct {
	path      string
	size      int64
	truncated bool
}

// spillBody takes head separately from rest because callTool has already read
// that prefix to decide the response was large, and an http body does not seek.
func (c sessionMCPConfig) spillBody(requestID string, head []byte, rest io.Reader) (spilledBody, error) {
	if c.sharedDir == "" {
		return spilledBody{}, errors.New("no shared volume is configured for this session")
	}
	dir := filepath.Join(c.sharedDir, spillSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return spilledBody{}, fmt.Errorf("prepare the shared volume directory: %w", err)
	}

	// A temporary file in the same directory, renamed once complete: the agent
	// polling for its file never opens a half-written one, and a failed write
	// leaves no path the tool result could have named.
	tmp, err := os.CreateTemp(dir, "."+requestID+"-*")
	if err != nil {
		return spilledBody{}, fmt.Errorf("create the response file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if committed {
			return
		}
		tmp.Close()
		os.Remove(tmpName)
	}()

	written, err := tmp.Write(head)
	if err != nil {
		return spilledBody{}, fmt.Errorf("write the response: %w", err)
	}
	size := int64(written)
	// One byte past the ceiling again, for the same reason as the inline limit:
	// it separates "exactly at the limit" from "longer than we will store".
	copied, err := io.Copy(tmp, io.LimitReader(rest, maxSpilledBodyBytes-size+1))
	if err != nil {
		return spilledBody{}, fmt.Errorf("write the response: %w", err)
	}
	size += copied
	truncated := false
	if size > maxSpilledBodyBytes {
		if err := tmp.Truncate(maxSpilledBodyBytes); err != nil {
			return spilledBody{}, fmt.Errorf("bound the response file: %w", err)
		}
		size = maxSpilledBodyBytes
		truncated = true
	}
	if err := tmp.Close(); err != nil {
		return spilledBody{}, fmt.Errorf("close the response file: %w", err)
	}

	final := filepath.Join(dir, requestID+spillFileSuffix)
	if err := os.Rename(tmpName, final); err != nil {
		return spilledBody{}, fmt.Errorf("publish the response file: %w", err)
	}
	committed = true
	return spilledBody{path: final, size: size, truncated: truncated}, nil
}
