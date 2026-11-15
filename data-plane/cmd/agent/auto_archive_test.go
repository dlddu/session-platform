package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// uploadSink stands in for the object store: it keeps what each PUT carried.
type uploadSink struct {
	mu      sync.Mutex
	bodies  [][]byte
	paths   []string
	length  []int64
	writers []string
}

func newUploadSink(t *testing.T) (*uploadSink, *httptest.Server) {
	t.Helper()
	sink := &uploadSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "PUT only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sink.mu.Lock()
		sink.bodies = append(sink.bodies, body)
		sink.paths = append(sink.paths, r.URL.Path)
		sink.length = append(sink.length, r.ContentLength)
		sink.writers = append(sink.writers, r.Header.Get("X-Amz-Meta-Writer"))
		sink.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return sink, srv
}

func (s *uploadSink) uploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *uploadSink) last() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[len(s.bodies)-1]
}

const autoArchiveTestPath = "/bucket/auto/0123456789abcdef0123456789abcdef/sess-x/checkpoint.tar"

func newTestAutoArchiveRunner(base string, produce autoArchiveProducer) *autoArchiveRunner {
	return &autoArchiveRunner{
		target:  newAutoArchiveTarget(base),
		produce: produce,
		client:  &http.Client{},
		logger:  testLogger(),
		now:     time.Now,
	}
}

func putAutoArchiveTarget(t *testing.T, srv *httptest.Server, url string, expires time.Time) int {
	t.Helper()
	body := `{"url":"` + url + `","expiresAt":"` + expires.UTC().Format(time.RFC3339) +
		`","headers":{"X-Amz-Meta-Writer":"pod:sess-x"}}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+autoArchiveTargetPath, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// AC-B5: a pod takes a URL only for the destination it was created with.
func TestAutoArchiveTargetAcceptsOnlyItsDestination(t *testing.T) {
	now := time.Now()
	base := "https://bucket.s3.example" + autoArchiveTestPath
	target := newAutoArchiveTarget(base)
	for _, tc := range []struct {
		name    string
		url     string
		expires time.Time
		ok      bool
	}{
		{"its own key", base + "?X-Amz-Signature=abc", now.Add(2 * time.Hour), true},
		{"another key", "https://bucket.s3.example/bucket/auto/other/checkpoint.tar?sig=1", now.Add(time.Hour), false},
		{"another host", "https://elsewhere.example" + autoArchiveTestPath + "?sig=1", now.Add(time.Hour), false},
		{"expired", base + "?sig=1", now.Add(-time.Second), false},
		{"not http", "file://" + autoArchiveTestPath, now.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := target.set(tc.url, nil, tc.expires, now); (err == nil) != tc.ok {
				t.Fatalf("set(%q) err = %v, want accepted %v", tc.url, err, tc.ok)
			}
		})
	}
	if err := newAutoArchiveTarget("").set(base, nil, now.Add(time.Hour), now); err != errAutoArchiveNoBase {
		t.Fatalf("set without a base = %v, want errAutoArchiveNoBase", err)
	}
	for _, headers := range []map[string]string{{"Authorization": "x"}, {"X-Amz-Meta-Writer": "a\r\nb"}} {
		if err := target.set(base+"?sig=1", headers, now.Add(time.Hour), now); err != errAutoArchiveHeader {
			t.Fatalf("set with headers %q = %v, want errAutoArchiveHeader", headers, err)
		}
	}
}

// AC-B5: the preStop run archives the shell and uploads it; the archive
// restores like a snapshot's, and the dying shell accepts no more input.
func TestAutoArchiveShellUploadRestores(t *testing.T) {
	sink, store := newUploadSink(t)
	a, src := newShellAgentServer(t)
	writeViaHTTP(t, src, "echo kept > note.txt; echo before-$((1+1))\n", http.StatusOK)
	eventuallyRead(t, src, 0, func(p string) bool { return strings.Contains(p, "before-2") })

	run := newTestAutoArchiveRunner(store.URL+autoArchiveTestPath, a.autoArchiveTo)
	srv := httptest.NewServer(withAutoArchive(routes(testLogger(), a), run))
	t.Cleanup(srv.Close)
	if code := putAutoArchiveTarget(t, srv, store.URL+autoArchiveTestPath+"?sig=1", time.Now().Add(2*time.Hour)); code != http.StatusNoContent {
		t.Fatalf("set target status = %d", code)
	}

	resp, err := http.Get(srv.URL + autoArchiveRunPath)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(result), "uploaded:") || sink.uploads() != 1 {
		t.Fatalf("run = %q with %d uploads, want one upload", result, sink.uploads())
	}
	if sink.length[0] != int64(len(sink.last())) || sink.paths[0] != autoArchiveTestPath || sink.writers[0] != "pod:sess-x" {
		t.Fatalf("upload length=%d path=%q writer=%q for %d bytes", sink.length[0], sink.paths[0], sink.writers[0], len(sink.last()))
	}
	writeViaHTTP(t, srv, "echo after\n", http.StatusConflict)

	dst, restored := newRestoreAgentServer(t)
	if code, body := postRestore(t, restored, sink.last()); code != http.StatusOK {
		t.Fatalf("restore status=%d body=%q", code, body)
	}
	if note, err := os.ReadFile(filepath.Join(dst.homeDir(), "note.txt")); err != nil || string(note) != "kept\n" {
		t.Fatalf("restored note = %q, %v", note, err)
	}
	if full, _ := readViaHTTP(t, restored, 0); !strings.Contains(full, "before-2") {
		t.Fatalf("restored output lost history: %q", full)
	}
}

// A pod without a URL (never armed, or disarmed after a committed snapshot)
// and a restore target before its restore upload nothing.
func TestAutoArchiveSkips(t *testing.T) {
	t.Run("no URL", func(t *testing.T) {
		sink, store := newUploadSink(t)
		a, _ := newShellAgentServer(t)
		run := newTestAutoArchiveRunner(store.URL+autoArchiveTestPath, a.autoArchiveTo)
		if got := run.run(context.Background()); !strings.HasPrefix(got, "skipped") || sink.uploads() != 0 {
			t.Fatalf("run = %q, uploads = %d", got, sink.uploads())
		}
	})
	t.Run("withdrawn URL", func(t *testing.T) {
		sink, store := newUploadSink(t)
		a, _ := newShellAgentServer(t)
		run := newTestAutoArchiveRunner(store.URL+autoArchiveTestPath, a.autoArchiveTo)
		srv := httptest.NewServer(withAutoArchive(routes(testLogger(), a), run))
		t.Cleanup(srv.Close)
		putAutoArchiveTarget(t, srv, store.URL+autoArchiveTestPath+"?sig=1", time.Now().Add(time.Hour))
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+autoArchiveTargetPath, nil)
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("withdraw: %v %v", resp, err)
		}
		if got := run.run(context.Background()); !strings.HasPrefix(got, "skipped") || sink.uploads() != 0 {
			t.Fatalf("run = %q, uploads = %d", got, sink.uploads())
		}
	})
	t.Run("restore target", func(t *testing.T) {
		sink, store := newUploadSink(t)
		a, _ := newRestoreAgentServer(t)
		run := newTestAutoArchiveRunner(store.URL+autoArchiveTestPath, a.autoArchiveTo)
		if err := run.target.set(store.URL+autoArchiveTestPath+"?sig=1", nil, time.Now().Add(time.Hour), time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := run.run(context.Background()); !strings.HasPrefix(got, "skipped") || sink.uploads() != 0 {
			t.Fatalf("run = %q, uploads = %d", got, sink.uploads())
		}
	})
}

// AC-B5: an agent workload's archive does not wait for the running turn. The
// turn is stopped, the queued prompt is dropped with a notice, and the archive
// restores with the stopped turn's record.
func TestClaudeAutoArchiveStopsTheRunningTurn(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)
	writeViaHTTP(t, srv, "first", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	writeViaHTTP(t, srv, "second", http.StatusOK)

	var archive bytes.Buffer
	done := make(chan error, 1)
	go func() {
		skipped, err := c.autoArchiveTo(context.Background(), &archive)
		if skipped {
			err = errAutoArchiveExpired
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("autoArchiveTo: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the automatic archive waited for the running turn")
	}
	if runs, _ := runner.snapshot(); len(runs) != 1 {
		t.Fatalf("runs = %d, want the queued prompt never started", len(runs))
	}
	writeViaHTTP(t, srv, "too late", http.StatusConflict)

	restored, rsrv := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, true, nil)
	resp, err := http.Post(rsrv.URL+"/restore", "application/x-tar", bytes.NewReader(archive.Bytes()))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: %v %v", resp, err)
	}
	resp.Body.Close()
	full, _ := readViaHTTP(t, rsrv, 0)
	if !strings.Contains(full, "status=canceled") || !strings.Contains(full, "1 queued prompt(s) were not run") {
		t.Fatalf("restored output = %q, want the stopped turn and the dropped prompt noted", full)
	}
	if !restored.isReady() {
		t.Fatal("restored workload is not ready")
	}
}

// AC-B5: a gated helper forwards only to its workload pod's destination and
// holds its own termination until an announced upload is through.
func TestAutoArchiveRelay(t *testing.T) {
	sink, store := newUploadSink(t)
	relay := newAutoArchiveRelay(store.URL+autoArchiveTestPath, &http.Client{}, testLogger())
	helper := httptest.NewServer(withAutoArchiveRelay(http.NotFoundHandler(), relay))
	t.Cleanup(helper.Close)

	put := func(target string) int {
		req, _ := http.NewRequest(http.MethodPut, helper.URL+autoArchiveRelayPath, strings.NewReader("tar-bytes"))
		req.Header.Set(autoArchiveURLHeader, target)
		req.Header.Set("X-Amz-Meta-Writer", "pod:sess-x")
		req.Header.Set("Authorization", "not-forwarded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put("https://elsewhere.example/steal?sig=1"); code != http.StatusForbidden || sink.uploads() != 0 {
		t.Fatalf("foreign destination: status %d, uploads %d", code, sink.uploads())
	}
	if code := put(store.URL + autoArchiveTestPath + "?sig=1"); code != http.StatusNoContent || string(sink.last()) != "tar-bytes" || sink.writers[0] != "pod:sess-x" {
		t.Fatalf("own destination: status %d, writer %q", code, sink.writers)
	}

	quiet := autoArchiveQuietWait
	autoArchiveQuietWait = 50 * time.Millisecond
	t.Cleanup(func() { autoArchiveQuietWait = quiet })

	idle := newAutoArchiveRelay(store.URL+autoArchiveTestPath, &http.Client{}, testLogger())
	if got := idle.hold(context.Background()); got != "released: no upload announced" {
		t.Fatalf("hold with nothing announced = %q", got)
	}

	busy := newAutoArchiveRelay(store.URL+autoArchiveTestPath, &http.Client{}, testLogger())
	busy.adjust(1)
	released := make(chan string, 1)
	go func() { released <- busy.hold(context.Background()) }()
	select {
	case got := <-released:
		t.Fatalf("hold released while an upload was pending: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	busy.adjust(-1)
	select {
	case got := <-released:
		if got != "released: relay finished" {
			t.Fatalf("hold = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not release after the upload finished")
	}
}

// A gated workload pod uploads through its helper, which forwards to the store.
func TestAutoArchiveUploadsThroughTheRelay(t *testing.T) {
	sink, store := newUploadSink(t)
	base := store.URL + autoArchiveTestPath
	relay := newAutoArchiveRelay(base, &http.Client{}, testLogger())
	helper := httptest.NewServer(withAutoArchiveRelay(http.NotFoundHandler(), relay))
	t.Cleanup(helper.Close)

	run := newTestAutoArchiveRunner(base, func(_ context.Context, w io.Writer) (bool, error) {
		_, err := io.WriteString(w, "gated-archive")
		return false, err
	})
	run.relay = helper.URL
	if err := run.target.set(base+"?sig=1", map[string]string{"X-Amz-Meta-Writer": "pod:sess-x"}, time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := run.run(context.Background()); !strings.HasPrefix(got, "uploaded:") || string(sink.last()) != "gated-archive" || sink.writers[0] != "pod:sess-x" {
		t.Fatalf("run = %q", got)
	}
	if announced, pending, _ := relay.state(); !announced || pending != 0 {
		t.Fatalf("relay announced=%v pending=%d, want a finished announcement", announced, pending)
	}
}

func TestAutoArchivePreStopUsage(t *testing.T) {
	var stderr bytes.Buffer
	if code := runAutoArchivePreStop([]string{"/elsewhere"}, io.Discard, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2 for an unknown path", code)
	}
}
