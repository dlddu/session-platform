//go:build e2e

// 검증 시나리오: agent-auth-pool.md#시나리오 9
//
// The manager pair on a real pod (AC-N11): one pod that the ordinary create
// path built, its workload container and the session-DB sidecar that was
// already there, and the handover going parent -> monitor -> records -> parent
// across them.
//
// Why this file exists at all. Every other check of the pair is in-process:
// data-plane/cmd/agent/session_work_relay_wake_test.go stands two lane
// workloads behind one httptest port with a real database and a real waker, but
// it stands them itself. What it cannot show is that the *deployment* produces
// that shape — that the ConfigMap opt-in reaches both containers, that the
// monitor lane's state tree exists on the pod's volume, that the lane MCP
// address the platform renders into each lane's managed config is the one the
// sidecar answers, and that the sidecar's loopback hop crosses a real container
// boundary to a real lane. Those are the clauses the unit test is structurally
// unable to buy, and they are the ones that break in a deployment.
//
// What stands in for the model, and why that is not the interesting part. There
// is no ChatGPT login on this SUT (AC-J3: a codex session signs itself in, and
// that is outside e2e), so neither lane can run a model turn that decides
// anything. So the two lanes' *decisions* are scripted: this test makes the
// calls a parent model would make at the parent lane's own MCP address, and the
// ones a monitor model would make at the monitor's. Everything between those
// calls is the product — the addresses, the sidecar, the database, the role
// split, the wake. The one thing that is emphatically not scripted is the wake
// itself: nothing here writes to the monitor lane, so a monitor turn can only
// have been started by the sidecar.
//
// What this file deliberately does NOT assert, and why:
//
//   - that a model reads the wake prompt and decides to claim. No login, no
//     model — and with no login the CLI hangs rather than answering, so a woken
//     turn here never gets past being started. What this file buys is that the
//     turn was started by the sidecar and given the wake prompt and nothing
//     else; what the model would then do with it is the unit test's.
//   - claim recovery after a workload-container restart. The scenario's
//     expected result (h) wants the container restarted on its own, and the
//     harness has no container-level restart — deleting the pod takes the
//     sidecar and the database's open handle with it, which is a different
//     event. Recovery stays with the unit test, which can kill one lane.
//   - that a misspelt opt-in fails the pod's start. That needs a second SUT
//     whose ConfigMap says something else, and the gate for it would be a pod
//     that never becomes ready — a deployment variant this overlay does not
//     have. Still owned by managerPairOptIn's unit test.
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The pod-side facts of AC-N11, spelled out here because this suite knows the
// cluster and the wire rather than the domain types.
const (
	lanePairParentHome  = "/session/state/home"
	lanePairMonitorHome = "/session/state-monitor/home"
	codexManagedConfig  = "/.codex/config.toml"
	// The sidecar's table in each lane's managed config. Its url is the lane's
	// own MCP address, role and incarnation included.
	lanePairDBTable = "[mcp_servers.db]"
	// The one message this test hands over. Keep it in step with the probe.
	lanePairHandoverID      = "e2e-handover-1"
	lanePairTaskID          = "e2e-rollout"
	lanePairHandoverSubject = "the person chose the staged rollout"
	lanePairHandoverBody    = "Record the choice on the work and take the first step."
	// What the person says to the parent. Distinct from everything else here so
	// its absence from the monitor's buffer means something.
	lanePairParentPrompt = "e2e-parent-prompt-keep-this-lane-busy"
	// A phrase from the wake prompt the sidecar sends (relayWakePrompt in
	// data-plane/cmd/agent/session_work_relay_wake.go). Finding it in the
	// monitor lane's scrollback is the wake, in the lane's own words.
	lanePairWakePhrase = "has handed you work that nobody has settled"
)

// lanePairPod is the session's one pod, its workload container and its sidecar.
type lanePairPod struct {
	pod      corev1.Pod
	workload corev1.Container
	sidecar  corev1.Container
}

func lanePairPodOf(t *testing.T, cs kubernetes.Interface, id string) lanePairPod {
	t.Helper()
	pod := sessionWorkloadPod(t, cs, id)
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want the agent and the sidecar — the pair adds neither", len(pod.Spec.Containers))
	}
	out := lanePairPod{pod: pod}
	for _, c := range pod.Spec.Containers {
		if c.Name == "k3s-mcp-proxy" {
			out.sidecar = c
			continue
		}
		out.workload = c
	}
	if out.sidecar.Name == "" || out.workload.Name == "" {
		t.Fatalf("pod %s has no workload/sidecar pair: %+v", pod.Name, pod.Spec.Containers)
	}
	return out
}

// execOut runs a command in the workload container and fails on a non-zero exit.
func execOut(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, pod string, command ...string) string {
	t.Helper()
	stdout, stderr, err := execInAgent(ctx, cs, cfg, sessionNamespace(), pod, command)
	if err != nil {
		t.Fatalf("exec %v: %v\nstdout:\n%s\nstderr:\n%s", command, err, stdout, stderr)
	}
	return stdout
}

// laneMCPURL is the address this lane calls the records from, read out of the
// managed config the platform rewrote on this pod's boot. Taking it from there
// rather than composing it is the point: a lane's role lives in its address, so
// the config is where a lane could have been given the wrong one.
func laneMCPURL(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, pod, home string) string {
	t.Helper()
	config := execOut(t, ctx, cs, cfg, pod, "cat", home+codexManagedConfig)
	inTable := false
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inTable = line == lanePairDBTable
			continue
		}
		if !inTable || !strings.HasPrefix(line, "url") {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	t.Fatalf("no %s url in %s:\n%s", lanePairDBTable, home+codexManagedConfig, config)
	return ""
}

// laneMCP is one JSON-RPC call to the sidecar, made from inside the pod because
// the database binds loopback and is reachable no other way. It returns the
// tool's own payload, or the refusal if there was one.
func laneMCP(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, pod, url, method string, params map[string]any) (map[string]any, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	// curl with an argv rather than a shell: the payload carries quotes.
	raw := execOut(t, ctx, cs, cfg, pod, "curl", "-sS", "-X", "POST",
		"-H", "content-type: application/json", "--data-binary", string(body), url)
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("%s %s: %v\nbody:\n%s", method, url, err, raw)
	}
	if envelope.Error != nil {
		return nil, envelope.Error.Message
	}
	if method != "tools/call" {
		return envelope.Result, ""
	}
	content, ok := envelope.Result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("%s: no content in %v", params["name"], envelope.Result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("%s: %v\ntext:\n%s", params["name"], err, text)
	}
	// A tool that refused answers in its own payload rather than as a JSON-RPC
	// error, so the caller gets both and decides which it wanted.
	if failure, bad := payload["error"].(string); bad {
		return payload, failure
	}
	return payload, ""
}

func laneCall(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, pod, url, name string, args map[string]any) map[string]any {
	t.Helper()
	payload, refusal := laneMCP(t, ctx, cs, cfg, pod, url, "tools/call",
		map[string]any{"name": name, "arguments": args})
	if refusal != "" {
		t.Fatalf("%s at %s: %s", name, url, refusal)
	}
	return payload
}

// lanePairWakeProbe hands the decision over and then watches both lanes, in one
// shell so that no exec round trip sits between the two.
//
// $1 is the JSON-RPC body, $2 the parent lane's MCP address, $3 the monitor
// lane's queue and $4 the parent's. It prints `SEND <reply>`, then:
//
//	MSTART  whether the monitor was ever seen mid-turn — a turn on that lane
//	        that only the sidecar can have started
//	BOTH    whether one sample caught *both* lanes mid-turn at once
//	MFIN    the monitor's finished-turn count
//	SAMPLES how many samples it took
//
// MSTART rather than MFIN is the wake signal, which the first run of this test
// got wrong. A turn here has no login to run against and the CLI does not fail
// fast; it hangs until the agent's 30-minute run timeout. So `finished` stays 0
// long after the wake has plainly happened, and waiting on it only ever times
// out. A lane that is mid-turn has been started, which is the whole claim.
//
// It polls with no sleep so that a fast turn, if the CLI ever gains one, is
// still caught, and stops as soon as there is nothing further to learn.
const lanePairWakeProbe = `set -u
body=$1; mcp=$2; mq=$3; pq=$4
printf 'SEND '
curl -sS -X POST -H 'content-type: application/json' --data-binary "$body" "$mcp"
printf '\n'
mstart=no; both=no; mfin=0; samples=0
end=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$end" ]; do
  samples=$(( samples + 1 ))
  m=$(curl -sS "$mq" 2>/dev/null) || m=''
  p=$(curl -sS "$pq" 2>/dev/null) || p=''
  [ -n "$m" ] || continue
  # Plain field reads, never jq's "//": that operator treats false as absent,
  # so '.idle // "true"' would report a busy lane as idle and this loop would
  # never see a started turn or an overlap at all.
  mi=$(printf '%s' "$m" | jq -r '.idle' 2>/dev/null)
  mf=$(printf '%s' "$m" | jq -r '.finished' 2>/dev/null)
  pi=$(printf '%s' "$p" | jq -r '.idle' 2>/dev/null)
  case "$mf" in ''|null) mf=0 ;; esac
  mfin=$mf
  if [ "$mi" = false ]; then
    mstart=yes
    if [ "$pi" = false ]; then both=yes; fi
  fi
  if [ "$both" = yes ] || [ "$mfin" -gt 0 ]; then break; fi
done
printf 'MSTART %s\nBOTH %s\nMFIN %s\nSAMPLES %s\n' "$mstart" "$both" "$mfin" "$samples"
`

// relaySendBody is the handover as a JSON-RPC request: the call the parent's
// model would make, built here because the probe runs it inside the pod.
func relaySendBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "relay_send", "arguments": map[string]any{
			"id": lanePairHandoverID, "request_id": "e2e-parent-send-1",
			"message": map[string]any{
				"kind": "decision", "subject": lanePairHandoverSubject,
				"body": lanePairHandoverBody,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// probeField reads one `KEY value` line the probe printed.
func probeField(t *testing.T, probe, key string) string {
	t.Helper()
	for _, line := range strings.Split(probe, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+" "); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("the probe printed no %s line:\n%s", key, probe)
	return ""
}

// probeHandover unwraps the relay_send reply the probe captured.
func probeHandover(t *testing.T, probe string) map[string]any {
	t.Helper()
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	raw := probeField(t, probe, "SEND")
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("relay_send reply: %v\nbody:\n%s", err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("relay_send: %s", envelope.Error.Message)
	}
	content, ok := envelope.Result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("relay_send returned no content: %v", envelope.Result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("relay_send payload: %v\ntext:\n%s", err, text)
	}
	if failure, bad := payload["error"].(string); bad {
		t.Fatalf("relay_send refused: %s", failure)
	}
	return payload
}

func sessionLanes(t *testing.T, id string) lanePage {
	t.Helper()
	resp, raw := doAs(t, "", http.MethodGet, "/api/v1/sessions/"+id+"/lanes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET lanes: %d %s", resp.StatusCode, raw)
	}
	var page lanePage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode lanes: %v body=%s", err, raw)
	}
	return page
}

// lanePage is GET /sessions/{id}/lanes on the wire.
type lanePage struct {
	Pod    string `json:"pod"`
	Paired bool   `json:"paired"`
	Lanes  []struct {
		Role  string `json:"role"`
		Pod   string `json:"pod"`
		Path  string `json:"path"`
		Ready bool   `json:"ready"`
	} `json:"lanes"`
}

func readLane(t *testing.T, id, lane string, offset int64) readResp {
	t.Helper()
	resp, raw := doAs(t, "", http.MethodPost, "/api/v1/sessions/"+id+"/read",
		map[string]any{"offset": offset, "lane": lane})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read lane %q: %d %s", lane, resp.StatusCode, raw)
	}
	var out readResp
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode read: %v body=%s", err, raw)
	}
	return out
}

// eventuallyLaneContains waits for one lane's scrollback to carry a phrase and
// returns the whole buffer. A turn writes its prompt as it begins, so this is
// how a lane says what it was asked to do.
func eventuallyLaneContains(t *testing.T, id, lane, phrase string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var last string
	for {
		last = readLane(t, id, lane, 0).Payload
		if strings.Contains(last, phrase) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s lane's buffer never carried %q; it holds:\n%s", lane, phrase, last)
		}
		time.Sleep(2 * time.Second)
	}
}

func writeLane(t *testing.T, id, lane, payload string) int {
	t.Helper()
	resp, _ := doAs(t, "", http.MethodPost, "/api/v1/sessions/"+id+"/write",
		map[string]any{"payload": payload, "lane": lane})
	return resp.StatusCode
}

// TestAuthPool9_ParentHandsOverAndTheWokenMonitorMovesTheWork is the pair's
// whole point on a real pod: the parent hands a decision over, the sidecar
// starts the monitor, the monitor writes the records the parent may not write,
// and the parent reads both the records and the outcome.
func TestAuthPool9_ParentHandsOverAndTheWokenMonitorMovesTheWork(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	s := createAs(t, "", map[string]any{"name": uniqueName(t), "workloadType": "codex-manager"})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// ---- the pod the ordinary create path made ---------------------------
	p := lanePairPodOf(t, cs, s.ID)
	if e, ok := containerEnv(p.workload, "MANAGER_MONITOR_LANE"); !ok || e.ValueFrom == nil ||
		e.ValueFrom.ConfigMapKeyRef == nil || e.ValueFrom.ConfigMapKeyRef.Key != "manager-monitor-lane" {
		t.Fatalf("workload MANAGER_MONITOR_LANE = %+v, want the codex ConfigMap key", e)
	}
	if e, ok := containerEnv(p.workload, "MANAGER_MONITOR_STATE_DIR"); !ok || e.Value != "/session/state-monitor" {
		t.Errorf("workload MANAGER_MONITOR_STATE_DIR = %+v, want the tree beside the archived one", e)
	}
	// The sidecar gets the lane to wake and the same opt-in, and nothing about
	// where that lane keeps its conversation — that is the workload's business.
	wake, ok := containerEnv(p.sidecar, "MANAGER_MONITOR_WAKE_URL")
	if !ok || !strings.HasPrefix(wake.Value, "http://127.0.0.1:") || !strings.HasSuffix(wake.Value, "/monitor/write") {
		t.Fatalf("sidecar MANAGER_MONITOR_WAKE_URL = %+v, want this pod's loopback monitor write", wake)
	}
	if _, ok := containerEnv(p.sidecar, "MANAGER_MONITOR_LANE"); !ok {
		t.Error("the sidecar did not get the pair's opt-in; it would store handovers and sweep for nothing")
	}
	if e, leaked := containerEnv(p.sidecar, "MANAGER_MONITOR_STATE_DIR"); leaked {
		t.Errorf("the sidecar received MANAGER_MONITOR_STATE_DIR = %+v", e)
	}
	monitorQueueURL := strings.TrimSuffix(wake.Value, "/write") + "/queue"
	parentQueueURL := strings.TrimSuffix(wake.Value, "/monitor/write") + "/queue"

	// ---- the pod's own answer about its lanes ----------------------------
	// Polled: the lanes answer from the pod, so this doubles as waiting for the
	// agent to have both of them serving.
	var lanes lanePage
	deadline := time.Now().Add(3 * time.Minute)
	for {
		lanes = sessionLanes(t, s.ID)
		ready := lanes.Paired && len(lanes.Lanes) == 2
		for _, l := range lanes.Lanes {
			ready = ready && l.Ready
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pod never reported two ready lanes: %+v", lanes)
		}
		time.Sleep(3 * time.Second)
	}
	if lanes.Pod != p.pod.Name {
		t.Errorf("lanes report pod %q, the session's pod is %q", lanes.Pod, p.pod.Name)
	}
	byRole := map[string]string{}
	for _, l := range lanes.Lanes {
		if l.Pod != p.pod.Name {
			t.Errorf("lane %s is in pod %q, not the session's %q — the pair must be one pod", l.Role, l.Pod, p.pod.Name)
		}
		byRole[l.Role] = l.Path
	}
	if byRole["parent"] != "/read" || byRole["monitor"] != "/monitor/read" {
		t.Errorf("lane paths = %v, want the parent on the agent's own path and the monitor under /monitor", byRole)
	}

	// ---- each lane's own MCP address, from its own managed config ---------
	parentMCP := laneMCPURL(t, ctx, cs, cfg, p.pod.Name, lanePairParentHome)
	monitorMCP := laneMCPURL(t, ctx, cs, cfg, p.pod.Name, lanePairMonitorHome)
	if parentMCP == monitorMCP {
		t.Fatalf("both lanes call the records from %s; the role is supposed to be in the address", parentMCP)
	}
	for lane, url := range map[string]string{"parent": parentMCP, "monitor": monitorMCP} {
		if !strings.Contains(url, "/session-db/mcp/lane/"+lane+"/") {
			t.Errorf("the %s lane's address is %q, want /session-db/mcp/lane/%s/<incarnation>", lane, url, lane)
		}
	}

	// ---- the surface each lane actually loaded ----------------------------
	// This is the role split as the model meets it, served by the sidecar that
	// is running in this pod rather than by a table in a test.
	surface := func(url string) (map[string]string, []string) {
		result, refusal := laneMCP(t, ctx, cs, cfg, p.pod.Name, url, "tools/list", map[string]any{})
		if refusal != "" {
			t.Fatalf("tools/list at %s: %s", url, refusal)
		}
		described, names := map[string]string{}, []string{}
		for _, tool := range result["tools"].([]any) {
			entry := tool.(map[string]any)
			name, _ := entry["name"].(string)
			description, _ := entry["description"].(string)
			described[name] = description
			names = append(names, name)
		}
		return described, names
	}
	parentSurface, parentNames := surface(parentMCP)
	monitorSurface, _ := surface(monitorMCP)
	for _, write := range []string{"task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch"} {
		if _, offered := parentSurface[write]; offered {
			t.Errorf("the parent lane in this pod is offered %s; the monitor owns writing the records", write)
		}
		if _, offered := monitorSurface[write]; !offered {
			t.Errorf("the monitor lane in this pod is not offered %s", write)
		}
	}
	for _, read := range []string{"task_get", "task_list", "turn_list"} {
		if _, offered := parentSurface[read]; !offered {
			t.Errorf("the parent lane cannot read %s; it reads the records directly", read)
		}
	}
	for _, relay := range []string{"relay_send", "relay_list", "relay_claim", "relay_ack", "relay_status"} {
		if _, offered := parentSurface[relay]; !offered {
			t.Errorf("the parent lane has no %s; it would have no way to get a change made", relay)
		}
	}
	// The guidance a parent needs in order to use any of this: that it reads
	// the records, that handing over runs the monitor, and where the answer
	// comes back. A lane has no instructions file — its brief is this text.
	for _, phrase := range []string{"yours to read", "is then started", "relay_status"} {
		if !strings.Contains(parentSurface["relay_send"], phrase) {
			t.Errorf("the parent's loaded relay_send description lacks %q:\n%s", phrase, parentSurface["relay_send"])
		}
	}
	if !strings.Contains(monitorSurface["relay_send"], "No turn starts for the parent") {
		t.Errorf("the monitor's loaded relay_send description does not say the parent is not started:\n%s",
			monitorSurface["relay_send"])
	}
	t.Logf("parent lane loaded %d tools: %s", len(parentNames), strings.Join(parentNames, " "))

	// ---- the person speaks to the parent, which keeps that lane busy ------
	// Two prompts, not a queue full of them: a turn here has no login to run
	// against and the CLI hangs rather than failing, so the first one holds the
	// parent's serial worker for as long as this test could ever need and the
	// second proves the queue behind it is the parent's own. Writing with no
	// lane at all is the person's chat, exactly as it was before the pair.
	for i := 0; i < 2; i++ {
		if status := writeLane(t, s.ID, "", fmt.Sprintf("%s (%d)", lanePairParentPrompt, i)); status != http.StatusOK {
			t.Fatalf("write %d to the parent lane: %d", i, status)
		}
	}

	// ---- hand over, and watch both lanes from inside the pod -------------
	// The send and the watching are one exec, and the queues are the only way
	// to see a lane mid-turn: /queue is loopback-only, so it cannot be read
	// from the test runner at all. Keeping the send in the same shell also
	// means the first sample lands milliseconds after the handover commits,
	// which costs nothing and would matter if a turn here were ever quick.
	//
	// It is not quick: with no login the CLI hangs rather than failing, so a
	// woken turn sits there until the agent's 30-minute run timeout. That is
	// what makes the overlap easy to catch here and what makes `finished` the
	// wrong thing to wait on (see lanePairWakeProbe).
	probe := execOut(t, ctx, cs, cfg, p.pod.Name, "sh", "-c", lanePairWakeProbe, "sh",
		string(relaySendBody(t)), parentMCP, monitorQueueURL, parentQueueURL)
	handover := probeHandover(t, probe)
	// Every reading is logged before anything fails on one of them, so a red run
	// says which of the three it was.
	t.Logf("wake probe: MSTART=%s BOTH=%s MFIN=%s SAMPLES=%s",
		probeField(t, probe, "MSTART"), probeField(t, probe, "BOTH"),
		probeField(t, probe, "MFIN"), probeField(t, probe, "SAMPLES"))
	if handover["state"] != "pending" {
		t.Fatalf("relay_send committed %v, want a pending message\nprobe:\n%s", handover["state"], probe)
	}
	// Nothing in this test wrote to the monitor lane and the control plane never
	// addressed it, so a turn running there is the sidecar's loopback hop and
	// nothing else. This is the clause the whole file exists for.
	if probeField(t, probe, "MSTART") != "yes" {
		t.Fatalf("the monitor lane never went mid-turn; nothing but the sidecar could start one\nprobe:\n%s", probe)
	}
	if probeField(t, probe, "BOTH") == "yes" {
		t.Log("saw the parent and monitor lanes mid-turn in the same sample: " +
			"two execution contexts in one pod, which a single serial worker cannot produce")
	} else {
		// Not a failure. The monitor still ran a turn the parent's queue never
		// carried, which is already two workers; what went unobserved is only
		// the instant of overlap.
		t.Logf("did not catch both lanes mid-turn in one sample (%s samples) — the simultaneity clause "+
			"stays with data-plane/cmd/agent/session_work_relay_wake_test.go, which holds a parent turn "+
			"open for the whole monitor round trip", probeField(t, probe, "SAMPLES"))
	}

	// ---- what the woken lane was actually given --------------------------
	// The strongest form of the same claim, on the product's own read surface
	// rather than a loopback diagnostic: a turn writes its prompt into the
	// lane's scrollback as it begins, so reading the monitor lane shows the
	// words the sidecar sent. Nobody else wrote to this lane.
	//
	// And it shows what the prompt does *not* carry. The wake is level-
	// triggered rather than a delivery, so the message's own words must be
	// absent — the database is where the work is, and a prompt carrying the
	// message could disagree with it.
	monitorBuffer := eventuallyLaneContains(t, s.ID, "monitor", lanePairWakePhrase)
	for _, leaked := range []string{lanePairHandoverBody, lanePairHandoverSubject} {
		if strings.Contains(monitorBuffer, leaked) {
			t.Errorf("the wake prompt carried the message's own words (%q); it is a wake, not a delivery",
				leaked)
		}
	}
	if strings.Contains(monitorBuffer, lanePairParentPrompt) {
		t.Error("the person's prompt to the parent appears in the monitor lane's buffer")
	}

	// The parent may not write the records even holding a list that says it can:
	// the guard behind the menu refuses and names the way through.
	_, refusal := laneMCP(t, ctx, cs, cfg, p.pod.Name, parentMCP, "tools/call", map[string]any{
		"name": "task_put", "arguments": map[string]any{
			"id": "e2e-parent-forbidden", "request_id": "e2e-parent-write-1", "expected_version": 0,
			"task": map[string]any{"session_id": nil, "title": "t", "description": "d",
				"completion_criteria": "c", "status": "planned"},
		},
	})
	if !strings.Contains(refusal, "relay_send") || !strings.Contains(refusal, "monitor") {
		t.Errorf("the parent's task_put was refused with %q, want the monitor's name and the way to hand it over", refusal)
	}

	// ---- the monitor does what it was handed, at its own address ----------
	open := laneCall(t, ctx, cs, cfg, p.pod.Name, monitorMCP, "relay_list",
		map[string]any{"box": "inbox", "open": true})
	messages := open["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("the monitor's open inbox has %d messages, want the one handed over: %v", len(messages), open)
	}
	message := messages[0].(map[string]any)
	if message["id"] != lanePairHandoverID {
		t.Fatalf("the monitor's inbox holds %v", message["id"])
	}
	claim := laneCall(t, ctx, cs, cfg, p.pod.Name, monitorMCP, "relay_claim", map[string]any{
		"id": message["id"], "request_id": "e2e-monitor-claim-1", "expected_version": message["version"],
	})
	token, _ := claim["claim_token"].(string)
	if token == "" {
		t.Fatalf("relay_claim gave no claim_token: %v", claim)
	}
	laneCall(t, ctx, cs, cfg, p.pod.Name, monitorMCP, "task_put", map[string]any{
		"id": lanePairTaskID, "request_id": "e2e-monitor-task-1", "expected_version": 0,
		"task": map[string]any{
			"session_id": nil, "title": "staged rollout",
			"description": "roll the release out in stages", "completion_criteria": "every stage is live",
			"status": "running",
		},
	})
	laneCall(t, ctx, cs, cfg, p.pod.Name, monitorMCP, "turn_append", map[string]any{
		"task_id": lanePairTaskID, "request_id": "e2e-monitor-turn-1",
		"expected_version": 1, "expected_turn_version": 1,
		"turn": map[string]any{"instruction": "stage 1"},
	})
	settled := laneCall(t, ctx, cs, cfg, p.pod.Name, monitorMCP, "relay_ack", map[string]any{
		"id": message["id"], "request_id": "e2e-monitor-ack-1", "claim_token": token,
		"outcome": "processed", "result": "recorded the choice on task e2e-rollout and started stage 1",
	})
	if settled["state"] != "processed" {
		t.Fatalf("the monitor settled the handover as %v", settled["state"])
	}

	// ---- the parent reads it back on its own read-only surface -----------
	record := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "task_get", map[string]any{"id": lanePairTaskID})
	task := record["task"].(map[string]any)
	if task["title"] != "staged rollout" || task["status"] != "running" {
		t.Fatalf("the parent does not see the record the monitor wrote: %v", record)
	}
	turns := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "turn_list", map[string]any{"task_id": lanePairTaskID})
	if len(turns["turns"].([]any)) != 1 {
		t.Fatalf("the monitor's turn is not on the task: %v", turns)
	}
	status := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "relay_status", map[string]any{"id": lanePairHandoverID})
	if status["settled"] != true || status["dealt_with"] != true {
		t.Fatalf("relay_status does not say the handover was dealt with: %v", status)
	}
	if result, _ := status["message"].(map[string]any)["result"].(string); !strings.Contains(result, lanePairTaskID) {
		t.Fatalf("the parent does not read the monitor's result: %q", result)
	}

	// ---- two conversations, two cursors ----------------------------------
	// Separation both ways, which is stronger than either half: the person's
	// words are on the parent lane and only there, the sidecar's wake is on the
	// monitor lane and only there, and the two cursors are counted apart.
	parentRead := readLane(t, s.ID, "parent", 0)
	monitorRead := readLane(t, s.ID, "monitor", 0)
	if !strings.Contains(parentRead.Payload, lanePairParentPrompt) {
		t.Errorf("the person's prompt is not in the parent lane's buffer:\n%s", parentRead.Payload)
	}
	if strings.Contains(parentRead.Payload, lanePairWakePhrase) {
		t.Error("the sidecar's wake prompt is in the parent lane's buffer; the parent is never woken")
	}
	if strings.Contains(monitorRead.Payload, lanePairParentPrompt) {
		t.Error("the person's prompt is in the monitor lane's buffer; the conversations are separate")
	}
	if !strings.Contains(monitorRead.Payload, lanePairWakePhrase) {
		t.Errorf("the wake is not in the monitor lane's buffer:\n%s", monitorRead.Payload)
	}
	// An offset means nothing across lanes, so a cursor from one must not be
	// read as a position in the other.
	if other := readLane(t, s.ID, "monitor", parentRead.NextOffset); other.NextOffset < parentRead.NextOffset &&
		strings.Contains(other.Payload, lanePairParentPrompt) {
		t.Error("the parent's cursor read the parent's own words out of the monitor lane")
	}
	t.Logf("parent cursor %d, monitor cursor %d", parentRead.NextOffset, monitorRead.NextOffset)
	if status := writeLane(t, s.ID, "Monitor", "a misspelt lane"); status != http.StatusBadRequest {
		t.Errorf("write with lane=%q = %d, want 400 — a misspelt lane must not quietly reach the chat", "Monitor", status)
	}
}
