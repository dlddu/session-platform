// The stub's lane half (AC-N11).
//
// A lane is a conversation inside one pod, so the stub keeps one buffer per
// (pod, lane) and the parent lane's buffer *is* the pod's own — that is what
// makes "a call that names no lane reaches what it always reached" a property a
// test can see rather than a claim.
package agent

import (
	"context"
	"io"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// LanePaired makes this stub answer as a manager pod running both lanes. A stub
// left unpaired reports one conversation, which is every workload but a paired
// manager.
func (c *StubClient) LanePaired(paired bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lanePaired = paired
}

// LaneUnreachable makes the inventory answer as a pod that has no such route —
// one started before the pair. It is reported as unsupported rather than as a
// pod with no conversations.
func (c *StubClient) LaneUnreachable(unreachable bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.laneUnreachable = unreachable
}

// LaneWrites is what WriteLane was given for one lane of pod, in order.
func (c *StubClient) LaneWrites(pod string, lane session.Lane) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.laneWrites[laneKey{pod, lane}]...)
}

type laneKey struct {
	pod  string
	lane session.Lane
}

// laneBufKey is where a lane's output lives. The parent lane shares the pod's
// own buffer, so a write with no lane and a write naming the parent are the
// same conversation — as they are in the pod.
func (c *StubClient) laneBuf(pod string, lane session.Lane) []byte {
	if lane.IsDefault() {
		return c.bufs[pod]
	}
	return c.laneBufs[laneKey{pod, lane}]
}

func (c *StubClient) WriteLane(_ context.Context, pod string, lane session.Lane, payload string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.laneWrites == nil {
		c.laneWrites = map[laneKey][]string{}
	}
	key := laneKey{pod, lane}
	c.laneWrites[key] = append(c.laneWrites[key], payload)
	if lane.IsDefault() {
		c.bufs[pod] = append(c.bufs[pod], payload...)
		return nil
	}
	if c.laneBufs == nil {
		c.laneBufs = map[laneKey][]byte{}
	}
	c.laneBufs[key] = append(c.laneBufs[key], payload...)
	return nil
}

func (c *StubClient) ReadLane(_ context.Context, pod string, lane session.Lane, offset int64) (string, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := c.laneBuf(pod, lane)
	n := int64(len(buf))
	if offset < 0 || offset >= n {
		return "", n, nil
	}
	return string(buf[offset:]), n, nil
}

// StreamLane opens a lane's feed. It reuses Stream's own event shape for the
// parent lane and serves the lane's buffer for the other, which is enough for a
// test to see that the two cursors are not the same cursor.
func (c *StubClient) StreamLane(ctx context.Context, pod string, lane session.Lane, offset int64) (io.ReadCloser, error) {
	if lane.IsDefault() {
		return c.Stream(ctx, pod, offset)
	}
	c.mu.Lock()
	buf := append([]byte(nil), c.laneBuf(pod, lane)...)
	c.mu.Unlock()
	return c.streamBytes(buf, offset)
}

// Lanes answers which conversations this stub's pod runs.
func (c *StubClient) Lanes(_ context.Context, pod string) (session.LaneInventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.laneUnreachable {
		return session.LaneInventory{}, session.ErrLanesUnsupported
	}
	inventory := session.LaneInventory{
		Lanes: []session.LaneView{{Role: session.LaneParent, Path: "/read", Ready: true}},
	}
	if c.lanePaired {
		inventory.Paired = true
		inventory.Lanes = append(inventory.Lanes,
			session.LaneView{Role: session.LaneMonitor, Path: "/monitor/read", Ready: true})
	}
	return inventory, nil
}
