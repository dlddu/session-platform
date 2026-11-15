package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AutoArchiveTargetPath is the agent route that holds the upload URL its pod
// uses for the automatic archive it writes when terminated (AC-B5). Keep in
// sync with data-plane/cmd/agent (autoArchiveTargetPath).
const AutoArchiveTargetPath = "/auto-archive/target"

type autoArchiveTarget struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SetAutoArchiveTarget hands the pod's agent the upload URL it may use until
// expires.
func (c *HTTPClient) SetAutoArchiveTarget(ctx context.Context, pod, url string, expires time.Time) error {
	body, err := json.Marshal(autoArchiveTarget{URL: url, ExpiresAt: expires.UTC()})
	if err != nil {
		return err
	}
	return c.autoArchiveTarget(ctx, http.MethodPut, pod, bytes.NewReader(body))
}

// ClearAutoArchiveTarget takes the URL back, so the pod writes no automatic
// archive when the control plane reclaims it after a committed snapshot.
func (c *HTTPClient) ClearAutoArchiveTarget(ctx context.Context, pod string) error {
	return c.autoArchiveTarget(ctx, http.MethodDelete, pod, nil)
}

func (c *HTTPClient) autoArchiveTarget(ctx context.Context, method, pod string, body io.Reader) error {
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL(ip)+AutoArchiveTargetPath, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("automatic archive target for session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("automatic archive target for session pod %s: agent returned %d: %s",
			pod, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
