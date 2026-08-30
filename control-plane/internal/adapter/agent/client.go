// Package agent contains the control plane's workload I/O and archive client
// for a session pod's data plane agent, plus its HTTP implementation.
// It is deliberately separate from the k8s.PodOrchestrator port: the
// orchestrator owns the pod *lifecycle* (start/stop/reach), while this client
// moves workload input/output (AC-D2/D3, AC-E2/E3) once the session is active.
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
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const checkpointIDHeader = "X-Session-Checkpoint-ID"

// Client is the workload I/O port onto a session's data plane agent, addressed by
// pod name (the only pod handle sessions store).
//
// AC mapping:
//   - Write → AC-D2/AC-E2: shell stdin or an asynchronously accepted prompt.
//   - Read  → AC-D3/AC-E3: output after offset plus nextOffset; offset 0
//     replays the full history and reads are non-consuming.
//   - Stream → the same append-only output as resumable SSE events.
type Client interface {
	Write(ctx context.Context, pod, payload string) error
	Read(ctx context.Context, pod string, offset int64) (payload string, nextOffset int64, err error)
	Stream(ctx context.Context, pod string, offset int64) (io.ReadCloser, error)
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
func (c *HTTPClient) Checkpoint(ctx context.Context, pod string) (io.ReadCloser, string, error) {
	return c.checkpoint(ctx, pod, "")
}

// CheckpointWithGeneration starts an archive checkpoint under a control-plane
// owned generation. Persisting that generation before this call lets a restarted
// control plane abort the exact admission barrier without trusting a stale ID.
func (c *HTTPClient) CheckpointWithGeneration(ctx context.Context, pod, generation string) (io.ReadCloser, string, error) {
	return c.checkpoint(ctx, pod, generation)
}

func (c *HTTPClient) checkpoint(ctx context.Context, pod, generation string) (io.ReadCloser, string, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/checkpoint", nil)
	if err != nil {
		return nil, "", err
	}
	if generation != "" {
		req.Header.Set(checkpointIDHeader, generation)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("checkpoint session pod %s: %w", pod, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", fmt.Errorf("checkpoint session pod %s: agent returned %d: %s", pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, resp.Header.Get(checkpointIDHeader), nil
}

// AbortCheckpoint reopens a claude-code agent after its archive was streamed
// but durable storage or pod reclamation failed. The data plane endpoint is
// idempotent. Shell CRIU callers never invoke this method because a completed
// dump cannot be resumed by this protocol.
func (c *HTTPClient) AbortCheckpoint(ctx context.Context, pod, checkpointID string) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/checkpoint/abort", nil)
	if err != nil {
		return err
	}
	req.Header.Set(checkpointIDHeader, checkpointID)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("abort checkpoint for session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf(
			"abort checkpoint for session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	return nil
}

// Restore streams a workload archive to a restore-target pod. The agent either
// CRIU-restores a shell or unpacks Claude filesystem state.
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

// Write forwards shell stdin or a Claude prompt to the agent (AC-D2/AC-E2).
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
		message := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			return fmt.Errorf("write to session pod %s: %w: %s", pod, session.ErrWorkloadQueueFull, message)
		case http.StatusRequestEntityTooLarge:
			return fmt.Errorf("write to session pod %s: %w: %s", pod, session.ErrWorkloadPromptTooLarge, message)
		case http.StatusInsufficientStorage:
			return fmt.Errorf("write to session pod %s: %w: %s", pod, session.ErrWorkloadOutputFull, message)
		default:
			return fmt.Errorf("write to session pod %s: agent returned %d: %s", pod, resp.StatusCode, message)
		}
	}
	return nil
}

// Read fetches workload output after offset from the agent (AC-D3/AC-E3).
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

// Stream opens the data plane's long-lived SSE output feed at offset. Unlike
// the short Read client, it uses the context-bounded streaming HTTP client so
// an otherwise healthy workspace connection has no 30-second deadline.
func (c *HTTPClient) Stream(ctx context.Context, pod string, offset int64) (io.ReadCloser, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/stream?offset=%d", c.baseURL(ip), offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stream from session pod %s: %w", pod, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		message := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusBadRequest {
			return nil, fmt.Errorf("stream from session pod %s: %w: %s", pod, session.ErrInvalidInput, message)
		}
		return nil, fmt.Errorf("stream from session pod %s: agent returned %d: %s", pod, resp.StatusCode, message)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("stream from session pod %s: unexpected content type %q", pod, contentType)
	}
	return resp.Body, nil
}
