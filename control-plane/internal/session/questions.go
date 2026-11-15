package session

import (
	"errors"
	"time"
)

// A claude-code turn's AskUserQuestion, waiting for a person's answer while
// the turn runs (docs/prd/claude-code-workload.md AC-E2, 「턴 중 질문」). The
// data plane agent holds the questions; the control plane only relays them.

// QuestionOption is one choice the model offered.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question is one question of an AskUserQuestion call.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

// PendingQuestion is one AskUserQuestion call the running turn waits on. It
// is answered as a whole: one answer per question, keyed by the question's
// text.
type PendingQuestion struct {
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
	AskedAt   time.Time  `json:"askedAt"`
}

// ErrQuestionNotFound is a question that is not waiting: never asked, already
// answered, or closed with its turn.
var ErrQuestionNotFound = errors.New("question not found or no longer waiting")
