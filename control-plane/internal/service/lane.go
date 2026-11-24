// Read, write and stream on one lane of a manager pod, and the answer to "what
// lanes does this session have" (AC-N11).
//
// The state branching is the session's own: a lane is a conversation inside one
// pod, not a session of its own, so a read on either lane promotes an idle
// session and restores a snapshot exactly as a read does, and a write queues on
// the lane it names after the same activation. What differs is only where in the
// pod the call lands.
//
// The inventory is the exception: it is passive, like the work reads and
// Questions. Asking which lanes a session has must not be the thing that starts
// its pod — a reader checking that the pair is in one pod would otherwise bring
// a frozen session back to do it.
package service

import (
	"context"
	"fmt"
	"io"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// laneAgent is the lane half of the workload I/O port, reached by capability
// assertion like the steer and work clients: a Client that does not have it is
// a deployment whose agent cannot address a second conversation, which is
// reported as unsupported rather than as a lane with nothing in it.
type laneAgent interface {
	ReadLane(ctx context.Context, pod string, lane session.Lane, offset int64) (string, int64, error)
	WriteLane(ctx context.Context, pod string, lane session.Lane, payload string) error
	StreamLane(ctx context.Context, pod string, lane session.Lane, offset int64) (io.ReadCloser, error)
	Lanes(ctx context.Context, pod string) (session.LaneInventory, error)
}

// ReadLane reads one lane's output after offset (AC-N11, AC-C2).
func (s *Service) ReadLane(ctx context.Context, id string, lane session.Lane, offset int64) (*session.ReadResult, error) {
	if lane.IsDefault() {
		return s.Read(ctx, id, offset)
	}
	reader, err := s.laneIO()
	if err != nil {
		return nil, err
	}
	sess, branch, err := s.activate(ctx, id, session.RestoreOptions{})
	if err != nil {
		return nil, err
	}
	payload, next, err := reader.ReadLane(ctx, sess.Pod, lane, offset)
	if err != nil {
		if adopted, lostErr := s.podLost(ctx, id, sess, err); !adopted {
			return nil, lostErr
		}
		return s.ReadLane(ctx, id, lane, offset)
	}
	s.touch(ctx, id)
	return &session.ReadResult{
		Session:    sess,
		Path:       dispatchPath(branch, "read"),
		Payload:    payload,
		NextOffset: next,
	}, nil
}

// StreamLane opens one lane's live output. Like Stream it is passive: it does
// not promote idle, restore a snapshot or touch LastAccess.
func (s *Service) StreamLane(ctx context.Context, id string, lane session.Lane, offset int64) (io.ReadCloser, error) {
	if lane.IsDefault() {
		return s.Stream(ctx, id, offset)
	}
	streamer, err := s.laneIO()
	if err != nil {
		return nil, err
	}
	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State == session.StateSnapshot || sess.Pod == "" {
		return nil, session.ErrInvalidState
	}
	return streamer.StreamLane(ctx, sess.Pod, lane, offset)
}

// SessionLanes reports the conversations this session's pod runs, with the pod
// each one runs in.
//
// The pod is stamped from the session record onto every lane the pod itself
// reported. That is what makes "the pair is in one pod" a fact a reader can
// check at the entry point rather than something the control plane asserts: the
// lanes come from one pod's own answer, so if they were in different pods there
// would be no single answer to put them in.
func (s *Service) SessionLanes(ctx context.Context, id string) (session.LanePage, error) {
	sess, err := s.Get(ctx, id)
	if err != nil {
		return session.LanePage{}, err
	}
	page := session.LanePage{Session: sess, Pod: sess.Pod, Lanes: []session.LaneView{}}
	if sess.State == session.StateSnapshot || sess.Pod == "" {
		// A session with no live pod runs no conversation right now. Saying so
		// is not the same as saying it has one lane, and it must not be the
		// thing that starts the pod.
		return page, fmt.Errorf("%w: this session has no live pod to ask", session.ErrInvalidState)
	}
	reporter, err := s.laneIO()
	if err != nil {
		return page, err
	}
	inventory, err := reporter.Lanes(ctx, sess.Pod)
	if err != nil {
		return page, err
	}
	page.Paired = inventory.Paired
	for _, lane := range inventory.Lanes {
		lane.Pod = sess.Pod
		page.Lanes = append(page.Lanes, lane)
	}
	return page, nil
}

// laneIO is the lane capability of this deployment's agent client.
func (s *Service) laneIO() (laneAgent, error) {
	agent, ok := s.agent.(laneAgent)
	if !ok {
		return nil, fmt.Errorf("%w: this deployment's agent client cannot address a lane", session.ErrLanesUnsupported)
	}
	return agent, nil
}

// laneWriter is how a write reaches one lane. The parent lane is the agent's
// own write, so a write that names no lane takes exactly the path it did before
// the pair existed.
func (s *Service) laneWriter(sess *session.Session, lane session.Lane) (func(context.Context, string, string) error, error) {
	if lane.IsDefault() {
		return s.agent.Write, nil
	}
	if sess.WorkloadType != session.WorkloadTypeCodexManager {
		return nil, fmt.Errorf("%w: only a manager session runs a second conversation", session.ErrLanesUnsupported)
	}
	writer, err := s.laneIO()
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pod, payload string) error {
		return writer.WriteLane(ctx, pod, lane, payload)
	}, nil
}

// errLaneWithToolApprovals is both choices at once: a lane that is not the
// person's chat, and the owner deciding that turn's tool calls. The second is
// claude-code's (AC-E2) and the first is a manager's, so no write can be both —
// and refusing is better than quietly dropping one of them.
var errLaneWithToolApprovals = fmt.Errorf(
	"%w: a write cannot both name a lane and ask the session owner to decide the turn's tool calls",
	session.ErrInvalidInput)
