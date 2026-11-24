// The two lanes of a manager pod (AC-N11).
//
// A manager session is one person's chat and the work behind it. Those are two
// jobs with incompatible timing: the parent has to be able to spend a minute
// writing an answer to the person, while the work it set going needs looking
// after on its own clock — a scheduled check, a result to collect, a ledger row
// to move. One conversation cannot do both, because a turn is serial: whatever
// the single worker is doing, nothing else is.
//
// So the pod runs two of them. Each lane is a whole agent workload — its own
// conversation with its own id, its own queue and serial worker, its own output
// buffer and therefore its own cursor, and its own managed config and tool
// surface. They share the pod and nothing else that matters: neither can see
// the other's conversation, and the parent writing a long answer does not hold
// the monitor's worker for a moment.
//
// Why one pod rather than two sessions:
//
//   - The work records live on the slot this pod mounts, and the sidecar that
//     serves them binds loopback. Two pods would need a second copy of that
//     database or a network path to it; one pod has neither problem, and both
//     lanes read the same records through the same sidecar.
//   - The slot's login is one login. Two pods on one slot are two writers of
//     one file across a network filesystem; two lanes in one pod go through one
//     process's lock (auth_slot.go).
//   - A session is the unit the control plane starts, freezes, archives and
//     deletes. Two sessions for one person's chat would be two of each, and the
//     pair would have to agree about which of them a snapshot was of.
//
// What the two lanes are for, and the division the relay enforces: the parent
// talks to the person and reads the records; the monitor moves the work and owns
// writing the records. The parent hands over what it learns from the person
// (relay_send) and reads back what came of it (relay_status); nothing about this
// division is advisory — the records' write tools are not on the parent lane's
// surface (session_db_mcp.go).
//
// What a lane does *not* keep: the monitor's conversation is not in the
// session's archive. Its state directory sits beside the archived tree rather
// than inside it, so a snapshot, a restore and a container restart all start the
// monitor with a fresh conversation. That is deliberate. The monitor's record of
// what it was doing is the slot's database — its monitoring rows and its
// unsettled relay messages — and recovering from there is a property worth
// having rather than a gap: a lane that came back with a conversation it could
// not trust would still have to read the records to know where it stood.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	// managerMonitorLaneEnv turns the pair on. The control plane sets it on a
	// codex-manager pod that has a session database to coordinate through;
	// unset is a manager with one lane, which is what every session created
	// before the pair keeps being. Keep in sync with
	// control-plane/internal/adapter/k8s (ManagerMonitorLaneEnvVar).
	managerMonitorLaneEnv = "MANAGER_MONITOR_LANE"
	// The two values of that opt-in. Anything else is a refusal rather than
	// off, so a misspelling cannot read as "not paired".
	managerMonitorLaneOn  = "1"
	managerMonitorLaneOff = "0"
	// managerMonitorStateDirEnv is where the monitor lane keeps its own state
	// tree: its HOME, its managed config and its workspace. It is on the pod's
	// state volume but outside the archived tree on purpose (see the file
	// comment). Keep in sync with control-plane/internal/adapter/k8s
	// (ManagerMonitorStateDirEnvVar).
	managerMonitorStateDirEnv = "MANAGER_MONITOR_STATE_DIR"

	// managerLanePathPrefix is where the monitor lane's conversation is
	// reachable on the agent: /monitor/read, /monitor/write, /monitor/stream.
	// The parent lane keeps the agent's own paths, so every client that knew
	// this agent before the pair still talks to the person's chat.
	managerLanePathPrefix = "/monitor"

	// agentLanesPath is where this pod says which lanes it runs. The pod
	// answers it rather than the control plane assembling it from
	// configuration: the pod is what actually started the lanes, so this cannot
	// claim a lane that is not there. Keep in sync with
	// control-plane/internal/adapter/agent (agentLanesPath).
	agentLanesPath = "/lanes"
)

// managerLanePaths is what a lane answers: its conversation and the diagnostics
// that are about that conversation. Everything else an agent serves —
// checkpoint, restore, the auth slot's state, the work views the control plane
// relays, the plugin refresh — is about the pod or the slot rather than one
// lane, and the pod has one of each. A lane address for one of those would be a
// second way to do a thing that must happen once, so there is none.
var managerLanePaths = map[string]bool{
	"/healthz": true,
	"/read":    true,
	"/write":   true,
	"/stream":  true,
	"/queue":   true,
	"/steer":   true,
}

// managerLane is which lane a workload is, and the state that is its own.
type managerLane struct {
	// role is relayLaneParent or relayLaneMonitor, empty for a manager running
	// a single lane.
	role string
	// incarnation is this run of this lane. It is minted at start and reaches
	// the database through the lane's MCP address, which is how a relay message
	// a lane held when it died becomes claimable again exactly once
	// (session_work_relay.go).
	incarnation string
	// stateDir overrides the pod's state directory; empty keeps it.
	stateDir string
	// slot is the login both lanes share, so one process's lock serialises the
	// copies rather than two unsynchronised ones racing (auth_slot.go). Nil
	// builds one from the environment.
	slot *authSlot
}

// managerPairEnabled reports whether this pod runs the pair. It is read once,
// in main: a lane that exists has to exist for the pod's whole life, because a
// lane appearing later would be a lane the person's chat never handed anything
// to.
//
// A value that is neither off nor on is an error rather than off, for the reason
// the raw SQL opt-in is (session_db_mcp.go): a deployment that meant to turn the
// pair on and misspelled it would otherwise run a single-lane manager that looks
// exactly like one that was never paired.
func managerPairEnabled(workload string) (bool, error) {
	on, err := managerPairOptIn()
	if err != nil || !on {
		return false, err
	}
	if workload != workloadCodexManager && workload != legacyWorkloadCodexChat {
		return false, fmt.Errorf("%s is codex-manager's; workload %q runs one lane", managerMonitorLaneEnv, workload)
	}
	return true, nil
}

// managerPairOptIn is the opt-in's value alone, without the workload it belongs
// to. The manager's own container reads it through managerPairEnabled; the
// sidecar beside it reads it here, because whether to sweep for a lane to wake
// is the same question as whether that lane exists (session_work_relay_wake.go)
// and two opt-ins that could disagree would be one too many.
func managerPairOptIn() (bool, error) {
	switch raw := strings.TrimSpace(os.Getenv(managerMonitorLaneEnv)); raw {
	case "", managerMonitorLaneOff:
		return false, nil
	case managerMonitorLaneOn:
		return true, nil
	default:
		return false, fmt.Errorf("%s must be unset, %q or %q, not %q",
			managerMonitorLaneEnv, managerMonitorLaneOff, managerMonitorLaneOn, raw)
	}
}

// managerMonitorStateDir is the monitor lane's state tree. It defaults beside
// the parent's rather than inside it, so the parent's archive neither carries
// the monitor's conversation nor is made inconsistent by the monitor writing
// while a checkpoint is being taken.
func managerMonitorStateDir(parentStateDir string) string {
	if dir := strings.TrimSpace(os.Getenv(managerMonitorStateDirEnv)); dir != "" {
		return dir
	}
	parent := filepath.Clean(parentStateDir)
	return filepath.Join(filepath.Dir(parent), filepath.Base(parent)+"-monitor")
}

// newLaneIncarnation mints this run's identity. It is random rather than a
// counter or a timestamp: the database only has to be able to tell two runs
// apart, and a counter would need somewhere to live that survives the restart
// it exists to detect.
func newLaneIncarnation() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// errNotALaneDBURL is a SESSION_DB_MCP_URL a lane cannot be derived from. The
// lane address is this address with the lane's own path, so an address whose
// path is not the database MCP's is one this code would be guessing at.
var errNotALaneDBURL = errors.New(sessionDBMCPURLEnv + " must end in " + sessionDBMCPPath + " for a lane address to be derived from it")

// sessionDBLaneMCPURL is the address this lane calls the database from: the
// sidecar's own, with the lane and this run in the path. The role is in the
// address because the address is in the lane's managed config, which the
// platform rewrites on every boot — so a lane cannot call as the other one, and
// a run cannot call as an earlier one (session_db_mcp.go).
func sessionDBLaneMCPURL(raw, lane, incarnation string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Path != sessionDBMCPPath {
		return "", errNotALaneDBURL
	}
	if !isRelayLane(lane) {
		return "", fmt.Errorf("a manager lane is %s", strings.Join(relayLanes, " or "))
	}
	if !workID.MatchString(incarnation) {
		return "", errors.New("an incarnation is 1..128 letters, digits, _ or -")
	}
	parsed.Path = sessionDBLaneMCPPath(lane, incarnation)
	return parsed.String(), nil
}

// withManagerMonitorLane puts the monitor lane's conversation beside the
// parent's on the same agent port, under /monitor. One port keeps the pod's
// network boundary exactly as it was: no new listener, no NetworkPolicy change,
// and nothing new reachable from outside the pod that was not reachable before.
//
// The lane serves only its conversation (managerLanePaths). A path under the
// prefix that is not one of those is refused by name rather than falling
// through to the parent's handler, because silently answering it from the
// parent's conversation would be the one thing the pair exists to prevent.
func withManagerMonitorLane(parent http.Handler, monitor http.Handler, logger *slog.Logger) http.Handler {
	if monitor == nil {
		return parent
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != managerLanePathPrefix && !strings.HasPrefix(r.URL.Path, managerLanePathPrefix+"/") {
			parent.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == managerLanePathPrefix+agentLanesPath {
			// The inventory is the pod's, not a lane's: asking a lane what lanes
			// exist would make the answer depend on which one you asked.
			http.Error(w, agentLanesPath+" is the pod's answer and is served without the "+
				managerLanePathPrefix+" prefix", http.StatusNotFound)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, managerLanePathPrefix)
		if !managerLanePaths[rest] {
			http.Error(w, "the monitor lane serves its own conversation only ("+
				strings.Join(sortedManagerLanePaths(), ", ")+"); everything else is the pod's and is served without the "+
				managerLanePathPrefix+" prefix", http.StatusNotFound)
			return
		}
		scoped := r.Clone(r.Context())
		scoped.URL.Path = rest
		logger.Debug("monitor lane request", "path", rest, "method", r.Method)
		monitor.ServeHTTP(w, scoped)
	})
}

// laneView is one lane as the control plane relays it. The pod is not in it:
// the pod is who answered, and this agent only ever answers for itself — which
// is precisely why the control plane can stamp the same pod onto every lane it
// got back and have that be a fact rather than an assumption (AC-N11).
type laneView struct {
	Role  string `json:"role"`
	Path  string `json:"path"`
	Ready bool   `json:"ready"`
}

type laneInventory struct {
	Paired bool       `json:"paired"`
	Lanes  []laneView `json:"lanes"`
}

// serveManagerLanes answers which conversations this pod runs. A workload with
// one conversation answers that one and paired=false, so a reader can tell "one
// lane" from "this pod could not be asked" — which is a 404 from a pod that
// predates the route.
func serveManagerLanes(w http.ResponseWriter, parent, monitor *claudeWorkload) {
	inventory := laneInventory{Lanes: []laneView{}}
	if parent == nil {
		http.Error(w, "this workload runs no conversation", http.StatusNotFound)
		return
	}
	inventory.Lanes = append(inventory.Lanes, laneView{
		Role: relayLaneParent, Path: "/read", Ready: parent.isReady(),
	})
	if monitor != nil {
		inventory.Paired = true
		inventory.Lanes = append(inventory.Lanes, laneView{
			Role: relayLaneMonitor, Path: managerLanePathPrefix + "/read", Ready: monitor.isReady(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(inventory)
}

func sortedManagerLanePaths() []string {
	out := make([]string, 0, len(managerLanePaths))
	for _, path := range []string{"/healthz", "/read", "/write", "/stream", "/queue", "/steer"} {
		if managerLanePaths[path] {
			out = append(out, managerLanePathPrefix+path)
		}
	}
	return out
}
