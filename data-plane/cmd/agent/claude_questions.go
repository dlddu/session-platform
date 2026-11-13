// A claude-code turn's AskUserQuestion, answered by a person while the turn
// waits (docs/prd/claude-code-workload.md AC-E2, 「턴 중 질문」).
//
// The runner's canUseTool callback posts the model's questions here on
// loopback and polls for the answer; the control plane lists the pending
// questions and posts the person's answer on the pod IP. The turn keeps
// running while it waits, so the answer lands in the same conversation turn
// as the tool call that asked. A question lives only as long as its turn: the
// turn's end, a checkpoint that waits on the turn, or the runner giving up
// closes it.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// questionAskPath is the runner's loopback route: POST asks, GET
	// {id} polls, DELETE {id} gives up.
	questionAskPath = "/questions/ask"
	// questionsPath is the control plane's: GET lists what is pending, POST
	// {id}/answer answers.
	questionsPath = "/questions"

	maxQuestionBodyBytes = 64 << 10
	// The SDK's AskUserQuestion takes 1-4 questions of 2-4 options; the
	// platform enforces the same shape rather than trusting the runner.
	maxQuestionsPerAsk    = 4
	minQuestionOptions    = 2
	maxQuestionOptions    = 4
	maxQuestionTextRunes  = 2000
	maxQuestionLabelRunes = 200
	maxAnswerRunes        = 4000
	// maxPendingAsks bounds what one turn can leave open at once.
	maxPendingAsks = 4

	questionStatePending   = "pending"
	questionStateAnswered  = "answered"
	questionStateCancelled = "cancelled"
)

var (
	errQuestionNoTurn      = errors.New("no turn is running; a question belongs to the running turn")
	errQuestionClosed      = errors.New("the session is being frozen; nobody can answer now")
	errQuestionTooMany     = errors.New("too many questions are already waiting in this turn")
	errQuestionNotFound    = errors.New("no such pending question")
	errQuestionInvalid     = errors.New("invalid question")
	errQuestionBadAnswer   = errors.New("invalid answer")
	errQuestionUnsupported = errors.New("this workload does not ask questions")
)

type userQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type userQuestion struct {
	Question    string               `json:"question"`
	Header      string               `json:"header"`
	Options     []userQuestionOption `json:"options"`
	MultiSelect bool                 `json:"multiSelect"`
}

// questionAsker is an agentCLI whose turns may ask a person (claude-code).
type questionAsker interface {
	asksQuestions() bool
}

type pendingAsk struct {
	ID        string            `json:"id"`
	Turn      int               `json:"-"`
	Questions []userQuestion    `json:"questions"`
	AskedAt   time.Time         `json:"askedAt"`
	State     string            `json:"-"`
	Answers   map[string]string `json:"-"`
	Reason    string            `json:"-"`
}

// questionDesk holds the running turn's questions. It has its own lock so the
// HTTP handlers never wait on the workload's.
type questionDesk struct {
	mu   sync.Mutex
	asks map[string]*pendingAsk
}

func (d *questionDesk) add(ask *pendingAsk) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	open := 0
	for _, existing := range d.asks {
		if existing.Turn == ask.Turn && existing.State == questionStatePending {
			open++
		}
	}
	if open >= maxPendingAsks {
		return errQuestionTooMany
	}
	if d.asks == nil {
		d.asks = map[string]*pendingAsk{}
	}
	d.asks[ask.ID] = ask
	return nil
}

// status is what the runner's poll reads.
func (d *questionDesk) status(id string) (pendingAsk, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ask, ok := d.asks[id]
	if !ok {
		return pendingAsk{}, false
	}
	copied := *ask
	return copied, true
}

func (d *questionDesk) pending() []pendingAsk {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []pendingAsk{}
	for _, ask := range d.asks {
		if ask.State == questionStatePending {
			out = append(out, *ask)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AskedAt.Before(out[j].AskedAt) })
	return out
}

func (d *questionDesk) answer(id string, answers map[string]string) (pendingAsk, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ask, ok := d.asks[id]
	if !ok || ask.State != questionStatePending {
		return pendingAsk{}, errQuestionNotFound
	}
	if err := validateAnswers(ask.Questions, answers); err != nil {
		return pendingAsk{}, err
	}
	ask.State = questionStateAnswered
	ask.Answers = answers
	return *ask, nil
}

// cancel closes one pending question, reporting whether it was pending.
func (d *questionDesk) cancel(id, reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ask, ok := d.asks[id]
	if !ok || ask.State != questionStatePending {
		return false
	}
	ask.State = questionStateCancelled
	ask.Reason = reason
	return true
}

// cancelAll closes every pending question with reason; the runner reads it on
// its next poll.
func (d *questionDesk) cancelAll(reason string) []pendingAsk {
	d.mu.Lock()
	defer d.mu.Unlock()
	var closed []pendingAsk
	for _, ask := range d.asks {
		if ask.State == questionStatePending {
			ask.State = questionStateCancelled
			ask.Reason = reason
			closed = append(closed, *ask)
		}
	}
	return closed
}

// dropTurn forgets a finished turn's questions: nothing polls them any more.
func (d *questionDesk) dropTurn(turn int) []pendingAsk {
	d.mu.Lock()
	defer d.mu.Unlock()
	var open []pendingAsk
	for id, ask := range d.asks {
		if ask.Turn == turn {
			if ask.State == questionStatePending {
				open = append(open, *ask)
			}
			delete(d.asks, id)
		}
	}
	return open
}

func validateQuestions(questions []userQuestion) error {
	if len(questions) == 0 || len(questions) > maxQuestionsPerAsk {
		return fmt.Errorf("%w: ask 1-%d questions", errQuestionInvalid, maxQuestionsPerAsk)
	}
	seen := map[string]bool{}
	for i, q := range questions {
		if !boundedText(q.Question, maxQuestionTextRunes) {
			return fmt.Errorf("%w: question %d needs text of at most %d characters", errQuestionInvalid, i+1, maxQuestionTextRunes)
		}
		if seen[q.Question] {
			return fmt.Errorf("%w: question %d repeats an earlier question", errQuestionInvalid, i+1)
		}
		seen[q.Question] = true
		if q.Header != "" && !boundedText(q.Header, maxQuestionLabelRunes) {
			return fmt.Errorf("%w: question %d has an oversized header", errQuestionInvalid, i+1)
		}
		if len(q.Options) < minQuestionOptions || len(q.Options) > maxQuestionOptions {
			return fmt.Errorf("%w: question %d needs %d-%d options", errQuestionInvalid, i+1, minQuestionOptions, maxQuestionOptions)
		}
		for j, option := range q.Options {
			if !boundedText(option.Label, maxQuestionLabelRunes) || utf8.RuneCountInString(option.Description) > maxQuestionTextRunes {
				return fmt.Errorf("%w: question %d option %d needs a label", errQuestionInvalid, i+1, j+1)
			}
		}
	}
	return nil
}

// validateAnswers wants exactly one non-empty answer per question, keyed by
// the question's text — the shape AskUserQuestion's answers take. A choice
// outside the options is the person's own words, which the tool accepts.
func validateAnswers(questions []userQuestion, answers map[string]string) error {
	if len(answers) != len(questions) {
		return fmt.Errorf("%w: answer each of the %d questions", errQuestionBadAnswer, len(questions))
	}
	for _, q := range questions {
		answer, ok := answers[q.Question]
		if !ok {
			return fmt.Errorf("%w: missing an answer to %q", errQuestionBadAnswer, q.Question)
		}
		if !boundedText(answer, maxAnswerRunes) {
			return fmt.Errorf("%w: an answer must be 1-%d characters", errQuestionBadAnswer, maxAnswerRunes)
		}
	}
	return nil
}

func boundedText(s string, maxRunes int) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= maxRunes
}

func newQuestionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q_" + hex.EncodeToString(b[:]), nil
}

func (c *claudeWorkload) asksQuestions() bool {
	asker, ok := c.cli.(questionAsker)
	return ok && asker.asksQuestions()
}

// askQuestions opens a question for the running turn and leaves a line in the
// transcript, so a person reading the session sees what the turn waits on.
func (c *claudeWorkload) askQuestions(questions []userQuestion) (string, error) {
	if !c.asksQuestions() {
		return "", errQuestionUnsupported
	}
	if err := validateQuestions(questions); err != nil {
		return "", err
	}
	c.mu.Lock()
	switch {
	case !c.active:
		c.mu.Unlock()
		return "", errQuestionNoTurn
	case c.checkpointing:
		c.mu.Unlock()
		return "", errQuestionClosed
	}
	turn := c.turns
	c.mu.Unlock()
	id, err := newQuestionID()
	if err != nil {
		return "", err
	}
	if err := c.questions.add(&pendingAsk{
		ID: id, Turn: turn, Questions: questions, AskedAt: c.now().UTC(), State: questionStatePending,
	}); err != nil {
		return "", err
	}
	c.appendPlatformNotice(c.redactString(renderQuestionNotice(id, questions)))
	return id, nil
}

func (c *claudeWorkload) answerQuestion(id string, answers map[string]string) error {
	if !c.asksQuestions() {
		return errQuestionUnsupported
	}
	ask, err := c.questions.answer(id, answers)
	if err != nil {
		return err
	}
	c.appendPlatformNotice(c.redactString(renderAnswerNotice(ask)))
	return nil
}

// giveUpQuestion is the runner's own timeout: nobody answered in time.
func (c *claudeWorkload) giveUpQuestion(id string) {
	if c.questions.cancel(id, "nobody answered in time") {
		c.appendPlatformNotice(fmt.Sprintf("\n[session-platform: question %s went unanswered; the turn continues without it]\n", id))
	}
}

// closeQuestionsForCheckpoint lets a turn blocked on a person finish, so the
// checkpoint that waits on it is not held for the runner's whole wait.
func (c *claudeWorkload) closeQuestionsForCheckpoint() {
	for _, ask := range c.questions.cancelAll(errQuestionClosed.Error()) {
		c.appendPlatformNotice(fmt.Sprintf("\n[session-platform: question %s closed — the session is being frozen]\n", ask.ID))
	}
}

func (c *claudeWorkload) dropTurnQuestions(turn int) {
	c.questions.dropTurn(turn)
}

func renderQuestionNotice(id string, questions []userQuestion) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[session-platform: question %s — the turn waits for your answer in the workspace]\n", id)
	for _, q := range questions {
		labels := make([]string, 0, len(q.Options))
		for _, option := range q.Options {
			labels = append(labels, option.Label)
		}
		header := ""
		if q.Header != "" {
			header = "[" + questionLine(q.Header) + "] "
		}
		fmt.Fprintf(&b, "  ? %s%s (%s)\n", header, questionLine(q.Question), questionLine(strings.Join(labels, " / ")))
	}
	return b.String()
}

func renderAnswerNotice(ask pendingAsk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[session-platform: question %s answered]\n", ask.ID)
	for _, q := range ask.Questions {
		fmt.Fprintf(&b, "  → %s: %s\n", questionLine(q.Question), questionLine(ask.Answers[q.Question]))
	}
	return b.String()
}

// questionLine keeps model or person text on one transcript line.
func questionLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
}

func questionStatus(err error) int {
	switch {
	case errors.Is(err, errQuestionInvalid), errors.Is(err, errQuestionBadAnswer):
		return http.StatusBadRequest
	case errors.Is(err, errQuestionNotFound), errors.Is(err, errQuestionUnsupported):
		return http.StatusNotFound
	case errors.Is(err, errQuestionTooMany):
		return http.StatusTooManyRequests
	case errors.Is(err, errQuestionNoTurn), errors.Is(err, errQuestionClosed):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func decodeQuestionBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxQuestionBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", errQuestionInvalid, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the JSON body", errQuestionInvalid)
	}
	return nil
}

// fromThisPod reports whether a request came from a process in this pod:
// loopback, or one of the pod's own addresses (a shell command can reach the
// agent on the pod IP too).
func fromThisPod(r *http.Request) bool {
	if fromLoopback(r) {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return true
	}
	remote := net.ParseIP(host)
	if remote == nil {
		return true
	}
	addrs, err := localInterfaceAddrs()
	if err != nil {
		// Unable to tell: refuse rather than let the model answer itself.
		return true
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.Equal(remote) {
			return true
		}
	}
	return false
}

// localInterfaceAddrs is a seam for tests.
var localInterfaceAddrs = net.InterfaceAddrs

// registerQuestionRoutes adds the question routes. The runner's are loopback
// only; the answer refuses anything from inside the pod, so the model cannot
// answer its own question with a shell command — only the control plane can.
func registerQuestionRoutes(mux *http.ServeMux, c *claudeWorkload) {
	mux.HandleFunc("POST "+questionAskPath, func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "questions are asked by the session's own runner", http.StatusForbidden)
			return
		}
		var body struct {
			Questions []userQuestion `json:"questions"`
		}
		if err := decodeQuestionBody(w, r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := c.askQuestions(body.Questions)
		if err != nil {
			http.Error(w, err.Error(), questionStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("GET "+questionAskPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "questions are polled by the session's own runner", http.StatusForbidden)
			return
		}
		ask, ok := c.questions.status(r.PathValue("id"))
		if !ok {
			http.Error(w, errQuestionNotFound.Error(), http.StatusNotFound)
			return
		}
		out := map[string]any{"state": ask.State}
		switch ask.State {
		case questionStateAnswered:
			out["answers"] = ask.Answers
		case questionStateCancelled:
			out["reason"] = ask.Reason
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("DELETE "+questionAskPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "questions are withdrawn by the session's own runner", http.StatusForbidden)
			return
		}
		c.giveUpQuestion(r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+questionsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"questions": c.questions.pending()})
	})
	mux.HandleFunc("POST "+questionsPath+"/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		if fromThisPod(r) {
			http.Error(w, "a question is answered by a person through the control plane", http.StatusForbidden)
			return
		}
		var body struct {
			Answers map[string]string `json:"answers"`
		}
		if err := decodeQuestionBody(w, r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := c.answerQuestion(r.PathValue("id"), body.Answers); err != nil {
			http.Error(w, err.Error(), questionStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"answered"}`))
	})
}
