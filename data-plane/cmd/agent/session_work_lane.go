// How the parent lane of a manager gets work done by the monitor lane
// (AC-N11): it hands over the person's request as written, is given a receipt
// at once, and asks that receipt later for the monitor turn's own answer.
//
// The two lanes are two execution contexts in one pod, each with its own
// conversation, queue and serial worker, so neither can speak into the other's
// turn. What the parent needs is not a mailbox — it is a call it does not have
// to wait for. Its own turn is a person's turn: a parent that blocked until the
// monitor had answered would have stopped being a conversation, which is the
// whole reason there are two lanes.
//
// So this is two tools and no storage of its own:
//
//	lane_ask     puts the request on the monitor lane's queue and returns the
//	             receipt its write issued. It does not wait for the turn.
//	lane_answer  resolves one receipt to that turn's state and, once there is
//	             one, that turn's own answer.
//
// The path is the one already in the pod. The sidecar is the process that holds
// the slot's database (AC-N6) and the only one both lanes call, and it reaches
// the monitor over the same loopback hop the scheduler already uses to write to
// a person's chat (AC-P2). No new listener, no control-plane hop, nothing
// reachable from outside the pod that was not reachable before.
//
// What keeps two answers from being confused is the receipt, and the receipt is
// the lane's own (turn_ticket.go): it names the run that issued it and the
// prompt of that run, it is only issued for a prompt that was actually queued,
// and it resolves to the bytes that one turn wrote. A receipt from a lane that
// has restarted answers `unknown_run` rather than the new run's first turn.
//
// What this deliberately does not do: redeliver. A request that was queued on a
// lane which then restarted is gone, and its receipt says so. Asking again is
// the parent's decision, made with the person in front of it — not a sweep's.
// The durable record of what was decided and what came of it is the work
// ledger, which the monitor writes and the parent reads (session_work.go);
// this is how a turn gets started, not where the result is kept.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	laneAskTool    = "lane_ask"
	laneAnswerTool = "lane_answer"

	// managerMonitorWriteURLEnv is the monitor lane's own write address, on
	// loopback, given to the sidecar. Unset is a sidecar with no lane to hand
	// anything to, which is every pod that does not run the pair. Keep in sync
	// with control-plane/internal/adapter/k8s (ManagerMonitorWriteURLEnvVar).
	managerMonitorWriteURLEnv = "MANAGER_MONITOR_WRITE_URL"

	// laneAskRequestMax bounds one request for the reason a task's text is
	// bounded: a request is a request rather than a transcript, and long
	// material belongs in the work ledger the monitor writes.
	laneAskRequestMax = 16384

	// laneCallTimeout bounds one loopback call. Both calls are meant to return
	// immediately — the write queues and the receipt is read from memory — so
	// this is a ceiling on a hop, not on a turn.
	laneCallTimeout = 10 * time.Second

	// laneAskIdempotencyKept bounds the request_id table. It is in memory for
	// the same reason the receipts are: a receipt only means anything to the
	// run that issued it, so remembering request_ids across a restart would
	// hand back receipts that can no longer be resolved.
	laneAskIdempotencyKept = 256
)

// monitorLaneClient is the monitor lane as the sidecar beside it sees it.
type monitorLaneClient struct {
	writeURL string
	turnURL  string
	client   *http.Client

	mu sync.Mutex
	// asked remembers which receipt each request_id was given, so a parent
	// whose answer was lost resends the same call and learns what it already
	// did instead of queueing a second turn. Keyed in arrival order so the
	// oldest can be dropped.
	asked map[string]askedRequest
	order []string
}

// askedRequest is one answered lane_ask, kept so a retry can be replayed.
type askedRequest struct {
	ticket  string
	request string
}

// monitorWriteURLFromEnv reads the lane's write address. Only a loopback
// address on this pod is one: these tools exist to reach the conversation next
// to this sidecar, and an address pointing anywhere else would be this sidecar
// starting turns in another session.
func monitorWriteURLFromEnv(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(managerMonitorWriteURLEnv))
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "http://127.0.0.1:") || !strings.HasSuffix(raw, managerLanePathPrefix+"/write") {
		return "", fmt.Errorf("%s must be an http URL on 127.0.0.1 ending in %s",
			managerMonitorWriteURLEnv, managerLanePathPrefix+"/write")
	}
	return raw, nil
}

func newMonitorLaneClient(writeURL string) *monitorLaneClient {
	return &monitorLaneClient{
		writeURL: writeURL,
		turnURL:  strings.TrimSuffix(writeURL, "/write") + agentTurnPath,
		client:   &http.Client{Timeout: laneCallTimeout},
		asked:    map[string]askedRequest{},
	}
}

// replay reports the receipt a request_id already got in this run.
func (c *monitorLaneClient) replay(requestID string) (askedRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prior, ok := c.asked[requestID]
	return prior, ok
}

func (c *monitorLaneClient) remember(requestID, request, ticket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.asked[requestID]; !seen {
		c.order = append(c.order, requestID)
	}
	c.asked[requestID] = askedRequest{ticket: ticket, request: request}
	for len(c.order) > laneAskIdempotencyKept {
		delete(c.asked, c.order[0])
		c.order = c.order[1:]
	}
}

// ask queues one request on the monitor lane and returns the receipt the lane
// issued for it. A non-2xx is an error rather than a receipt: there is no turn
// to ask about.
func (c *monitorLaneClient) ask(ctx context.Context, request string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.writeURL, strings.NewReader(request))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("the monitor lane did not take the request: %d %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var queued struct {
		Status string `json:"status"`
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&queued); err != nil {
		return "", fmt.Errorf("the monitor lane's answer could not be read: %w", err)
	}
	if strings.TrimSpace(queued.Ticket) == "" {
		return "", fmt.Errorf("the monitor lane took the request without issuing a receipt")
	}
	return queued.Ticket, nil
}

// answer resolves one receipt against the monitor lane.
func (c *monitorLaneClient) answer(ctx context.Context, ticket string) (map[string]any, error) {
	address := c.turnURL + "?ticket=" + url.QueryEscape(ticket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("the monitor lane would not answer that receipt: %d %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(&out); err != nil {
		return nil, fmt.Errorf("the monitor lane's answer could not be read: %w", err)
	}
	return out, nil
}

// laneToolDefinitionsFor is the handover surface for one lane. Only the parent
// gets it: the monitor answers inside the turn the parent started, and a tool
// that started a parent turn would put words in front of a person who had not
// spoken. A manager running a single lane gets nothing — a tool whose only
// possible answer is a refusal is one the model should not be shown.
func laneHandoverToolDefinitions(lane string) []map[string]any {
	if lane != laneParent {
		return nil
	}
	str := func() map[string]any { return map[string]any{"type": "string"} }
	return []map[string]any{
		{
			"name": laneAskTool,
			"description": "Hands the person's request, as they put it, to this manager's monitor lane and returns at once with a receipt. " +
				"It does not wait for the turn: you keep talking to the person, and you ask " + laneAnswerTool +
				" what came of it when you need the answer. The monitor is the lane that writes the work records; you read them. " +
				"request_id makes a resend safe — the same id with the same request returns the receipt that was issued the first time rather than starting a second turn, " +
				"and the same id with a different request is a conflict. A receipt belongs to the run of the lane that issued it: if that lane restarts, " +
				laneAnswerTool + " says so rather than answering about some other turn, and whether to ask again is yours to decide with the person.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"request": map[string]any{"type": "string",
					"description": "What the monitor is being asked to do, in the person's own words where they gave them (1..16384 characters). Long material belongs in a task's record, which this can name."},
				"request_id": str(),
			}, "required": []string{"request", "request_id"}, "additionalProperties": false},
		},
		{
			"name": laneAnswerTool,
			"description": "What came of one request you handed over, by its receipt: queued (the monitor has not started it), running (started, with whatever it has written so far), " +
				"or done with that turn's own answer and its end status. It is the answer of that turn and no other. " +
				"Three refusals are answers too — unknown_run means the monitor lane restarted and the request went with it, absent means no such receipt was ever issued, " +
				"and expired means it was issued too long ago to still be held. Asking changes nothing and starts nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"ticket": str()},
				"required": []string{"ticket"}, "additionalProperties": false},
		},
	}
}

var laneHandoverTools = []string{laneAskTool, laneAnswerTool}

func isLaneHandoverTool(name string) bool { return containsString(laneHandoverTools, name) }

// callLaneHandover runs lane_ask or lane_answer. The refusals are deliberately
// specific: a caller that cannot use these has to learn why, not that its
// arguments were wrong.
func (s *sessionDB) callLaneHandover(ctx context.Context, caller workCaller, name string, raw json.RawMessage) (any, *jsonRPCError) {
	if caller.lane != laneParent {
		if caller.lane == laneMonitor {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: name +
				" is the parent lane's: you are the monitor, and your answer is the turn you are in — the parent reads it with " +
				laneAnswerTool + " on the receipt it already holds. Record what lasts in the work records."}
		}
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: name +
			" hands work to a manager's monitor lane; this session database is serving a single lane, so there is no other lane to hand anything to"}
	}
	if s.monitorLane == nil {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: name +
			" has no monitor lane address on this sidecar, so there is nothing it could reach"}
	}
	var args struct {
		Request   string `json:"request"`
		RequestID string `json:"request_id"`
		Ticket    string `json:"ticket"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if len(raw) > 0 {
		if err := decoder.Decode(&args); err != nil {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: name + ": " + err.Error()}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, laneCallTimeout)
	defer cancel()

	if name == laneAnswerTool {
		if strings.TrimSpace(args.Ticket) == "" {
			return sessionDBToolResult(map[string]string{"error": "ticket required — it is what names the turn you asked for"}, true), nil
		}
		answer, err := s.monitorLane.answer(ctx, args.Ticket)
		if err != nil {
			return sessionDBToolResult(map[string]string{"error": err.Error()}, true), nil
		}
		answer["lane"] = laneMonitor
		return sessionDBToolResult(answer, false), nil
	}

	request := strings.TrimSpace(args.Request)
	switch {
	case request == "":
		return sessionDBToolResult(map[string]string{"error": "request must not be empty"}, true), nil
	case len(args.Request) > laneAskRequestMax:
		return sessionDBToolResult(map[string]string{
			"error": fmt.Sprintf("request must be at most %d characters — long material belongs in a task's record", laneAskRequestMax),
		}, true), nil
	case !workID.MatchString(args.RequestID):
		return sessionDBToolResult(map[string]string{
			"error": "request_id must be 1..128 letters, digits, _ or -",
		}, true), nil
	}
	// The receipt a retry gets back is the one already issued, so a lost answer
	// costs a repeated call and never a second turn.
	if prior, ok := s.monitorLane.replay(args.RequestID); ok {
		if prior.request != request {
			return sessionDBToolResult(map[string]string{
				"error": "conflict: that request_id was used for a different request — use a new id for new words",
			}, true), nil
		}
		return sessionDBToolResult(map[string]any{
			"ticket": prior.ticket, "lane": laneMonitor, "state": turnTicketQueued, "replayed": true,
		}, false), nil
	}
	ticket, err := s.monitorLane.ask(ctx, request)
	if err != nil {
		// No receipt is issued for a request the lane did not take, so there is
		// nothing dangling for the caller to ask about later.
		return sessionDBToolResult(map[string]string{"error": err.Error()}, true), nil
	}
	s.monitorLane.remember(args.RequestID, request, ticket)
	return sessionDBToolResult(map[string]any{
		"ticket": ticket, "lane": laneMonitor, "state": turnTicketQueued, "replayed": false,
	}, false), nil
}
