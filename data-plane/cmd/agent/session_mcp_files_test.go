package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spilledMCP is newGatedMCP with the two knobs these cases need: a body of a
// chosen size, and a shared volume to put it on.
type spilledMCP struct {
	*gatedMCP
	sharedDir string
	body      string
}

func newSpilledMCP(t *testing.T, sharedDir, body string) *spilledMCP {
	t.Helper()
	s := &spilledMCP{gatedMCP: &gatedMCP{gateway: newFakeGateway(t, "APPROVED")}, sharedDir: sharedDir, body: body}
	gate := s.gateway.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }
	s.server = newSessionMCPServerWith(t, sessionMCPConfig{
		gateway:   gate,
		sharedDir: sharedDir,
		fetch: func(_ context.Context, target string) (*http.Response, error) {
			s.upstream.Add(1)
			s.fetched = append(s.fetched, target)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(s.body)),
			}, nil
		},
	})
	return s
}

func (s *spilledMCP) payloadOf(t *testing.T) map[string]any {
	t.Helper()
	_, isError, text := toolResult(t, s.call(t, "https://artifacts.vendor.example/report"))
	if isError {
		t.Fatalf("approved call returned a tool error: %s", text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool result text is not the JSON payload: %v", err)
	}
	return payload
}

// The line AC-F5 draws: under it nothing changes, so a session that only ever
// fetches small things sees the tool it always had.
func TestSessionMCPKeepsSmallBodiesInline(t *testing.T) {
	s := newSpilledMCP(t, t.TempDir(), strings.Repeat("a", maxInlineBodyBytes))
	payload := s.payloadOf(t)

	if payload["body"] != s.body {
		t.Fatalf("payload body is %d bytes, want the upstream body inline", len(payload["body"].(string)))
	}
	if _, ok := payload["bodyPath"]; ok {
		t.Fatalf("a body at the inline limit was spilled to %v; only larger ones go to the volume", payload["bodyPath"])
	}
	if entries, err := os.ReadDir(s.sharedDir); err != nil || len(entries) != 0 {
		t.Fatalf("shared volume holds %v (err %v), want nothing written for an inline body", entries, err)
	}
}

// The half of AC-F5 this change buys: the bytes go to the volume and the tool
// result names the file instead of carrying it.
func TestSessionMCPSpillsLargeBodiesToTheSharedVolume(t *testing.T) {
	body := strings.Repeat("b", maxInlineBodyBytes+1)
	s := newSpilledMCP(t, t.TempDir(), body)
	payload := s.payloadOf(t)

	if _, ok := payload["body"]; ok {
		t.Fatal("the tool result still carries a body; AC-F5 exists so large results do not make that round trip")
	}
	path, ok := payload["bodyPath"].(string)
	if !ok {
		t.Fatalf("payload = %v, want a bodyPath for a body over the inline limit", payload)
	}
	if got := payload["bodyBytes"]; got != float64(len(body)) {
		t.Fatalf("bodyBytes = %v, want %d", got, len(body))
	}
	if payload["truncated"] != false {
		t.Fatalf("truncated = %v, want false for a body under the file ceiling", payload["truncated"])
	}
	// Under the volume the workload pod holds at the same path, so the path in
	// the result is one the agent can open.
	if !strings.HasPrefix(path, s.sharedDir+string(filepath.Separator)) {
		t.Fatalf("bodyPath = %q, want it under the shared volume %q", path, s.sharedDir)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the spilled body: %v", err)
	}
	if string(stored) != body {
		t.Fatalf("the file holds %d bytes, want the upstream body's %d", len(stored), len(body))
	}
	// A temporary file left behind would be a second, half-written copy of an
	// approved result sitting on the session's volume.
	entries, err := os.ReadDir(filepath.Join(s.sharedDir, spillSubdir))
	if err != nil {
		t.Fatalf("read the spill directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spill directory holds %d entries, want exactly the published file", len(entries))
	}
}

// The deployment that configured no ReadWriteMany class still runs sessions of
// this type, and they behave exactly as they did before AC-F5.
func TestSessionMCPWithoutASharedVolumeTruncatesInline(t *testing.T) {
	body := strings.Repeat("c", maxInlineBodyBytes+500)
	s := newSpilledMCP(t, "", body)
	payload := s.payloadOf(t)

	inline, ok := payload["body"].(string)
	if !ok {
		t.Fatalf("payload = %v, want an inline body when there is no volume to spill to", payload)
	}
	if len(inline) != maxInlineBodyBytes {
		t.Fatalf("inline body = %d bytes, want it cut at %d", len(inline), maxInlineBodyBytes)
	}
	if _, ok := payload["bodyPath"]; ok {
		t.Fatalf("payload names %v with no volume configured", payload["bodyPath"])
	}
	if _, ok := payload["spillError"]; ok {
		t.Fatalf("payload reports a spill error where no spill was attempted: %v", payload["spillError"])
	}
}

// A volume that cannot take the write must not throw away a result a human
// approved — but it must not hide the reason either.
func TestSessionMCPFallsBackInlineWhenTheVolumeRefusesTheWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("d", maxInlineBodyBytes+500)
	s := newSpilledMCP(t, dir, body)
	payload := s.payloadOf(t)

	inline, ok := payload["body"].(string)
	if !ok || len(inline) != maxInlineBodyBytes {
		t.Fatalf("payload = %v, want the pre-AC-F5 truncated body as the fallback", payload)
	}
	if payload["truncated"] != true {
		t.Fatalf("truncated = %v, want true — the agent is holding a cut body", payload["truncated"])
	}
	if _, ok := payload["spillError"].(string); !ok {
		t.Fatalf("payload = %v, want the reason the volume could not take it", payload)
	}
	if _, ok := payload["bodyPath"]; ok {
		t.Fatalf("payload names %v for a write that failed", payload["bodyPath"])
	}
}

// The file ceiling, asserted directly rather than through a 64 MiB fetch.
func TestSpillBodyStopsAtTheFileCeiling(t *testing.T) {
	dir := t.TempDir()
	c := sessionMCPConfig{sharedDir: dir}
	head := []byte("head")
	rest := strings.NewReader(strings.Repeat("e", int(maxSpilledBodyBytes)))

	spill, err := c.spillBody("req-ceiling", head, rest)
	if err != nil {
		t.Fatalf("spill: %v", err)
	}
	if !spill.truncated {
		t.Fatal("a body past the ceiling was not reported truncated")
	}
	if spill.size != maxSpilledBodyBytes {
		t.Fatalf("spilled size = %d, want the ceiling %d", spill.size, maxSpilledBodyBytes)
	}
	info, err := os.Stat(spill.path)
	if err != nil {
		t.Fatalf("stat the spilled file: %v", err)
	}
	if info.Size() != maxSpilledBodyBytes {
		t.Fatalf("file is %d bytes, want the ceiling %d", info.Size(), maxSpilledBodyBytes)
	}
}

// The filename comes from the id this server minted, never from the URL the
// caller chose — the caller must not be able to steer a path segment.
func TestSpillBodyNamesTheFileAfterTheRequestID(t *testing.T) {
	dir := t.TempDir()
	c := sessionMCPConfig{sharedDir: dir}
	spill, err := c.spillBody("req-1a2b3c4d", []byte("payload"), strings.NewReader(""))
	if err != nil {
		t.Fatalf("spill: %v", err)
	}
	want := filepath.Join(dir, spillSubdir, "req-1a2b3c4d"+spillFileSuffix)
	if spill.path != want {
		t.Fatalf("spill path = %q, want %q", spill.path, want)
	}
}
