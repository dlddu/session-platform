package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// callInBackground issues one tools/call and ignores its answer. These tests
// assert on what the *feed* said while the call was blocked on a decision; the
// call's own result has its own tests, and asserting on it from a background
// goroutine is not something a test may do anyway.
func callInBackground(mcpURL, target string) {
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"` +
		webFetchGetTool + `","arguments":{"url":"` + target + `"}}}`
	go func() {
		resp, err := http.Post(mcpURL+sessionMCPPath, "application/json", strings.NewReader(body))
		if err != nil {
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
}

// collectMarkers runs a tailer against a session MCP until want markers have
// arrived or the deadline passes, and returns everything appended.
func collectMarkers(t *testing.T, mcpURL string, want int) string {
	t.Helper()
	var (
		appended  = make(chan string, 32)
		ctx, stop = context.WithCancel(context.Background())
	)
	t.Cleanup(stop)
	tailer := newNoticeTailer(mcpURL, func(s string) { appended <- s }, testLogger())
	tailer.retry = 10 * time.Millisecond
	go tailer.run(ctx)

	var got strings.Builder
	deadline := time.After(5 * time.Second)
	for i := 0; i < want; i++ {
		select {
		case s := <-appended:
			got.WriteString(s)
		case <-deadline:
			t.Fatalf("only %d of %d markers arrived: %q", i, want, got.String())
		}
	}
	return got.String()
}

// The requirement, end to end within the data plane: an approval wait started
// in the helper pod's MCP container shows up as an in-band marker in the
// workload's append-only output byte stream, in the shape AC-F3 names.
func TestApprovalNoticesLandInTheOutputByteStream(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	callInBackground(g.server.URL, "https://rates.vendor.example/v1/latest")

	got := collectMarkers(t, g.server.URL, 2)
	if !strings.Contains(got, "[session-platform: awaiting approval — "+webFetchGetTool+" · sess-abc:"+requestIDPrefix) {
		t.Errorf("output = %q, want the awaiting-approval marker", got)
	}
	if !strings.Contains(got, "[session-platform: approval approved — "+webFetchGetTool+" · sess-abc:") {
		t.Errorf("output = %q, want the decision marker", got)
	}
	// AC-E3's markers are line-delimited, so a marker never fuses with the
	// agent output on either side of it.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "[session-platform: ") || !strings.HasSuffix(line, "]") {
			t.Errorf("marker line = %q, want a whole [session-platform: …] line", line)
		}
	}
}

// Every wait ends in a marker, whichever way it ends. A refusal that left the
// stream silent would be indistinguishable from one still waiting.
func TestApprovalRefusalsAreMarkedToo(t *testing.T) {
	for _, tc := range []struct{ status, want string }{
		{"REJECTED", "[session-platform: approval rejected — "},
		{"EXPIRED", "[session-platform: approval expired — "},
	} {
		t.Run(tc.status, func(t *testing.T) {
			g := newGatedMCP(t, tc.status)
			callInBackground(g.server.URL, "https://rates.vendor.example/v1/latest")
			got := collectMarkers(t, g.server.URL, 2)
			if !strings.Contains(got, tc.want) {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// A notice this agent does not understand renders as nothing rather than as a
// malformed marker: a helper pod one version ahead must not be able to write
// garbage into a byte stream whose offsets have already been handed out.
func TestUnknownNoticeKindsRenderNothing(t *testing.T) {
	if got := renderApprovalNotice(approvalNotice{Kind: "something-newer"}); got != "" {
		t.Fatalf("rendered %q, want nothing", got)
	}
}

// The marker goes through the bounded append, so AC-E3's cumulative limit and
// its terminal marker hold: once the stream has been closed by the bound,
// a platform notice cannot reopen it or push past it.
func TestApprovalNoticesRespectTheCumulativeOutputLimit(t *testing.T) {
	limit := len(claudeOutputLimitMarker) + 32
	var out scrollback
	if full := out.appendClaudeBoundedAt([]byte(strings.Repeat("x", 64)), limit, claudeOutputLimitMarker); !full {
		t.Fatal("the buffer did not reach its bound")
	}
	closed, _ := out.Since(0)
	if !strings.HasSuffix(string(closed), claudeOutputLimitMarker) {
		t.Fatalf("buffer = %q, want it closed by the terminal marker", closed)
	}

	marker := renderApprovalNotice(approvalNotice{
		Kind: noticeAwaiting, Tool: webFetchGetTool, ExternalID: "sess-abc:req-1",
	})
	out.appendClaudeBoundedAt([]byte(marker), limit, claudeOutputLimitMarker)

	after, _ := out.Since(0)
	if string(after) != string(closed) {
		t.Fatalf("a notice was appended past the cumulative limit: %q", after)
	}
}

// Offsets already issued stay valid: a marker only ever appends.
func TestApprovalNoticesOnlyAppend(t *testing.T) {
	var out scrollback
	out.Append([]byte("assistant output"))
	before, cursor := out.Since(0)

	out.appendClaudeBoundedAt(
		[]byte(renderApprovalNotice(approvalNotice{Kind: noticeAwaiting, Tool: webFetchGetTool, ExternalID: "s:r"})),
		1<<20, claudeOutputLimitMarker)

	delta, next := out.Since(cursor)
	if next <= cursor {
		t.Fatalf("nextOffset %d did not advance past %d", next, cursor)
	}
	if !strings.HasPrefix(string(delta), "\n[session-platform: awaiting approval") {
		t.Fatalf("delta = %q, want only the marker", delta)
	}
	whole, _ := out.Since(0)
	if !strings.HasPrefix(string(whole), string(before)) {
		t.Fatal("the bytes before the marker were rewritten")
	}
}

// The tailer must not be able to take the workload down. An unreachable feed
// costs the session its markers and nothing else.
func TestNoticeTailerSurvivesAnUnreachableFeed(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	tailer := newNoticeTailer("http://127.0.0.1:1", func(string) {
		t.Error("an unreachable feed produced a marker")
	}, testLogger())
	tailer.retry = time.Millisecond

	done := make(chan struct{})
	go func() { tailer.run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tailer did not stop with its context")
	}
}
