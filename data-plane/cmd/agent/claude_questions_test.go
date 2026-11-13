package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testQuestion = "Which database?"

func askBody(questions ...userQuestion) string {
	data, _ := json.Marshal(map[string]any{"questions": questions})
	return string(data)
}

func validQuestion() userQuestion {
	return userQuestion{
		Question: testQuestion,
		Header:   "DB",
		Options:  []userQuestionOption{{Label: "SQLite", Description: "a file"}, {Label: "Postgres", Description: "a server"}},
	}
}

// questionClient calls the agent's routes in process, from loopback (the
// runner) or from the pod IP (the control plane).
type questionClient struct {
	t       *testing.T
	handler http.Handler
}

func (q questionClient) do(method, path, body string, loopback bool) (int, string) {
	q.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if loopback {
		req.RemoteAddr = "127.0.0.1:50000"
	} else {
		req.RemoteAddr = "10.42.0.9:50000"
	}
	rec := httptest.NewRecorder()
	q.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func newQuestionWorkload(t *testing.T, runner *fakeClaudeRunner) (*claudeWorkload, questionClient) {
	t.Helper()
	// The control plane's address is not one of this machine's.
	saved := localInterfaceAddrs
	localInterfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
	t.Cleanup(func() { localInterfaceAddrs = saved })
	c, _ := newClaudeTestServer(t, runner, platformDefaultModel, false, []string{"sk-secret"})
	c.cli = claudeCLI{agentURL: "http://127.0.0.1:8090"}
	return c, questionClient{t: t, handler: routes(testLogger(), &agent{claude: c})}
}

func startBlockedTurn(t *testing.T, c *claudeWorkload, runner *fakeClaudeRunner, q questionClient) recordedClaudeRun {
	t.Helper()
	if code, body := q.do(http.MethodPost, "/write", "decide the database", false); code != http.StatusOK {
		t.Fatalf("write = %d %s", code, body)
	}
	return receiveClaudeRun(t, runner.started)
}

func askID(t *testing.T, q questionClient, body string) string {
	t.Helper()
	code, resp := q.do(http.MethodPost, questionAskPath, body, true)
	if code != http.StatusCreated {
		t.Fatalf("ask = %d %s", code, resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || !strings.HasPrefix(out.ID, "q_") {
		t.Fatalf("ask answer = %q", resp)
	}
	return out.ID
}

func pollState(t *testing.T, q questionClient, id string) (int, map[string]any) {
	t.Helper()
	code, resp := q.do(http.MethodGet, questionAskPath+"/"+id, "", true)
	var out map[string]any
	_ = json.Unmarshal([]byte(resp), &out)
	return code, out
}

// AC-E2: the turn's AskUserQuestion waits for a person; the control plane
// lists it and answers it, the runner's poll reads the answer, and the
// transcript shows both sides.
func TestClaudeQuestionWaitsForAPersonsAnswer(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newQuestionWorkload(t, runner)

	run := startBlockedTurn(t, c, runner, q)
	if !strings.Contains(strings.Join(run.argv, " "), "--questions-url http://127.0.0.1:8090 --") {
		t.Fatalf("argv = %v, want the agent's loopback URL for questions", run.argv)
	}

	question := validQuestion()
	question.Question = "Which database for sk-secret?"
	id := askID(t, q, askBody(question))

	code, resp := q.do(http.MethodGet, questionsPath, "", false)
	if code != http.StatusOK || !strings.Contains(resp, `"id":"`+id+`"`) || !strings.Contains(resp, `"label":"Postgres"`) {
		t.Fatalf("pending list = %d %s", code, resp)
	}
	if code, state := pollState(t, q, id); code != http.StatusOK || state["state"] != questionStatePending {
		t.Fatalf("poll before the answer = %d %v", code, state)
	}

	answer := `{"answers":{"Which database for sk-secret?":"Postgres"}}`
	if code, _ := q.do(http.MethodPost, questionsPath+"/"+id+"/answer", answer, true); code != http.StatusForbidden {
		t.Fatalf("an answer from inside the pod = %d, want 403: the model must not answer itself", code)
	}
	podIP := &net.IPNet{IP: net.ParseIP("10.42.0.9"), Mask: net.CIDRMask(24, 32)}
	localInterfaceAddrs = func() ([]net.Addr, error) { return []net.Addr{podIP}, nil }
	code, _ = q.do(http.MethodPost, questionsPath+"/"+id+"/answer", answer, false)
	localInterfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
	if code != http.StatusForbidden {
		t.Fatalf("an answer from the pod's own IP = %d, want 403", code)
	}
	if code, _ := q.do(http.MethodPost, questionsPath+"/"+id+"/answer", `{"answers":{}}`, false); code != http.StatusBadRequest {
		t.Fatalf("an answer missing a question = %d, want 400", code)
	}
	if code, _ := q.do(http.MethodPost, questionsPath+"/"+id+"/answer", `{"answers":{"Which database for sk-secret?":"  "}}`, false); code != http.StatusBadRequest {
		t.Fatalf("a blank answer = %d, want 400", code)
	}
	if code, resp := q.do(http.MethodPost, questionsPath+"/"+id+"/answer", answer, false); code != http.StatusOK {
		t.Fatalf("answer = %d %s", code, resp)
	}
	code, state := pollState(t, q, id)
	answers, _ := state["answers"].(map[string]any)
	if code != http.StatusOK || state["state"] != questionStateAnswered || answers["Which database for sk-secret?"] != "Postgres" {
		t.Fatalf("poll after the answer = %d %v", code, state)
	}
	if code, _ := q.do(http.MethodPost, questionsPath+"/"+id+"/answer", answer, false); code != http.StatusNotFound {
		t.Fatalf("a second answer = %d, want 404", code)
	}
	if _, resp := q.do(http.MethodGet, questionsPath, "", false); resp != "{\"questions\":[]}\n" {
		t.Fatalf("pending list after the answer = %s", resp)
	}

	payload, _ := c.out.Since(0)
	transcript := string(payload)
	for _, want := range []string{
		"[session-platform: question " + id + " — the turn waits for your answer in the workspace]",
		"  ? [DB] Which database for " + redactedLiteral + "? (SQLite / Postgres)",
		"[session-platform: question " + id + " answered]",
		"  → Which database for " + redactedLiteral + "?: Postgres",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript lacks %q:\n%s", want, transcript)
		}
	}
	if strings.Contains(transcript, "sk-secret") {
		t.Errorf("a credential reached the transcript: %s", transcript)
	}

	close(runner.release)
	waitClaudeIdle(t, c)
	if code, _ := pollState(t, q, id); code != http.StatusNotFound {
		t.Fatalf("poll after the turn ended = %d, want 404: a question lives only as long as its turn", code)
	}
}

// Only the running turn asks, only from inside the pod, and only in the
// shape AskUserQuestion takes.
func TestClaudeQuestionRefusals(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newQuestionWorkload(t, runner)

	if code, _ := q.do(http.MethodPost, questionAskPath, askBody(validQuestion()), true); code != http.StatusConflict {
		t.Fatalf("ask outside a turn = %d, want 409", code)
	}
	startBlockedTurn(t, c, runner, q)
	if code, _ := q.do(http.MethodPost, questionAskPath, askBody(validQuestion()), false); code != http.StatusForbidden {
		t.Fatalf("ask from the pod IP = %d, want 403", code)
	}
	if code, _ := q.do(http.MethodGet, questionAskPath+"/q_x", "", false); code != http.StatusForbidden {
		t.Fatalf("poll from the pod IP = %d, want 403", code)
	}
	one := validQuestion()
	one.Options = one.Options[:1]
	blank := validQuestion()
	blank.Question = " "
	five := []userQuestion{validQuestion(), validQuestion(), validQuestion(), validQuestion(), validQuestion()}
	for i := range five {
		five[i].Question += strings.Repeat("?", i)
	}
	for name, body := range map[string]string{
		"no questions":   askBody(),
		"one option":     askBody(one),
		"blank question": askBody(blank),
		"repeated":       askBody(validQuestion(), validQuestion()),
		"five questions": askBody(five...),
		"unknown field":  `{"questions":[],"extra":1}`,
		"not json":       `{`,
	} {
		if code, _ := q.do(http.MethodPost, questionAskPath, body, true); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	for i := 0; i < maxPendingAsks; i++ {
		question := validQuestion()
		question.Question += strings.Repeat("!", i)
		askID(t, q, askBody(question))
	}
	if code, _ := q.do(http.MethodPost, questionAskPath, askBody(validQuestion()), true); code != http.StatusTooManyRequests {
		t.Fatalf("ask past the per-turn bound = %d, want 429", code)
	}
	close(runner.release)
	waitClaudeIdle(t, c)
}

// A workload whose runner has nowhere to relay a question has no question
// routes to speak of.
func TestClaudeQuestionUnsupportedWithoutAnAgentURL(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, _ := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)
	q := questionClient{t: t, handler: routes(testLogger(), &agent{claude: c})}
	run := startBlockedTurn(t, c, runner, q)
	if slicesContain(run.argv, "--questions-url") {
		t.Fatalf("argv = %v, want no questions URL", run.argv)
	}
	if code, _ := q.do(http.MethodPost, questionAskPath, askBody(validQuestion()), true); code != http.StatusNotFound {
		t.Fatalf("ask = %d, want 404", code)
	}
	close(runner.release)
	waitClaudeIdle(t, c)
}

// The runner's own timeout withdraws a question, and a checkpoint closes the
// ones still open so the turn it waits on can finish.
func TestClaudeQuestionGiveUpAndCheckpointClose(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, q := newQuestionWorkload(t, runner)
	startBlockedTurn(t, c, runner, q)

	first := askID(t, q, askBody(validQuestion()))
	if code, _ := q.do(http.MethodDelete, questionAskPath+"/"+first, "", true); code != http.StatusNoContent {
		t.Fatalf("give up = %d", code)
	}
	if _, state := pollState(t, q, first); state["state"] != questionStateCancelled {
		t.Fatalf("withdrawn question = %v", state)
	}
	if code, _ := q.do(http.MethodPost, questionsPath+"/"+first+"/answer", `{"answers":{"Which database?":"SQLite"}}`, false); code != http.StatusNotFound {
		t.Fatalf("answer to a withdrawn question = %d, want 404", code)
	}

	second := askID(t, q, askBody(validQuestion()))
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := c.beginCheckpoint(ctx, "")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, state := pollState(t, q, second)
		if state["state"] == questionStateCancelled {
			if reason, _ := state["reason"].(string); !strings.Contains(reason, "frozen") {
				t.Fatalf("checkpoint close reason = %q", reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the checkpoint did not close the open question: %v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := q.do(http.MethodPost, questionAskPath, askBody(validQuestion()), true); code != http.StatusConflict {
		t.Fatalf("ask while a checkpoint waits = %d, want 409", code)
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatalf("checkpoint = %v", err)
	}
	c.abortCheckpoint()

	payload, _ := c.out.Since(0)
	for _, want := range []string{
		"[session-platform: question " + first + " went unanswered; the turn continues without it]",
		"[session-platform: question " + second + " closed — the session is being frozen]",
	} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("transcript lacks %q", want)
		}
	}
}
