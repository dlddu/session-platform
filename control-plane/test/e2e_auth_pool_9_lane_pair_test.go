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
//     model. The prompt's wording is checked where it is built
//     (session_work_relay_wake_test.go); that a *turn* ran is checked here.
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
	lanePairHandoverID = "e2e-handover-1"
	lanePairTaskID     = "e2e-rollout"
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
// lane's queue and $4 the parent's. It prints `SEND <reply>`, then `BOTH`
// (whether one sample ever caught both lanes mid-turn), `MFIN` (the monitor's
// finished-turn count, which only the sidecar can have moved) and `SAMPLES`.
//
// It polls with no sleep on purpose: the whole observable window is one turn
// that fails for want of a login, so the loop has to be faster than that turn.
// It stops as soon as the monitor's count moves, which is when there is nothing
// further to see.
const lanePairWakeProbe = `set -u
body=$1; mcp=$2; mq=$3; pq=$4
printf 'SEND '
curl -sS -X POST -H 'content-type: application/json' --data-binary "$body" "$mcp"
printf '\n'
both=no; mfin=0; samples=0
end=$(( $(date +%s) + 90 ))
while [ "$(date +%s)" -lt "$end" ]; do
  samples=$(( samples + 1 ))
  m=$(curl -sS "$mq" 2>/dev/null) || m=''
  p=$(curl -sS "$pq" 2>/dev/null) || p=''
  [ -n "$m" ] || continue
  # Plain field reads, never jq's "//": that operator treats false as absent,
  # so '.idle // "true"' would report a busy lane as idle and this loop would
  # never see an overlap at all.
  mi=$(printf '%s' "$m" | jq -r '.idle' 2>/dev/null)
  mf=$(printf '%s' "$m" | jq -r '.finished' 2>/dev/null)
  pi=$(printf '%s' "$p" | jq -r '.idle' 2>/dev/null)
  case "$mf" in ''|null) mf=0 ;; esac
  mfin=$mf
  if [ "$mi" = false ] && [ "$pi" = false ]; then both=yes; fi
  if [ "$mfin" -gt 0 ]; then break; fi
done
printf 'BOTH %s\nMFIN %s\nSAMPLES %s\n' "$both" "$mfin" "$samples"
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
				"kind": "decision", "subject": "the person chose the staged rollout",
				"body": "Record the choice on the work and take the first step.",
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

	// ---- keep the parent busy, so the monitor's turn has to overlap it ----
	// A codex turn on this SUT has no login to run against and fails fast, so
	// one prompt would be over before the monitor started. A queue of them
	// holds the parent's single serial worker for long enough that the monitor
	// running at all is the two contexts running at once.
	for i := 0; i < 48; i++ {
		if status := writeLane(t, s.ID, "", fmt.Sprintf("hold the parent lane busy (%d)", i)); status != http.StatusOK {
			t.Fatalf("write %d to the parent lane: %d", i, status)
		}
	}

	// ---- hand over, and watch both lanes from inside the pod -------------
	// The send and the watching are one exec on purpose. A turn here has no
	// login to run against and fails in well under a second, so sampling the
	// queues from the test runner — a fresh exec round trip each time — would
	// reliably miss the window the wake opens. Issuing the handover and then
	// polling both /queue endpoints in the same in-pod loop samples at curl
	// speed instead, which is the only way the overlap is observable at all on
	// this SUT.
	probe := execOut(t, ctx, cs, cfg, p.pod.Name, "sh", "-c", lanePairWakeProbe, "sh",
		string(relaySendBody(t)), parentMCP, monitorQueueURL, parentQueueURL)
	handover := probeHandover(t, probe)
	if handover["state"] != "pending" {
		t.Fatalf("relay_send committed %v, want a pending message\nprobe:\n%s", handover["state"], probe)
	}
	// Nothing in this test wrote to the monitor lane and the control plane never
	// addressed it, so a finished turn there is the sidecar's loopback hop and
	// nothing else. This is the clause the whole file exists for.
	if probeField(t, probe, "MFIN") == "0" {
		t.Fatalf("the monitor lane never finished a turn; nothing but the sidecar could start one\nprobe:\n%s", probe)
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
	// The person's prompts went to the parent lane. The monitor's buffer is its
	// own, so the parent's words are not in it and the offsets do not mean the
	// same thing.
	parentRead := readLane(t, s.ID, "parent", 0)
	monitorRead := readLane(t, s.ID, "monitor", 0)
	if strings.Contains(monitorRead.Payload, "hold the parent lane busy") {
		t.Error("the parent's prompts are in the monitor lane's buffer; the conversations are supposed to be separate")
	}
	t.Logf("parent cursor %d, monitor cursor %d", parentRead.NextOffset, monitorRead.NextOffset)
	if status := writeLane(t, s.ID, "Monitor", "a misspelt lane"); status != http.StatusBadRequest {
		t.Errorf("write with lane=%q = %d, want 400 — a misspelt lane must not quietly reach the chat", "Monitor", status)
	}
}
