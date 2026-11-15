package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// AC-B5: when its pod is terminated, the agent archives the session itself and
// uploads it straight to storage through a URL the control plane signed for
// this pod alone, so the archive does not depend on the control plane being up.
const (
	autoArchiveTargetPath   = "/auto-archive/target"
	autoArchiveRunPath      = "/auto-archive/run"
	autoArchiveHoldPath     = "/auto-archive/hold"
	autoArchiveAnnouncePath = "/auto-archive/announce"
	autoArchiveRelayPath    = "/auto-archive/relay"

	autoArchiveURLBaseEnv   = "AUTO_ARCHIVE_URL_BASE"
	autoArchiveURLHeader    = "X-Auto-Archive-URL"
	autoArchivePreStopCmd   = "auto-archive-prestop"
	maxAutoArchiveTargetLen = 8 << 10
)

// autoArchiveBudget bounds archiving plus upload inside the pod's 110-second
// termination grace period.
var autoArchiveBudget = 100 * time.Second

// autoArchiveQuietWait is how long a helper pod's hold waits for its workload
// pod to announce an upload before letting the helper go.
var autoArchiveQuietWait = 15 * time.Second

// autoArchiveSettleWait bounds how long a running turn may take to stop before
// the archive is taken anyway.
var autoArchiveSettleWait = 15 * time.Second

var (
	errAutoArchiveNoBase      = errors.New("this pod takes no automatic archive URL")
	errAutoArchiveDestination = errors.New("the URL does not point at this pod's automatic archive")
	errAutoArchiveExpired     = errors.New("the URL has already expired")
	errAutoArchiveHeader      = errors.New("only x-amz-meta-* headers can accompany the URL")
)

// autoArchiveTarget holds the URL the control plane pushed, checked against
// the one destination this pod was created with.
type autoArchiveTarget struct {
	base string

	mu      sync.Mutex
	url     string
	headers http.Header
	expires time.Time
}

func newAutoArchiveTarget(base string) *autoArchiveTarget {
	return &autoArchiveTarget{base: strings.TrimSpace(base)}
}

func (t *autoArchiveTarget) set(raw string, headers map[string]string, expires, now time.Time) error {
	if t.base == "" {
		return errAutoArchiveNoBase
	}
	if !expires.After(now) {
		return errAutoArchiveExpired
	}
	if base, err := urlWithoutQuery(raw); err != nil || base != t.base {
		return errAutoArchiveDestination
	}
	signed, err := metadataHeaders(headers)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.url, t.headers, t.expires = raw, signed, expires
	return nil
}

// metadataHeaders admits only the object metadata the URL's signature covers.
func metadataHeaders(in map[string]string) (http.Header, error) {
	out := http.Header{}
	for name, value := range in {
		if !strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") || strings.ContainsAny(value, "\r\n") {
			return nil, errAutoArchiveHeader
		}
		out.Set(name, value)
	}
	return out, nil
}

func copyMetadataHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") && len(values) > 0 {
			dst.Set(name, values[0])
		}
	}
}

func (t *autoArchiveTarget) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.url, t.headers, t.expires = "", nil, time.Time{}
}

func (t *autoArchiveTarget) current(now time.Time) (string, http.Header, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.url == "" || !t.expires.After(now) {
		return "", nil, false
	}
	return t.url, t.headers.Clone(), true
}

func (t *autoArchiveTarget) matches(raw string) bool {
	base, err := urlWithoutQuery(raw)
	return err == nil && t.base != "" && base == t.base
}

func urlWithoutQuery(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	return u.String(), nil
}

// autoArchiveProducer writes the session's archive to w. skipped means there
// is no session state worth archiving (a restore target before its restore).
type autoArchiveProducer func(ctx context.Context, w io.Writer) (skipped bool, err error)

type autoArchiveRunner struct {
	target  *autoArchiveTarget
	produce autoArchiveProducer
	// relay is the helper pod that uploads for a workload pod whose egress
	// reaches only that helper (gated types); empty uploads directly.
	relay  string
	client *http.Client
	logger *slog.Logger
	now    func() time.Time
}

// withAutoArchive serves the target and run routes in front of a workload
// agent's own routes.
func withAutoArchive(next http.Handler, run *autoArchiveRunner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT "+autoArchiveTargetPath, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL       string            `json:"url"`
			ExpiresAt time.Time         `json:"expiresAt"`
			Headers   map[string]string `json:"headers"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxAutoArchiveTargetLen)).Decode(&body); err != nil {
			http.Error(w, "decode automatic archive target: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := run.target.set(body.URL, body.Headers, body.ExpiresAt, run.now()); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errAutoArchiveNoBase) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE "+autoArchiveTargetPath, func(w http.ResponseWriter, _ *http.Request) {
		run.target.clear()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+autoArchiveRunPath, func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "the automatic archive runs from the pod's own preStop hook", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), autoArchiveBudget)
		defer cancel()
		_, _ = io.WriteString(w, run.run(ctx)+"\n")
	})
	mux.Handle("/", next)
	return mux
}

// run archives and uploads once, reporting what happened in one line. Any
// failure leaves the pod to terminate without an archive.
func (r *autoArchiveRunner) run(ctx context.Context) string {
	target, headers, ok := r.target.current(r.now())
	if !ok {
		r.logger.Info("automatic archive skipped: no valid upload URL")
		return "skipped: no valid upload URL"
	}
	if r.relay != "" {
		if err := r.announce(ctx, http.MethodPost); err != nil {
			r.logger.Warn("automatic archive relay not announced", "err", err)
		}
		defer func() {
			if err := r.announce(context.WithoutCancel(ctx), http.MethodDelete); err != nil {
				r.logger.Warn("automatic archive relay not released", "err", err)
			}
		}()
	}
	spool, err := os.CreateTemp("", "auto-archive-*.tar")
	if err != nil {
		return r.fail("spool", err)
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	skipped, err := r.produce(ctx, spool)
	if err != nil {
		return r.fail("archive", err)
	}
	if skipped {
		r.logger.Info("automatic archive skipped: no session state to archive")
		return "skipped: no session state"
	}
	size, err := spool.Seek(0, io.SeekEnd)
	if err != nil {
		return r.fail("measure", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return r.fail("rewind", err)
	}
	if err := r.upload(ctx, target, headers, spool, size); err != nil {
		return r.fail("upload", err)
	}
	r.logger.Info("automatic archive uploaded", "bytes", size, "relay", r.relay != "")
	return fmt.Sprintf("uploaded: %d bytes", size)
}

func (r *autoArchiveRunner) fail(step string, err error) string {
	r.logger.Error("automatic archive failed", "step", step, "err", err)
	return "failed: " + step + ": " + err.Error()
}

func (r *autoArchiveRunner) upload(ctx context.Context, target string, headers http.Header, body io.Reader, size int64) error {
	dest := target
	if r.relay != "" {
		dest = strings.TrimRight(r.relay, "/") + autoArchiveRelayPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, dest, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/x-tar")
	copyMetadataHeaders(req.Header, headers)
	if r.relay != "" {
		req.Header.Set(autoArchiveURLHeader, target)
	}
	return doUpload(r.client, req)
}

func (r *autoArchiveRunner) announce(ctx context.Context, method string) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.relay, "/")+autoArchiveAnnouncePath, nil)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("helper answered %d", resp.StatusCode)
	}
	return nil
}

func doUpload(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// autoArchiveRelay is a gated session's helper side: it uploads for its
// workload pod and holds its own termination until that upload is through.
type autoArchiveRelay struct {
	target *autoArchiveTarget
	client *http.Client
	logger *slog.Logger

	mu        sync.Mutex
	announced bool
	pending   int
	changed   chan struct{}
}

func newAutoArchiveRelay(base string, client *http.Client, logger *slog.Logger) *autoArchiveRelay {
	return &autoArchiveRelay{target: newAutoArchiveTarget(base), client: client, logger: logger, changed: make(chan struct{})}
}

func (r *autoArchiveRelay) adjust(delta int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.announced = true
	r.pending += delta
	if r.pending < 0 {
		r.pending = 0
	}
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *autoArchiveRelay) state() (announced bool, pending int, changed <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.announced, r.pending, r.changed
}

// hold returns once an announced upload is through, or when none is announced
// within autoArchiveQuietWait, or at ctx's end.
func (r *autoArchiveRelay) hold(ctx context.Context) string {
	quiet := time.NewTimer(autoArchiveQuietWait)
	defer quiet.Stop()
	for {
		announced, pending, changed := r.state()
		if announced && pending == 0 {
			return "released: relay finished"
		}
		select {
		case <-ctx.Done():
			return "released: time is up"
		case <-quiet.C:
			if !announced {
				return "released: no upload announced"
			}
		case <-changed:
		}
	}
}

func withAutoArchiveRelay(next http.Handler, relay *autoArchiveRelay) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+autoArchiveAnnouncePath, func(w http.ResponseWriter, _ *http.Request) {
		relay.adjust(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE "+autoArchiveAnnouncePath, func(w http.ResponseWriter, _ *http.Request) {
		relay.adjust(-1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT "+autoArchiveRelayPath, func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get(autoArchiveURLHeader)
		if !relay.target.matches(target) {
			http.Error(w, errAutoArchiveDestination.Error(), http.StatusForbidden)
			return
		}
		if r.ContentLength < 0 {
			http.Error(w, "the archive must arrive with its length", http.StatusLengthRequired)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), autoArchiveBudget)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.ContentLength = r.ContentLength
		req.Header.Set("Content-Type", "application/x-tar")
		copyMetadataHeaders(req.Header, r.Header)
		if err := doUpload(relay.client, req); err != nil {
			relay.logger.Error("automatic archive relay failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		relay.logger.Info("automatic archive relayed", "bytes", r.ContentLength)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+autoArchiveHoldPath, func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "the hold runs from the pod's own preStop hook", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), autoArchiveBudget)
		defer cancel()
		_, _ = io.WriteString(w, relay.hold(ctx)+"\n")
	})
	mux.Handle("/", next)
	return mux
}

// runAutoArchivePreStop is the preStop hook: it calls path on this container's
// own agent and always succeeds, so a failed archive never delays termination
// past what the agent already spent.
func runAutoArchivePreStop(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != autoArchiveRunPath && args[0] != autoArchiveHoldPath) {
		fmt.Fprintln(stderr, "usage: agent "+autoArchivePreStopCmd+" "+autoArchiveRunPath+"|"+autoArchiveHoldPath)
		return 2
	}
	_, port, err := net.SplitHostPort(env("DATA_PLANE_AGENT_ADDR", defaultAddr))
	if err != nil {
		fmt.Fprintln(stderr, autoArchivePreStopCmd+":", err)
		return 0
	}
	client := &http.Client{Timeout: autoArchiveBudget + 5*time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + args[0])
	if err != nil {
		fmt.Fprintln(stderr, autoArchivePreStopCmd+":", err)
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(stdout, io.LimitReader(resp.Body, 4096))
	return 0
}
