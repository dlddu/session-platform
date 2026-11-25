//go:build e2e

// 검증 시나리오: agent-auth-pool.md#시나리오 9
//
// The manager pair on a real pod (AC-N11): one pod that the ordinary create
// path built, its workload container and the session-DB sidecar that was
// already there, and the parent handing the person's request straight to the
// monitor lane across them.
//
// The shape being checked. The parent does not wait: it hands the request over
// through the sidecar, is given a receipt at once, and carries on talking to
// the person. Later it asks that receipt what came of it. So the two things
// worth proving on a real pod are that the request reaches the other lane *as
// written*, and that the two lanes are then running at the same time — which a
// single serial worker cannot do.
//
// Why this file exists at all. Every other check of this is in-process:
// data-plane/cmd/agent/session_work_lane_test.go stands two lane workloads
// behind one httptest port with a real database, but it stands them itself.
// What it cannot show is that the *deployment* produces that shape — that the
// ConfigMap opt-in reaches both containers, that the monitor lane's state tree
// exists on the pod's volume, that the lane MCP address the platform renders
// into each lane's managed config is the one the sidecar answers, and that the
// sidecar's loopback hop crosses a real container boundary onto a real lane's
// queue. Those are the clauses the unit test is structurally unable to buy.
//
// What stands in for the model. There is no ChatGPT login on this SUT (AC-J3: a
// codex session signs itself in, and that is outside e2e), so neither lane can
// run a model turn that decides anything — and with no login the CLI hangs
// rather than answering, until the agent's 30-minute run timeout. So:
//
//   - The parent's *decisions* are scripted: this test makes the calls a parent
//     model would make, at the parent lane's own MCP address.
//   - The monitor's ledger writes are scripted the same way, at the monitor's
//     address, because its model cannot run them.
//   - Everything between those calls is the product — the addresses, the
//     sidecar, the handover hop, the receipt, the role split.
//
// What this file deliberately does NOT assert, and why:
//
//   - a receipt reaching `done` with the model's real answer. That needs a turn
//     that finishes, which needs a login. The receipt is checked here up to
//     `running` on the turn the handover started; that a finished receipt
//     carries its own turn's words and no other turn's is owned by
//     data-plane/cmd/agent/turn_ticket_test.go, where turns complete.
//   - that a misspelt opt-in fails the pod's start. That needs a second SUT
//     whose ConfigMap says something else — a deployment variant this overlay
//     does not have. Still owned by managerPairOptIn's unit test.
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
	// The request the parent hands over, written so that finding it verbatim in
	// the monitor lane's buffer means something: the handover carries the
	// person's words, not a notice that something is waiting.
	lanePairRequest = "Record the staged rollout on the work and take the first step (e2e-request-marker)."
	// What the person says to the parent. Distinct from everything else here so
	// its absence from the monitor's buffer means something.
	lanePairParentPrompt = "e2e-parent-prompt-keep-this-lane-busy"
	lanePairTaskID       = "e2e-rollout"
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

// lanePairBothBusyProbe reports whether both lanes are mid-turn. /queue is
// loopback-only, so it can only be read from inside the pod. $1 is the monitor
// lane's queue and $2 the parent's; it prints `BOTH yes|no` and each lane's
// idle flag so a red run says which one was not running.
//
// It samples for a few seconds rather than once, because the monitor's turn
// starts a moment after the handover commits. It does not need to be fast: with
// no login a turn here runs until the agent's 30-minute timeout, so once both
// are up they stay up.
const lanePairBothBusyProbe = `set -u
mq=$1; pq=$2
both=no; mi=unknown; pi=unknown
end=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "$end" ]; do
  m=$(curl -sS "$mq" 2>/dev/null) || m=''
  p=$(curl -sS "$pq" 2>/dev/null) || p=''
  [ -n "$m" ] || continue
  # Plain field reads, never jq's "//": that operator treats false as absent,
  # so '.idle // "true"' would report a busy lane as idle.
  mi=$(printf '%s' "$m" | jq -r '.idle' 2>/dev/null)
  pi=$(printf '%s' "$p" | jq -r '.idle' 2>/dev/null)
  if [ "$mi" = false ] && [ "$pi" = false ]; then both=yes; break; fi
  sleep 1
done
printf 'BOTH %s\nMONITOR_IDLE %s\nPARENT_IDLE %s\n' "$both" "$mi" "$pi"
`

// probeField reads one `KEY value` line a probe printed.
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

// TestAuthPool9_ParentHandsTheRequestToTheMonitorAndKeepsTalking is the design's
// point on a real pod: the parent hands the person's request straight over, gets
// a receipt without waiting, and both lanes run at once.
func TestAuthPool9_ParentHandsTheRequestToTheMonitorAndKeepsTalking(t *testing.T) {
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
	// The sidecar gets the lane to hand work to and the same opt-in, and nothing
	// about where that lane keeps its conversation — that is the workload's.
	write, ok := containerEnv(p.sidecar, "MANAGER_MONITOR_WRITE_URL")
	if !ok || !strings.HasPrefix(write.Value, "http://127.0.0.1:") || !strings.HasSuffix(write.Value, "/monitor/write") {
		t.Fatalf("sidecar MANAGER_MONITOR_WRITE_URL = %+v, want this pod's loopback monitor write", write)
	}
	if _, ok := containerEnv(p.sidecar, "MANAGER_MONITOR_LANE"); !ok {
		t.Error("the sidecar did not get the pair's opt-in; it would have no second lane to hand work to")
	}
	if e, leaked := containerEnv(p.sidecar, "MANAGER_MONITOR_STATE_DIR"); leaked {
		t.Errorf("the sidecar received MANAGER_MONITOR_STATE_DIR = %+v", e)
	}
	monitorQueueURL := strings.TrimSuffix(write.Value, "/write") + "/queue"
	parentQueueURL := strings.TrimSuffix(write.Value, "/monitor/write") + "/queue"

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
	for _, name := range []string{"task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch"} {
		if _, offered := parentSurface[name]; offered {
			t.Errorf("the parent lane in this pod is offered %s; the monitor owns writing the records", name)
		}
		if _, offered := monitorSurface[name]; !offered {
			t.Errorf("the monitor lane in this pod is not offered %s", name)
		}
	}
	for _, name := range []string{"task_get", "task_list", "turn_list"} {
		if _, offered := parentSurface[name]; !offered {
			t.Errorf("the parent lane cannot read %s; it reads the records directly", name)
		}
	}
	// The handover is the parent's alone: the monitor's answer is the turn it is
	// already in, so it has nothing to hand over and no receipt to resolve.
	for _, name := range []string{"lane_ask", "lane_answer"} {
		if _, offered := parentSurface[name]; !offered {
			t.Errorf("the parent lane has no %s; it would have no way to get work done", name)
		}
		if _, offered := monitorSurface[name]; offered {
			t.Errorf("the monitor lane is offered %s; it does not start the parent's turns", name)
		}
	}
	// The guidance a parent needs in order to use any of this. A lane gets no
	// instructions file — its whole brief is this text.
	for _, phrase := range []string{"returns at once with a receipt", "does not wait", "lane_answer"} {
		if !strings.Contains(parentSurface["lane_ask"], phrase) {
			t.Errorf("the parent's loaded lane_ask description lacks %q:\n%s", phrase, parentSurface["lane_ask"])
		}
	}
	t.Logf("parent lane loaded %d tools: %s", len(parentNames), strings.Join(parentNames, " "))

	// ---- the person speaks to the parent, which keeps that lane busy ------
	// With no login the turn hangs rather than failing, which is what holds the
	// parent's serial worker for the rest of this test.
	for i := 0; i < 2; i++ {
		if status := writeLane(t, s.ID, "", fmt.Sprintf("%s (%d)", lanePairParentPrompt, i)); status != http.StatusOK {
			t.Fatalf("write %d to the parent lane: %d", i, status)
		}
	}

	// ---- the parent hands the request over and is not kept waiting --------
	// Scripted: this is the call the parent's model would make. Everything it
	// travels through is the product.
	asked := time.Now()
	ask := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "lane_ask", map[string]any{
		"request": lanePairRequest, "request_id": "e2e-parent-ask-1",
	})
	handed := time.Since(asked)
	ticket, _ := ask["ticket"].(string)
	if ticket == "" {
		t.Fatalf("lane_ask returned no receipt: %v", ask)
	}
	if ask["lane"] != "monitor" || ask["replayed"] != false {
		t.Errorf("lane_ask = %v, want a first handover to the monitor lane", ask)
	}
	// It returned without waiting for the turn — which on this SUT would have
	// been a 30-minute wait, so any ordinary duration proves the point.
	t.Logf("lane_ask returned a receipt in %s (the turn it started runs for far longer)", handed.Round(time.Millisecond))
	if handed > time.Minute {
		t.Errorf("lane_ask took %s; it is supposed to return as soon as the prompt is queued", handed)
	}

	// A retry is not a second turn: the same id with the same words replays the
	// receipt already issued.
	retry := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "lane_ask", map[string]any{
		"request": lanePairRequest, "request_id": "e2e-parent-ask-1",
	})
	if retry["ticket"] != ticket || retry["replayed"] != true {
		t.Errorf("the retry = %v, want the first receipt (%s) replayed", retry, ticket)
	}

	// ---- the request reached the other lane, as written -------------------
	// The strongest form of the claim, on the product's own read surface: a turn
	// writes its prompt into its lane's scrollback as it begins, so reading the
	// monitor lane shows the words the parent handed over. Nothing in this test
	// wrote to that lane and the control plane never addressed it.
	monitorBuffer := eventuallyLaneContains(t, s.ID, "monitor", lanePairRequest)
	if strings.Contains(monitorBuffer, lanePairParentPrompt) {
		t.Error("the person's prompt to the parent appears in the monitor lane's buffer")
	}

	// ---- both lanes mid-turn at once --------------------------------------
	probe := execOut(t, ctx, cs, cfg, p.pod.Name, "sh", "-c", lanePairBothBusyProbe, "sh",
		monitorQueueURL, parentQueueURL)
	t.Logf("busy probe: BOTH=%s MONITOR_IDLE=%s PARENT_IDLE=%s",
		probeField(t, probe, "BOTH"), probeField(t, probe, "MONITOR_IDLE"), probeField(t, probe, "PARENT_IDLE"))
	if probeField(t, probe, "BOTH") != "yes" {
		t.Fatalf("the two lanes were never mid-turn at the same time\nprobe:\n%s", probe)
	}

	// ---- the receipt names that turn, and refuses what it cannot name ------
	answer := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "lane_answer", map[string]any{"ticket": ticket})
	if state, _ := answer["state"].(string); state != "running" && state != "done" {
		t.Fatalf("the receipt resolves to %q, want the turn the handover started: %v", state, answer)
	}
	if answer["lane"] != "monitor" {
		t.Errorf("the answer does not say which lane it came from: %v", answer)
	}
	if seq, _ := answer["seq"].(float64); seq < 1 {
		t.Errorf("a started turn has no seq: %v", answer)
	}
	// A receipt from a run of the lane that is gone is told so, rather than
	// being resolved against this run's turn of the same number.
	stale := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "lane_answer",
		map[string]any{"ticket": "someearlierrun-1"})
	if stale["state"] != "unknown_run" {
		t.Errorf("a receipt from another run = %v, want unknown_run", stale)
	}
	if stale["response"] != nil {
		t.Errorf("a receipt from another run resolved to a turn's words: %v", stale)
	}
	// And one this run never issued is absent rather than someone else's turn.
	_, ticketNumber, _ := strings.Cut(ticket, "-")
	absent := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "lane_answer",
		map[string]any{"ticket": strings.TrimSuffix(ticket, ticketNumber) + "999999"})
	if absent["state"] != "absent" {
		t.Errorf("a receipt never issued = %v, want absent", absent)
	}

	// ---- the parent still may not write the records -----------------------
	_, refusal := laneMCP(t, ctx, cs, cfg, p.pod.Name, parentMCP, "tools/call", map[string]any{
		"name": "task_put", "arguments": map[string]any{
			"id": "e2e-parent-forbidden", "request_id": "e2e-parent-write-1", "expected_version": 0,
			"task": map[string]any{"session_id": nil, "title": "t", "description": "d",
				"completion_criteria": "c", "status": "planned"},
		},
	})
	for _, want := range []string{"monitor", "lane_ask", "lane_answer"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the parent's task_put refusal does not name %q: %s", want, refusal)
		}
	}

	// ---- what lasts is in the records: the monitor writes, the parent reads -
	// Scripted at the monitor's own address, because its model cannot run here.
	// The addresses, the role split and the database are the product's.
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
	record := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "task_get", map[string]any{"id": lanePairTaskID})
	task := record["task"].(map[string]any)
	if task["title"] != "staged rollout" || task["status"] != "running" {
		t.Fatalf("the parent does not see the record the monitor wrote: %v", record)
	}
	turns := laneCall(t, ctx, cs, cfg, p.pod.Name, parentMCP, "turn_list", map[string]any{"task_id": lanePairTaskID})
	if len(turns["turns"].([]any)) != 1 {
		t.Fatalf("the monitor's turn is not on the task: %v", turns)
	}

	// ---- two conversations, two cursors ----------------------------------
	parentRead := readLane(t, s.ID, "parent", 0)
	monitorRead := readLane(t, s.ID, "monitor", 0)
	if !strings.Contains(parentRead.Payload, lanePairParentPrompt) {
		t.Errorf("the person's prompt is not in the parent lane's buffer:\n%s", parentRead.Payload)
	}
	if strings.Contains(parentRead.Payload, lanePairRequest) {
		t.Error("the handed-over request is in the parent lane's buffer; it was queued on the monitor")
	}
	if strings.Contains(monitorRead.Payload, lanePairParentPrompt) {
		t.Error("the person's prompt is in the monitor lane's buffer; the conversations are separate")
	}
	t.Logf("parent cursor %d, monitor cursor %d", parentRead.NextOffset, monitorRead.NextOffset)
	if status := writeLane(t, s.ID, "Monitor", "a misspelt lane"); status != http.StatusBadRequest {
		t.Errorf("write with lane=%q = %d, want 400 — a misspelt lane must not quietly reach the chat", "Monitor", status)
	}
}
