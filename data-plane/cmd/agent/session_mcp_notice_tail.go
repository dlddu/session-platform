// The workload-pod half of AC-F3's "대기 표시". The helper pod's MCP knows when
// a call is waiting on a human and what external identifier it is waiting
// under; this is the loop that fetches those notices and writes them into the
// session's append-only output byte stream, where AC-F3 says they belong.
//
// They go in as in-band markers in the same shape the platform already uses for
// its other interjections (`[session-platform: …]`, AC-E3's truncation and
// terminal markers). That is the whole point of the requirement: no new SSE
// event type and no new field, so the cursor contract — `id=nextOffset`,
// `{offset,payloadBase64,nextOffset}`, UTF-8 boundaries, reset recovery — stays
// identical across workload types (AC-C2/AC-E3).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// noticeTailRetryInterval is how long the tailer waits after a failed poll.
	// A failure here is a helper pod restarting or a request cut short; the
	// gate is unaffected either way, so this backs off rather than escalating.
	noticeTailRetryInterval = 3 * time.Second
	// noticeTailRequestTimeout must outlast one server-side long poll window,
	// or every poll would look like a failure.
	noticeTailRequestTimeout = noticeFeedPollTimeout + 10*time.Second
	// maxNoticeFeedResponseBytes bounds one answer. The bodies are a handful of
	// small objects; anything larger is not this endpoint.
	maxNoticeFeedResponseBytes = 1 << 20
)

// noticeSink is the workload half of the feed. One poll yields two things the
// workload needs — the marker text for AC-E3's byte stream, and the wait state
// behind it for AC-F3's idle exception — and they arrive together, so they are
// handed over together.
type noticeSink interface {
	appendPlatformNotice(text string)
	observeApprovalNotices(notices []approvalNotice, dropped int)
}

// noticeTailer follows one session MCP's notice feed. It owns no session state:
// its cursor restarts at zero whenever the process does, which is correct,
// because a new agent process means a new helper pod with a feed that also
// starts empty (AC-F4).
type noticeTailer struct {
	baseURL string
	client  *http.Client
	// sink receives what one poll found. It is the workload in production,
	// injected so a test can read what would have been recorded.
	sink   noticeSink
	logger *slog.Logger
	retry  time.Duration
}

func newNoticeTailer(baseURL string, sink noticeSink, logger *slog.Logger) *noticeTailer {
	return &noticeTailer{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: noticeTailRequestTimeout},
		sink:    sink,
		logger:  logger,
		retry:   noticeTailRetryInterval,
	}
}

// run tails until ctx ends. It never returns an error to its caller: an
// approval-gated session whose notice feed is unreachable still gates every
// call — it only loses the marker — so this must not be able to take the
// workload down with it.
func (t *noticeTailer) run(ctx context.Context) {
	after := 0
	for ctx.Err() == nil {
		body, err := t.poll(ctx, after)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			t.logger.Info("approval notice feed is unavailable; retrying", "err", err)
			if !sleepUntil(ctx, t.retry) {
				return
			}
			continue
		}
		// State before markers. Both come from this one answer, and of the two
		// only the state can cost a pod: a wait recorded a moment late is a
		// freeze the control plane should not have performed, while a marker
		// written a moment late is only a marker written a moment late.
		t.sink.observeApprovalNotices(body.Notices, body.Dropped)
		if body.Dropped > 0 {
			// Say so rather than leave a silent hole: the byte stream is the
			// record a client reads, and an unexplained gap in it is worse
			// than an admitted one.
			t.sink.appendPlatformNotice(fmt.Sprintf("\n[session-platform: %d approval notices were dropped]\n", body.Dropped))
		}
		for _, notice := range body.Notices {
			if rendered := renderApprovalNotice(notice); rendered != "" {
				t.sink.appendPlatformNotice(rendered)
			}
		}
		if body.NextSeq > after {
			after = body.NextSeq
		}
	}
}

func (t *noticeTailer) poll(ctx context.Context, after int) (noticeFeedResponse, error) {
	url := t.baseURL + noticesPath + "?after=" + strconv.Itoa(after)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return noticeFeedResponse{}, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return noticeFeedResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return noticeFeedResponse{}, fmt.Errorf("notice feed returned %d", resp.StatusCode)
	}
	var body noticeFeedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxNoticeFeedResponseBytes)).Decode(&body); err != nil {
		return noticeFeedResponse{}, fmt.Errorf("notice feed body: %w", err)
	}
	return body, nil
}

// renderApprovalNotice is the marker text AC-F3 names. An unknown kind renders
// as nothing rather than as a malformed marker: a newer helper pod publishing a
// notice this agent does not know about must not corrupt the byte stream.
func renderApprovalNotice(n approvalNotice) string {
	switch n.Kind {
	case noticeAwaiting:
		return fmt.Sprintf("\n[session-platform: awaiting approval — %s · %s]\n", n.Tool, n.ExternalID)
	case noticeDecided:
		return fmt.Sprintf("\n[session-platform: approval %s — %s · %s]\n",
			strings.ToLower(n.Decision), n.Tool, n.ExternalID)
	case noticeUnavailable:
		return fmt.Sprintf("\n[session-platform: approval unavailable — %s · %s]\n", n.Tool, n.ExternalID)
	default:
		return ""
	}
}

// sleepUntil reports whether the wait completed rather than being cancelled.
func sleepUntil(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
