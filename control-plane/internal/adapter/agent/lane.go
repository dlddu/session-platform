// The control plane's hops onto one lane of a manager pod (AC-N11).
//
// A manager pod with the pair turned on serves the person's chat on the agent's
// own paths and the monitor's conversation under a prefix beside them. So a
// lane is a path prefix and nothing else: one pod, one port, one network
// boundary — which is why addressing the second conversation needs no new
// listener, no new Service and no change to what is reachable from outside the
// pod.
//
// Keep the prefix and the inventory path in sync with data-plane/cmd/agent
// (managerLanePathPrefix, agentLanesPath).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const (
	// monitorLanePathPrefix is where the monitor lane's conversation is served.
	monitorLanePathPrefix = "/monitor"
	// agentLanesPath is what a pod answers about its own lanes.
	agentLanesPath = "/lanes"
)

// lanePathPrefix is where one lane's conversation is served. The parent lane is
// the agent's own paths, so its prefix is empty — that is what makes a client
// that knows nothing about lanes reach the person's chat.
func lanePathPrefix(lane session.Lane) (string, error) {
	switch {
	case lane.IsDefault():
		return "", nil
	case lane == session.LaneMonitor:
		return monitorLanePathPrefix, nil
	}
	return "", fmt.Errorf("%w: %q is not a lane of a manager pod", session.ErrInvalidInput, lane)
}

// ReadLane reads one lane's output after offset.
func (c *HTTPClient) ReadLane(ctx context.Context, pod string, lane session.Lane, offset int64) (string, int64, error) {
	prefix, err := lanePathPrefix(lane)
	if err != nil {
		return "", 0, err
	}
	return c.readLane(ctx, pod, prefix, offset)
}

// WriteLane queues a prompt on one lane. It is the same acceptance the session's
// own write is: queued, not finished.
func (c *HTTPClient) WriteLane(ctx context.Context, pod string, lane session.Lane, payload string) error {
	prefix, err := lanePathPrefix(lane)
	if err != nil {
		return err
	}
	return c.write(ctx, pod, prefix, payload, nil)
}

// StreamLane opens one lane's live output.
func (c *HTTPClient) StreamLane(ctx context.Context, pod string, lane session.Lane, offset int64) (io.ReadCloser, error) {
	prefix, err := lanePathPrefix(lane)
	if err != nil {
		return nil, err
	}
	return c.streamLane(ctx, pod, prefix, offset)
}

// Lanes asks the pod which conversations it runs.
//
// A pod that predates the pair, or one whose workload runs a single
// conversation, has no such route and answers 404 for an unknown path. That is
// reported as unsupported rather than as "no lanes": the chat is there, and a
// caller has to be able to tell "this pod runs one conversation" from "this pod
// answered nothing".
func (c *HTTPClient) Lanes(ctx context.Context, pod string) (session.LaneInventory, error) {
	var inventory session.LaneInventory
	ip, err := c.resolve(ctx, pod)
	if err != nil {
		return inventory, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(ip)+agentLanesPath, nil)
	if err != nil {
		return inventory, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return inventory, fmt.Errorf("read the lanes of session pod %s: %w", pod, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message := strings.TrimSpace(string(body))
		switch resp.StatusCode {
		case http.StatusNotFound, http.StatusNotImplemented:
			return inventory, fmt.Errorf("%w: %s", session.ErrLanesUnsupported, message)
		default:
			return inventory, fmt.Errorf("read the lanes of session pod %s: agent returned %d: %s",
				pod, resp.StatusCode, message)
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&inventory); err != nil {
		return session.LaneInventory{}, fmt.Errorf("read the lanes of session pod %s: decode: %w", pod, err)
	}
	if inventory.Lanes == nil {
		inventory.Lanes = []session.LaneView{}
	}
	return inventory, nil
}
