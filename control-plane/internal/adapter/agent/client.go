// Package agent contains the AgentClient port — the control plane's shell I/O
// channel to a session pod's data plane agent — and its HTTP implementation.
// It is deliberately separate from the k8s.PodOrchestrator port: the
// orchestrator owns the pod *lifecycle* (start/stop/reach), while this client
// moves the shell *payload* (write→stdin AC-D2, read→scrollback delta AC-D3)
// once the session is active.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
)

// Client is the shell I/O port onto a session's data plane agent, addressed by
// pod name (the only pod handle sessions store).
//
// AC mapping:
//   - Write → AC-D2: payload goes verbatim into the shell's stdin (PTY
//     master); returns as soon as the agent accepted it, never waiting for
//     the command to run.
//   - Read  → AC-D3: returns the shell output accumulated after offset plus
//     the nextOffset cursor for the following delta read. offset 0 replays
//     the full history; reads are non-consuming.
type Client interface {
	Write(ctx context.Context, pod, payload string) error
	Read(ctx context.Context, pod string, offset int64) (payload string, nextOffset int64, err error)
}

// HTTPClient is the real Client: it resolves the pod's current IP through the
// Kubernetes API on every call (pod IPs are not stable across restore, and
// sessions store only the pod name) and talks plain HTTP to the agent's
// /write and /read endpoints. It also carries the pod's /checkpoint and /restore
// endpoints for the agent-driven CRIU path (see Checkpoint/Restore below), so
// the criu.AgentCheckpointer reuses this one client and its IP resolution.
type HTTPClient struct {
	client    kubernetes.Interface
	namespace string
	port      int
	http      *http.Client
	// stream is used for /checkpoint and /restore, whose bodies are whole
	// checkpoint archives — bounded by the request context, not the short shell
	// I/O timeout on http.
	stream *http.Client
}

// compile-time assertion that HTTPClient satisfies the shell I/O port.
var _ Client = (*HTTPClient)(nil)

// HTTPOption customises an HTTPClient.
type HTTPOption func(*HTTPClient)

// WithPort overrides the agent port (default k8s.AgentPort). Tests point the
// client at a local httptest agent; production keeps the default.
func WithPort(port int) HTTPOption {
	return func(c *HTTPClient) {
		if port > 0 {
			c.port = port
		}
	}
}

// NewHTTPClient builds a Client that resolves pods in the given namespace.
func NewHTTPClient(client kubernetes.Interface, namespace string, opts ...HTTPOption) *HTTPClient {
	c := &HTTPClient{
		client:    client,
		namespace: namespace,
		port:      k8s.AgentPort,
		http:      &http.Client{Timeout: 30 * time.Second},
		stream:    &http.Client{}, // no overall timeout: archive transfers are ctx-bounded
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Checkpoint drives the agent's /checkpoint: it returns the checkpoint archive
// stream (criu images + scrollback) the agent produced by CRIU-dumping its shell
// tree. The caller (criu.AgentCheckpointer) streams it to durable storage and
// must Close the returned reader.
func (c *HTTPClient) Checkpoint(ctx context.Context, pod string) (io.ReadCloser, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/checkpoint", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checkpoint session pod %s: %w", pod, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("checkpoint session pod %s: agent returned %d: %s", pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// Restore drives the agent's /restore on a restore-target pod: it streams the
// checkpoint archive to the agent, which CRIU-restores the shell tree from it.
func (c *HTTPClient) Restore(ctx context.Context, pod string, archive io.Reader) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/restore", archive)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.stream.Do(req)
	if err != nil {
		return fmt.Errorf("restore into session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("restore into session pod %s: agent returned %d: %s", pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// resolve looks up the pod's current IP. A missing or IP-less pod is an
// internal inconsistency for an activated session (activate proved the shell
// reachable), so it surfaces as a plain error — not a domain not-found.
func (c *HTTPClient) resolve(ctx context.Context, pod string) (string, error) {
	p, err := c.client.CoreV1().Pods(c.namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve session pod %s/%s: %w", c.namespace, pod, err)
	}
	if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
		return "", fmt.Errorf("session pod %s/%s is not running with an IP (phase=%s)", c.namespace, pod, p.Status.Phase)
	}
	return p.Status.PodIP, nil
}

func (c *HTTPClient) baseURL(ip string) string {
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(c.port))
}

// Write injects payload into the session shell's stdin via the agent (AC-D2).
func (c *HTTPClient) Write(ctx context.Context, pod, payload string) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/write", strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("write to session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("write to session pod %s: agent returned %d: %s", pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// Read fetches the shell output delta after offset from the agent (AC-D3).
func (c *HTTPClient) Read(ctx context.Context, pod string, offset int64) (string, int64, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return "", 0, err
	}
	url := fmt.Sprintf("%s/read?offset=%d", c.baseURL(ip), offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("read from session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("read from session pod %s: agent returned %d: %s", pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Payload    string `json:"payload"`
		NextOffset int64  `json:"nextOffset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("decode read response from session pod %s: %w", pod, err)
	}
	return out.Payload, out.NextOffset, nil
}
