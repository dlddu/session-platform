// Package agent contains the control plane's workload I/O and archive client
// for a session pod's data plane agent, plus its HTTP implementation.
// It is deliberately separate from the k8s.PodOrchestrator port: the
// orchestrator owns the pod *lifecycle* (start/stop/reach), while this client
// moves workload input/output (AC-D2/D3, AC-E2/E3) once the session is active.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
//   - RefreshPlugins → AC-J7: the agent fetches its platform plugins again.
type Client interface {
	Write(ctx context.Context, pod, payload string) error
	Read(ctx context.Context, pod string, offset int64) (payload string, nextOffset int64, err error)
	Stream(ctx context.Context, pod string, offset int64) (io.ReadCloser, error)
	RefreshPlugins(ctx context.Context, pod string) (session.PluginChanges, error)
}

// pluginRefreshTimeout bounds one refresh: the sidecar's clone timeout plus
// the agent's sync.
const pluginRefreshTimeout = 3 * time.Minute

// HTTPClient is the real Client: it resolves the pod's current IP through the
// Kubernetes API on every call (pod IPs are not stable across restore, and
// sessions store only the pod name) and talks plain HTTP to the agent.
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

var _ Client = (*HTTPClient)(nil)

// HTTPOption customises an HTTPClient.
type HTTPOption func(*HTTPClient)

// WithPort overrides the agent port (tests point it at a local httptest agent).
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

// CheckpointWithGeneration starts an archive checkpoint under a control-plane
// owned generation. Persisting that generation before this call lets a restarted
// control plane abort the exact admission barrier without trusting a stale ID.
// See archive.AgentCheckpointClient.
func (c *HTTPClient) CheckpointWithGeneration(ctx context.Context, pod, generation string) (io.ReadCloser, string, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/checkpoint", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set(checkpointIDHeader, generation)
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

// AbortCheckpoint reopens a session agent after its archive was streamed but
// durable storage or pod reclamation failed. The data plane endpoint is
// idempotent.
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

// Restore streams a workload archive to a restore-target pod, whose agent
// unpacks the filesystem state for its workload type.
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
	return c.write(ctx, pod, payload, nil)
}

// write is one /write call. The body is the raw payload either way; query
// carries what the caller chose for the turn it queues.
func (c *HTTPClient) write(ctx context.Context, pod, payload string, query url.Values) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	target := c.baseURL(ip) + "/write"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(payload))
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

// RefreshPlugins asks the agent to fetch its platform plugins again (AC-J7).
func (c *HTTPClient) RefreshPlugins(ctx context.Context, pod string) (session.PluginChanges, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return session.PluginChanges{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, pluginRefreshTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/plugins/refresh", nil)
	if err != nil {
		return session.PluginChanges{}, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return session.PluginChanges{}, fmt.Errorf("refresh plugins in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusConflict:
			return session.PluginChanges{}, fmt.Errorf("refresh plugins in session pod %s: %w: %s", pod, session.ErrWorkloadBusy, message)
		case http.StatusBadGateway:
			return session.PluginChanges{}, fmt.Errorf("refresh plugins in session pod %s: %w: %s", pod, session.ErrPluginRefreshFailed, message)
		case http.StatusNotFound:
			return session.PluginChanges{}, fmt.Errorf("refresh plugins in session pod %s: %w: %s", pod, session.ErrInvalidState, message)
		default:
			return session.PluginChanges{}, fmt.Errorf("refresh plugins in session pod %s: agent returned %d: %s", pod, resp.StatusCode, message)
		}
	}
	var changes session.PluginChanges
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&changes); err != nil {
		return session.PluginChanges{}, fmt.Errorf("refresh plugins in session pod %s: decode: %w", pod, err)
	}
	return changes, nil
}

// ToolApprovalsParam is the agent's /write query parameter for a turn whose
// tool calls wait for the session owner's decision (AC-E2). Keep in sync with
// data-plane/cmd/agent (claudeToolApprovalsParam).
const ToolApprovalsParam = "toolApprovals"

// WriteWithToolApprovals queues one prompt whose turn puts every tool call the
// platform policy allows to the session owner (AC-E2). Like PendingQuestions
// it is an optional capability, not part of the Client interface.
func (c *HTTPClient) WriteWithToolApprovals(ctx context.Context, pod, payload string) error {
	return c.write(ctx, pod, payload, url.Values{ToolApprovalsParam: {"true"}})
}

// PendingToolApprovals lists the tool calls a claude-code turn put to the
// session owner, with the epoch of the run that put them up (AC-E2).
func (c *HTTPClient) PendingToolApprovals(ctx context.Context, pod string) (session.ToolApprovalPage, error) {
	page := session.ToolApprovalPage{Approvals: []session.PendingToolApproval{}}
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return page, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+"/approvals", nil)
	if err != nil {
		return page, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return page, fmt.Errorf("list tool approvals in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return page, fmt.Errorf("list tool approvals in session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&page); err != nil {
		return session.ToolApprovalPage{Approvals: []session.PendingToolApproval{}},
			fmt.Errorf("list tool approvals in session pod %s: decode: %w", pod, err)
	}
	if page.Approvals == nil {
		page.Approvals = []session.PendingToolApproval{}
	}
	return page, nil
}

// LedgerTasks reads one keyset page of the chat slot's task ledger (AC-N6).
// The page comes from the session database the pod's sidecar serves, so routing
// by pod is what makes it the slot's current ledger. The request carries only a
// limit and a cursor — there is no SQL on this path — and it is a GET, so
// reading a page changes nothing in the session.
func (c *HTTPClient) LedgerTasks(ctx context.Context, pod string, limit int, afterID string) (session.LedgerTaskPage, error) {
	page := session.LedgerTaskPage{Tasks: []session.LedgerTaskRecord{}}
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return page, err
	}
	query := url.Values{}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if afterID != "" {
		query.Set("after_id", afterID)
	}
	target := c.baseURL(ip) + "/ledger/tasks"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return page, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return page, fmt.Errorf("read task ledger in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusNotImplemented:
			return page, fmt.Errorf("%w: %s", session.ErrLedgerUnsupported, message)
		case http.StatusBadRequest:
			return page, fmt.Errorf("%w: %s", session.ErrInvalidInput, message)
		default:
			return page, fmt.Errorf("read task ledger in session pod %s: agent returned %d: %s",
				pod, resp.StatusCode, message)
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&page); err != nil {
		return session.LedgerTaskPage{Tasks: []session.LedgerTaskRecord{}},
			fmt.Errorf("read task ledger in session pod %s: decode: %w", pod, err)
	}
	if page.Tasks == nil {
		page.Tasks = []session.LedgerTaskRecord{}
	}
	return page, nil
}

// DecideToolApproval relays the owner's decision to the turn waiting on it
// (AC-E2). The agent binds the decision to the call it names, so a refusal
// here is a decision that does not match what the turn is waiting on.
func (c *HTTPClient) DecideToolApproval(ctx context.Context, pod, approvalID string, decision session.ToolApprovalDecision) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	body, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	target := c.baseURL(ip) + "/approvals/" + url.PathEscape(approvalID) + "/decide"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("decide tool approval in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	message := strings.TrimSpace(string(detail))
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", session.ErrInvalidInput, message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", session.ErrToolApprovalStale, message)
	case http.StatusForbidden, http.StatusNotFound:
		return fmt.Errorf("%w: %s", session.ErrToolApprovalNotFound, message)
	default:
		return fmt.Errorf("decide tool approval in session pod %s: agent returned %d: %s", pod, resp.StatusCode, message)
	}
}

// Steer hands a message to the session's running turn (AC-J9). It is an
// optional capability, not part of the Client interface: only the Codex
// workloads' agent takes it. The answer distinguishes a message the turn
// took (applied) from one that never reached it, so the caller can queue a
// rejected message behind the turn instead of calling it delivered.
func (c *HTTPClient) Steer(ctx context.Context, pod, text string) (session.Steered, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return session.Steered{}, err
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return session.Steered{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/steer", strings.NewReader(string(body)))
	if err != nil {
		return session.Steered{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return session.Steered{}, fmt.Errorf("steer session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out session.Steered
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(raw, &out); err != nil {
			return session.Steered{}, fmt.Errorf("steer session pod %s: decode: %w", pod, err)
		}
		return out, nil
	case http.StatusConflict:
		// Either no turn is running or codex refused the message. Both are
		// rejections the caller acts on, not transport errors.
		if json.Unmarshal(raw, &out) == nil && out.Status != "" {
			return out, nil
		}
		return session.Steered{Status: "rejected", Reason: strings.TrimSpace(string(raw))}, nil
	case http.StatusBadRequest:
		return session.Steered{}, fmt.Errorf("%w: %s", session.ErrInvalidInput, strings.TrimSpace(string(raw)))
	default:
		return session.Steered{}, fmt.Errorf("steer session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// PendingQuestions lists the AskUserQuestion calls a claude-code turn waits on
// (AC-E2). Like AwaitingApproval it is an optional capability, not part of
// the Client interface.
func (c *HTTPClient) PendingQuestions(ctx context.Context, pod string) ([]session.PendingQuestion, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+"/questions", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list questions in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list questions in session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Questions []session.PendingQuestion `json:"questions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("list questions in session pod %s: decode: %w", pod, err)
	}
	if out.Questions == nil {
		out.Questions = []session.PendingQuestion{}
	}
	return out.Questions, nil
}

// AnswerQuestion hands a person's answer to the turn waiting on it (AC-E2).
func (c *HTTPClient) AnswerQuestion(ctx context.Context, pod, questionID string, answers map[string]string) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		return err
	}
	target := c.baseURL(ip) + "/questions/" + url.PathEscape(questionID) + "/answer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("answer question in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	message := strings.TrimSpace(string(detail))
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", session.ErrInvalidInput, message)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", session.ErrQuestionNotFound, message)
	default:
		return fmt.Errorf("answer question in session pod %s: agent returned %d: %s", pod, resp.StatusCode, message)
	}
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

// AwaitingApproval reports whether the pod is blocked on a human decision right
// now (AC-F3). It is deliberately not on the Client interface — the service
// reaches it through an optional capability assertion, as it does
// CheckpointWithGeneration.
func (c *HTTPClient) AwaitingApproval(ctx context.Context, pod string) (bool, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+"/approval", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("read approval wait from session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf(
			"read approval wait from session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var out struct {
		Awaiting bool `json:"awaiting"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("decode approval wait from session pod %s: %w", pod, err)
	}
	return out.Awaiting, nil
}

// ActiveSchedules reports how many schedules the pod's scheduler holds
// (docs/prd/session-scheduler.md AC-P3). Like AwaitingApproval it is an
// optional capability, asked only on the idle reaper path.
func (c *HTTPClient) ActiveSchedules(ctx context.Context, pod string) (int, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+"/scheduler", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read scheduler state from session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf(
			"read scheduler state from session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var out struct {
		Active *int `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode scheduler state from session pod %s: %w", pod, err)
	}
	if out.Active == nil {
		return 0, fmt.Errorf("decode scheduler state from session pod %s: no active count", pod)
	}
	return *out.Active, nil
}

// AuthSlotSignedIn reports whether the pod's auth pool slot holds a login
// (AC-N4). Like AwaitingApproval it is an optional capability, not part of
// Client: only the release path asks, and only to rank the slot for the next
// session.
func (c *HTTPClient) AuthSlotSignedIn(ctx context.Context, pod string) (bool, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+"/auth-slot", nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("read auth slot from session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf(
			"read auth slot from session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var out struct {
		SignedIn *bool `json:"signedIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("decode auth slot from session pod %s: %w", pod, err)
	}
	if out.SignedIn == nil {
		return false, fmt.Errorf("decode auth slot from session pod %s: no signedIn", pod)
	}
	return *out.SignedIn, nil
}

// Stream opens the data plane's long-lived SSE output feed at offset, over the
// context-bounded streaming client rather than the short Read one.
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

// CreateLedgerRequest stores one user request on a task of the chat slot's
// ledger (AC-N6, write side). It is the only write on this path and it is not
// a ledger write: the sidecar appends a row of its own and leaves every task
// row untouched, so a request arriving while the session is updating the same
// task cannot take that update out from under it.
//
// Routing by pod is what makes it the slot's current ledger, as on the read
// side. The sidecar's own answers are carried through as the classes a caller
// can act on: a task this ledger does not have is not found, a reused id or a
// deleted task is a conflict.
func (c *HTTPClient) CreateLedgerRequest(ctx context.Context, pod string, in session.LedgerRequestInput) (session.LedgerRequestStored, error) {
	var stored session.LedgerRequestStored
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return stored, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return stored, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+"/ledger/requests", bytes.NewReader(body))
	if err != nil {
		return stored, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return stored, fmt.Errorf("store task ledger request in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		if err := json.Unmarshal(raw, &stored); err != nil {
			return session.LedgerRequestStored{}, fmt.Errorf("store task ledger request in session pod %s: decode: %w", pod, err)
		}
		return stored, nil
	case http.StatusNotImplemented:
		return stored, fmt.Errorf("%w: %s", session.ErrLedgerUnsupported, ledgerMessage(raw))
	case http.StatusNotFound:
		// A pod still running the earlier image has no such route at all, and
		// its 404 is the mux's plain text rather than the ledger's own JSON.
		// Saying "that task is gone" there would be wrong and misleading, so a
		// route that is not there is reported as unsupported.
		if !ledgerAnswered(raw) {
			return stored, fmt.Errorf("%w: this session pod predates the ledger request route", session.ErrLedgerUnsupported)
		}
		return stored, fmt.Errorf("%w: %s", session.ErrNotFound, ledgerMessage(raw))
	case http.StatusConflict:
		// The slot refuses a pick it no longer offers with its own code, so a
		// stale screen is not reported as a reused id.
		if ledgerCode(raw) == "stale_choice" {
			return stored, fmt.Errorf("%w: %s", session.ErrLedgerChoiceStale, ledgerMessage(raw))
		}
		return stored, fmt.Errorf("%w: %s", session.ErrConflict, ledgerMessage(raw))
	case http.StatusBadRequest:
		return stored, fmt.Errorf("%w: %s", session.ErrInvalidInput, ledgerMessage(raw))
	default:
		return stored, fmt.Errorf("store task ledger request in session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, ledgerMessage(raw))
	}
}

// ledgerAnswered says whether a body came from the ledger itself: its errors
// are JSON carrying a code. A pod from before these routes answers with the
// mux's plain "404 page not found", which is a different fact.
func ledgerAnswered(raw []byte) bool {
	var answer struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(raw, &answer) == nil && strings.TrimSpace(answer.Code) != ""
}

// ledgerCode is the ledger's own class for a refusal, empty when the body did
// not come from the ledger.
func ledgerCode(raw []byte) string {
	var answer struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return ""
	}
	return strings.TrimSpace(answer.Code)
}

// ledgerMessage is the sidecar's own sentence, which names the ledger's view of
// what went wrong without naming its database file.
func ledgerMessage(raw []byte) string {
	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err == nil && strings.TrimSpace(answer.Error) != "" {
		return answer.Error
	}
	return strings.TrimSpace(string(raw))
}

// LedgerEvents reads one keyset page of a task's history (AC-N8). Read-only and
// passive, like the task page: it starts no turn and changes nothing, and a pod
// that predates the route is reported as unsupported rather than as an empty
// history — an empty page would claim the task has no story.
func (c *HTTPClient) LedgerEvents(ctx context.Context, pod, taskID string, afterSeq int64, limit int) (session.LedgerTaskEventPage, error) {
	page := session.LedgerTaskEventPage{Events: []session.LedgerTaskEvent{}}
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return page, err
	}
	query := url.Values{}
	query.Set("task_id", taskID)
	if afterSeq > 0 {
		query.Set("after_seq", strconv.FormatInt(afterSeq, 10))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+"/ledger/events?"+query.Encode(), nil)
	if err != nil {
		return page, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return page, fmt.Errorf("read task history in session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(raw, &page); err != nil {
			return session.LedgerTaskEventPage{Events: []session.LedgerTaskEvent{}},
				fmt.Errorf("read task history in session pod %s: decode: %w", pod, err)
		}
		if page.Events == nil {
			page.Events = []session.LedgerTaskEvent{}
		}
		return page, nil
	case http.StatusNotImplemented:
		return page, fmt.Errorf("%w: %s", session.ErrLedgerUnsupported, ledgerMessage(raw))
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		if !ledgerAnswered(raw) {
			return page, fmt.Errorf("%w: this session pod predates the task history route", session.ErrLedgerUnsupported)
		}
		return page, fmt.Errorf("%w: %s", session.ErrNotFound, ledgerMessage(raw))
	case http.StatusBadRequest:
		return page, fmt.Errorf("%w: %s", session.ErrInvalidInput, ledgerMessage(raw))
	default:
		return page, fmt.Errorf("read task history in session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, ledgerMessage(raw))
	}
}
