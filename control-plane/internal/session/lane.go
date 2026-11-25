// The lanes of a manager session (AC-N11).
//
// A manager pod runs the person's chat and, when the deployment turns the pair
// on, a second execution context beside it that moves the work. They are one
// session — one pod, one archive, one lifecycle, one slot — with two
// conversations in it, so the session's read and write take which lane they
// mean and default to the one the person talks to.
//
// Defaulting rather than requiring is what keeps every client that predates the
// pair correct: a read with no lane is the chat, exactly as it was.
package session

import "errors"

// Lane names one conversation of a manager pod. The empty lane is the default
// one — the person's chat — which is also the only one a workload that runs a
// single lane has.
type Lane string

const (
	// LaneParent talks to the person and reads the work records.
	LaneParent Lane = "parent"
	// LaneMonitor moves the work and owns writing the work records.
	LaneMonitor Lane = "monitor"
)

// Lanes is every lane a caller may name.
var Lanes = []Lane{LaneParent, LaneMonitor}

// NormalizeLane reads the lane a request names. Empty is the parent, because a
// request that does not mention lanes is about the person's chat. Anything that
// is not a lane is refused rather than defaulted: a misspelt lane silently
// reading the chat would be a client that thinks it is watching the monitor.
func NormalizeLane(raw string) (Lane, error) {
	switch Lane(raw) {
	case "", LaneParent:
		return LaneParent, nil
	case LaneMonitor:
		return LaneMonitor, nil
	}
	return "", ErrInvalidInput
}

// IsDefault reports whether this lane is the one a client that knows nothing
// about lanes reaches — the conversation served on the agent's own paths.
func (l Lane) IsDefault() bool { return l == "" || l == LaneParent }

// ErrLanesUnsupported is a session whose workload runs one conversation: every
// type but a manager with the pair turned on, and a manager pod started before
// it. It is not "no lanes" — the chat is there — it is "there is no second one
// to address", which a caller has to be able to tell from an empty answer.
var ErrLanesUnsupported = errors.New("this session's workload runs a single conversation")

// LaneView is one lane as a reader outside the session sees it: which lane, and
// the pod it runs in. The pod is on every lane rather than once beside them
// because the thing worth being able to check is that they are the same one.
type LaneView struct {
	Role Lane   `json:"role"`
	Pod  string `json:"pod"`
	// Path is where this lane's conversation is served on its pod's agent,
	// which is what makes two lanes in one pod two addresses rather than one.
	Path string `json:"path"`
	// Ready is the lane's own readiness: a restored pod's chat can still be
	// waiting for its archive while the monitor is already working.
	Ready bool `json:"ready"`
}

// LaneInventory is what a pod answers about itself. It comes from the pod
// rather than from the control plane's own configuration on purpose: the pod is
// what actually runs the lanes, and an answer assembled from configuration
// could say "paired" about a pod that failed to start its second lane.
type LaneInventory struct {
	Paired bool       `json:"paired"`
	Lanes  []LaneView `json:"lanes"`
}

// LanePage is the answer to "what lanes does this session have, and are they in
// one pod".
type LanePage struct {
	Session *Session   `json:"session"`
	Pod     string     `json:"pod"`
	Paired  bool       `json:"paired"`
	Lanes   []LaneView `json:"lanes"`
}
