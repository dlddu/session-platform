// The control plane's two hops onto the seal that says who writes a slot's work
// records, and onto the bound monitor's calls on them.
//
// Reaching them is the same relay as the reads above: the database is mounted in
// the sidecar, the agent is the container on the pod network, and these paths
// are its (data-plane/cmd/agent: agentWorkWriterPath, agentWorkCallPath).
//
// What is new is what the answers mean. The seal's POST answers only after the
// previous writer is already refused — the sidecar checks and moves the seal in
// one transaction against the same database every write takes — so a caller
// that has a 200 here may open the next writer, and must not before. Nothing on
// this path waits out a propagation delay, because a delay is a guess about
// another machine and a guess is not evidence that a writer stopped.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// WorkWriter reads who may write this slot's work records.
func (c *HTTPClient) WorkWriter(ctx context.Context, pod, token string) (session.WorkWriter, error) {
	raw, status, err := c.getWork(ctx, pod, "/work/writer", token)
	if err != nil {
		return session.WorkWriter{}, err
	}
	if status != http.StatusOK {
		return session.WorkWriter{}, workWriteError(pod, status, raw)
	}
	var view struct {
		Writer session.WorkWriter `json:"writer"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return session.WorkWriter{}, fmt.Errorf("read the work writer of session pod %s: decode: %w", pod, err)
	}
	return view.Writer, nil
}

// SealWorkWriter hands the records to a monitor, or gives them back.
func (c *HTTPClient) SealWorkWriter(
	ctx context.Context, pod, token string, in session.WorkSeal,
) (session.WorkSealResult, error) {
	var empty session.WorkSealResult
	raw, status, err := c.postWork(ctx, pod, "/work/writer", token, in)
	if err != nil {
		return empty, err
	}
	if status != http.StatusOK {
		return empty, workWriteError(pod, status, raw)
	}
	var result session.WorkSealResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return empty, fmt.Errorf("seal the work writer of session pod %s: decode: %w", pod, err)
	}
	return result, nil
}

// CallWork makes one work tool call as the bound monitor. The slot validates
// the arguments and enforces the seal exactly as it does for the session's own
// model: this adds no tool and relaxes no rule, it only says who is calling.
func (c *HTTPClient) CallWork(
	ctx context.Context, pod, token, name string, args json.RawMessage,
) (session.WorkCallResult, error) {
	var empty session.WorkCallResult
	raw, status, err := c.postWork(ctx, pod, "/work/call", token,
		map[string]any{"name": name, "args": args})
	if err != nil {
		return empty, err
	}
	switch status {
	case http.StatusOK, http.StatusConflict:
		// The slot's own tool result, refusal and all: the monitor's caller has
		// to read the same code the session's own model would.
		var result session.WorkCallResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return empty, fmt.Errorf("call work %s in session pod %s: decode: %w", name, pod, err)
		}
		return result, nil
	default:
		return empty, workWriteError(pod, status, raw)
	}
}

// getWork and postWork are the shared hops onto the agent's seal paths. The
// credential is the sidecar's, derived per session by the caller: the agent in
// between holds none and only passes the header along.
func (c *HTTPClient) getWork(ctx context.Context, pod, path, token string) ([]byte, int, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("get %s from session pod %s: %w", path, pod, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("get %s from session pod %s: %w", path, pod, err)
	}
	return raw, resp.StatusCode, nil
}

func (c *HTTPClient) postWork(
	ctx context.Context, pod, path, token string, body any,
) ([]byte, int, error) {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return nil, 0, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL(ip)+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("post %s to session pod %s: %w", path, pod, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("post %s to session pod %s: %w", path, pod, err)
	}
	return raw, resp.StatusCode, nil
}
