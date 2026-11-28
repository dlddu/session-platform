// `review` is the one status a person's judgement puts a task in, and the three
// moves into and out of it (AC-N13).
//
// What this replaced: the domain carried two statuses for "not moving" —
// `blocked` and `review` — and nothing said which was which. In practice both
// meant the same thing, that the work had reached something only the person
// could settle, and having two names for it cost twice: a reader had to ask for
// both to see everything waiting on them, and a writer had to guess which one
// the other side would filter on. So there is one: `review`. Nothing is renamed
// into existence beside it — no "waiting", no "held", no second chip — because a
// second name for one fact is the problem rather than the fix.
//
// `blocked` does not become a different status and it does not disappear from
// the slots that already hold it. It becomes an **alias on input** and a
// **classification on output**:
//
//   - On input (a status filter, a task_put) `blocked` is accepted and read as
//     `review`. An older writer and a filter somebody saved last month keep
//     working, and nothing new is stored under the word.
//   - On output, a row an older image wrote as `blocked` is resolved to the
//     status it actually is, from evidence that is already there. It is never
//     answered as `blocked`, never answered as `unknown`, and never left out of
//     a page — the stored byte and what decided travel with it
//     (`stored_status`, `status_basis`) so nobody has to take the resolution on
//     trust.
//
// The resolution is deliberately not a blanket mapping. A slot holds two
// different things under the one old word, and flattening them either invents a
// decision for a person who has none to make, or quietly resumes work nobody
// approved:
//
//   - **A person has to decide** → `review`. This is the default and the
//     ambiguous case: a row whose bookkeeping says nothing at all is a row this
//     code cannot explain, and the safe reading of "cannot explain" is that
//     somebody should look.
//   - **A pre-approved technical wait is running** → `running`, and only on a
//     structured, explicit statement in the parent's own bookkeeping that names
//     the three facts such a wait has: who owns it, when it is due, and what
//     retry is allowed (`details.technical_wait`). A CI run in flight is not a
//     question for anybody, and parking it in `review` would put a decision in
//     front of a person that does not exist.
//
// Prose does not count, a string that reads like an approval does not count,
// and a malformed statement is reported rather than guessed at — the same rule
// the cancellation backfill follows, for the same reason: this runs over work a
// person already has.
//
// The three moves. They are the **complete** path into and out of `review` —
// the one that carries the evidence — and not a fence around a bare status
// write: the agreed contract is that a task record is a full replacement under
// version CAS with no transition constraint on the status value, because the
// correction it names (a task written `completed` when nobody had decided
// anything, put back to `review`) depends on that (session_work_tasks.go). So
// task_put still writes any status in the domain; what these add is the record
// that makes the status mean something.
//
//	task_review_open      -> review, from whatever the task is at. Records, in
//	                      one transaction, the exact target, the scope, the
//	                      official approval route, the question a person can
//	                      answer and the basis for asking — and asks it on the
//	                      turn the parent is already in. Asking the same thing
//	                      twice is not a second review.
//	task_review_resume    review -> running. Only with the confirmation named,
//	                      and only when there is work left. An approval that is
//	                      not applied leaves the task in review.
//	task_review_complete  review -> completed. Only when the result was
//	                      accepted, the task's own completion criteria are
//	                      echoed back verbatim, no request is still open and
//	                      nothing is left.
//
// None of the three touches a turn's `request`, `instruction`, `response` or
// `explanation`, and none closes a turn: the exchanges are what happened, and a
// status move does not rewrite how the task got here. What a review replaces is
// kept — the previous review, and the status a legacy row carried, go onto
// `details.review_history` rather than over the top of each other.
//
// What this does **not** claim: the platform cannot see whether an approval was
// really applied out there. What it can do is refuse a resume that names no
// confirmation, keep the named confirmation beside the task, and never move the
// task itself on the strength of a guess. That limit is stated here rather than
// papered over, because a gate that pretends to verify is worse than one that
// says what it checks.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// workStatusReview is the one status a person's judgement puts a task in,
	// and workStatusBlockedLegacy the word an older image wrote for the same
	// thing. The second is an alias on input and a classification on output; it
	// is in no domain list, so nothing offers it and nothing stores it.
	workStatusReview        = "review"
	workStatusBlockedLegacy = "blocked"
	workStatusRunning       = "running"
	workStatusPlanned       = "planned"
	workStatusCompleted     = "completed"

	// The three moves into and out of review. They are named for what they do
	// to the task rather than for the status they write, because the status is
	// the easy half — the checks are the point.
	taskReviewOpenTool     = "task_review_open"
	taskReviewResumeTool   = "task_review_resume"
	taskReviewCompleteTool = "task_review_complete"

	// workReviewKey is where an open review lives in the parent's own
	// bookkeeping, and workReviewHistoryKey where the ones before it went. Both
	// are inside `details`, which is the parent's record beside the task — the
	// agreed task gains no column (session_work_parent.go).
	workReviewKey        = "review"
	workReviewHistoryKey = "review_history"
	// workTechnicalWaitKey is where a pre-approved technical wait is stated. It
	// is the one statement that resolves a legacy `blocked` row to `running`,
	// so its shape is checked rather than trusted.
	workTechnicalWaitKey = "technical_wait"

	// workReviewTextMax bounds one review's fields for the reason a task's text
	// is bounded: the slot's storage budget is shared with the session's own
	// archive, so a review is a review and not an attachment. The whole
	// bookkeeping record is bounded again at workLegacyBytesMax.
	workReviewTextMax = 4096
	// workReviewHistoryKept bounds how many closed reviews are carried. The
	// oldest go first, and the count is bounded rather than the bytes so that
	// what is kept is whole entries — half a record of why a task was held is
	// worse than one fewer.
	workReviewHistoryKept = 32
)

// workReviewInput is one decision a person has to make, as the monitor records
// it. Every field is required: a review that names no target, no scope or no
// route is not something a person can act on, and a review with no question is
// not something they can answer.
type workReviewInput struct {
	// Target is exactly what the decision is about — the repository, the pull
	// request, the resource. Not a category.
	Target string `json:"target"`
	// Scope is how far the decision reaches, so that granting it is not
	// open-ended.
	Scope string `json:"scope"`
	// Route is the official way this is approved. It is recorded because a
	// question without one leaves the person to find the door themselves.
	Route string `json:"route"`
	// Question is what the person is asked, in the monitor's own words, and it
	// is stored verbatim. Nothing here rewrites it or dresses it up.
	Question string `json:"question"`
	// Basis is why this is being asked — what was observed that made the
	// decision necessary. It is what a reader checks the question against.
	Basis string `json:"basis"`
	// Choices are sentences the person may send instead of writing their own.
	// They are a draft, not a permission and not a submission: pressing one
	// fills the request box, and what moves the task is the monitor reading
	// what came back (AC-N8).
	Choices []workChoice `json:"choices,omitempty"`
}

// workReviewOutcome is how a review ends. Which fields matter depends on which
// of the two ways out is being taken, and each refuses what it cannot check.
type workReviewOutcome struct {
	// Evidence is what confirmed the decision was applied — the status that
	// appeared, the permission that now answers, the run that went green. A
	// resume with nothing here is refused: the platform cannot see out there,
	// so the confirmation has to be named by whoever did see it.
	Evidence string `json:"evidence,omitempty"`
	// Remaining is what is left to do. A resume requires it (otherwise the task
	// is finished, not resumed) and a completion requires it to be empty.
	Remaining string `json:"remaining,omitempty"`
	// Accepted says the final result was accepted. Only a completion reads it,
	// and only `true` completes.
	Accepted *bool `json:"accepted,omitempty"`
	// CompletionCriteria is the task's own completion criteria, echoed back
	// verbatim. It is how "the criteria were met" is a statement about *these*
	// criteria rather than about whatever the caller remembers — a mismatch is
	// refused rather than resolved.
	CompletionCriteria *string `json:"completion_criteria,omitempty"`
}

// isReviewTool reports whether a name is one of the three moves.
func isReviewTool(name string) bool {
	switch name {
	case taskReviewOpenTool, taskReviewResumeTool, taskReviewCompleteTool:
		return true
	}
	return false
}

func reviewToolDefinitions() []map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	integer := func(min int) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "maximum": requestSafeInt}
	}
	choice := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label":        map[string]any{"type": "string", "description": "What the person sees on the choice (at most 80 characters)."},
			"request_text": map[string]any{"type": "string", "description": "The sentence pressing it puts in their request box (at most 1000 characters)."},
		},
		"required": []string{"label", "request_text"}, "additionalProperties": false,
	}
	review := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target":   map[string]any{"type": "string", "description": "Exactly what the decision is about — this repository, this pull request, this resource. Not a category."},
			"scope":    map[string]any{"type": "string", "description": "How far the decision reaches, so granting it is not open-ended."},
			"route":    map[string]any{"type": "string", "description": "The official way this is approved, so the person is not left to find the door."},
			"question": map[string]any{"type": "string", "description": "What the person is asked, in your own words. Stored verbatim — it is not rewritten or dressed up."},
			"basis":    map[string]any{"type": "string", "description": "What was observed that made this decision necessary. It is what a reader checks the question against."},
			"choices":  map[string]any{"type": "array", "maxItems": workChoiceMax, "items": choice},
		},
		"required": []string{"target", "scope", "route", "question", "basis"}, "additionalProperties": false,
	}
	outcome := func(properties map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []map[string]any{
		{
			"name": taskReviewOpenTool,
			"description": "Moves a task to review because something has come up that only the person can settle, and records it on the turn you are in: the exact target, how far the decision reaches, the official approval route, the question they can answer and what you saw that made it necessary. " +
				"One call, one transaction — the question and the status cannot land apart. " +
				"It takes a task at any status, including completed: a task recorded as finished when the person had in fact decided nothing belongs back in review, and this is the call that carries their statement and your check rather than a bare status write that would lose both. " +
				"Asking the same thing again is not a second review: a call naming the same target, scope and route while that review is still open answers already_open and writes nothing, so a retry or a fresh request from the person never stacks approvals. A different decision while one is open is a conflict. " +
				"This is not for a technical wait. A CI run in flight or a retry the policy already allows is not a question for anybody — it stays running, with its owner, due and retry recorded in " +
				taskBookPutTool + " under details." + workTechnicalWaitKey + ".",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "request_id": str(),
				"turn_id":               integer(1),
				"expected_version":      integer(1),
				"expected_turn_version": integer(1),
				"expected_book_version": integer(1),
				"review":                review,
			}, "required": []string{"task_id", "request_id", "turn_id",
				"expected_version", "expected_turn_version", "expected_book_version", "review"},
				"additionalProperties": false},
		},
		{
			"name": taskReviewResumeTool,
			"description": "Moves a task in review back to running, once you have read what came back and confirmed the decision is actually in force and what else the work needs. " +
				"outcome.evidence names what confirmed it — the status that appeared, the permission that now answers, the run that went green — and a resume that names nothing is refused: this platform cannot see out there, so the confirmation has to come from whoever did. " +
				"outcome.remaining says what is left; a task with nothing left is finished rather than resumed, so that is " + taskReviewCompleteTool + ". " +
				"A pick or a submitted request does not resume anything on its own — pressing a choice fills a draft, submitting it becomes the next turn, and this call is the separate step where you say you have read it. " +
				"The turns are left exactly as they are: the question stays asked and the exchanges stay as they happened. What this review was about goes onto details." + workReviewHistoryKey + " with the confirmation beside it.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "request_id": str(),
				"expected_version":      integer(1),
				"expected_book_version": integer(1),
				"outcome": outcome(map[string]any{
					"evidence":  map[string]any{"type": "string", "description": "What confirmed the decision is in force. Required — a resume that names nothing is refused."},
					"remaining": map[string]any{"type": "string", "description": "What is left to do. Required — a task with nothing left is completed, not resumed."},
				}, []string{"evidence", "remaining"}),
			}, "required": []string{"task_id", "request_id", "expected_version", "expected_book_version", "outcome"},
				"additionalProperties": false},
		},
		{
			"name": taskReviewCompleteTool,
			"description": "Completes a task that is in review, which is the only way out of review other than resuming. Every one of these has to hold: outcome.accepted is true (the final result was accepted), outcome.completion_criteria repeats the task's own criteria verbatim (so \"the criteria were met\" is about these criteria and not about what you remember — a mismatch is refused), outcome.remaining is empty, no request is still open on any turn, and no decision is left unapplied. " +
				"It closes the open review rather than requiring one to have been closed already — the person accepting the result is what settles the decision they were asked about, and outcome.evidence has to say how that acceptance was observed. " +
				"A task that records no completion criteria cannot be completed this way: write them with task_put first, because there is otherwise nothing to have met. " +
				"The ordinary pre-approved path is untouched — a task that never needed anybody's judgement still goes planned → running → completed with task_put.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "request_id": str(),
				"expected_version":      integer(1),
				"expected_book_version": integer(1),
				"outcome": outcome(map[string]any{
					"accepted":            map[string]any{"type": "boolean", "description": "The final result was accepted. Only true completes."},
					"evidence":            map[string]any{"type": "string", "description": "How that acceptance was observed. Required — a submitted sentence, a deferral or an approval that was never applied is not an acceptance."},
					"completion_criteria": map[string]any{"type": "string", "description": "The task's own completion_criteria, verbatim."},
					"remaining":           map[string]any{"type": "string", "description": "What is left. Must be empty or absent."},
				}, []string{"accepted", "evidence", "completion_criteria"}),
			}, "required": []string{"task_id", "request_id", "expected_version", "expected_book_version", "outcome"},
				"additionalProperties": false},
		},
	}
}

func validateReviewTool(name string, a workArgs) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if !workID.MatchString(a.TaskID) {
		return bad("task_id must be 1..128 letters, digits, _ or -")
	}
	if a.ExpectedVersion == nil || *a.ExpectedVersion < 1 || *a.ExpectedVersion > requestSafeInt {
		return bad("expected_version must be the task's current version")
	}
	if a.ExpectedBookVersion == nil || *a.ExpectedBookVersion < 1 || *a.ExpectedBookVersion > requestSafeInt {
		return bad("expected_book_version must be the book_version " + taskBookGetTool + " returned")
	}
	if name == taskReviewOpenTool {
		if a.TurnID == nil || *a.TurnID < 1 || *a.TurnID > requestSafeInt {
			return bad("turn_id must be the turn you are recording this question on")
		}
		if a.ExpectedTurnVersion == nil || *a.ExpectedTurnVersion < 1 || *a.ExpectedTurnVersion > requestSafeInt {
			return bad("expected_turn_version must be the turn_version the task's turns were read at")
		}
		return validateReviewInput(a.Review)
	}
	if a.Outcome == nil {
		return bad("outcome required")
	}
	if len(a.Outcome.Evidence) > workReviewTextMax || len(a.Outcome.Remaining) > workReviewTextMax {
		return bad(fmt.Sprintf("evidence and remaining must each be at most %d characters", workReviewTextMax))
	}
	if name == taskReviewResumeTool {
		if strings.TrimSpace(a.Outcome.Evidence) == "" {
			return bad("outcome.evidence must name what confirmed the decision is in force — " +
				"this platform cannot see that for itself, so a resume that names nothing is refused")
		}
		if strings.TrimSpace(a.Outcome.Remaining) == "" {
			return bad("outcome.remaining must say what is left to do — a task with nothing left is finished, " +
				"which is " + taskReviewCompleteTool)
		}
		return nil
	}
	if a.Outcome.Accepted == nil || !*a.Outcome.Accepted {
		return bad("outcome.accepted must be true — a result that was not accepted is not a completion")
	}
	// Named for the reason a resume names its confirmation: the acceptance is
	// something only the person gave and only the monitor saw, so a completion
	// that cannot say how it was observed is a claim rather than a record. It is
	// also what separates a real acceptance from a permission somebody said they
	// would grant and never applied.
	if strings.TrimSpace(a.Outcome.Evidence) == "" {
		return bad("outcome.evidence must name how the final acceptance was observed — " +
			"a submitted sentence, a deferral or an approval that was never applied is not one")
	}
	if a.Outcome.CompletionCriteria == nil {
		return bad("outcome.completion_criteria must repeat the task's own criteria verbatim")
	}
	if strings.TrimSpace(a.Outcome.Remaining) != "" {
		return bad("outcome.remaining must be empty to complete — what is left means the task is not done")
	}
	return nil
}

func validateReviewInput(review *workReviewInput) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if review == nil {
		return bad("review required")
	}
	for field, value := range map[string]string{
		"target": review.Target, "scope": review.Scope, "route": review.Route,
		"question": review.Question, "basis": review.Basis,
	} {
		if strings.TrimSpace(value) == "" {
			return bad("review." + field + " is required — " + reviewFieldReason(field))
		}
		if len(value) > workReviewTextMax {
			return bad(fmt.Sprintf("review.%s must be at most %d characters", field, workReviewTextMax))
		}
	}
	if len(review.Choices) > workChoiceMax {
		return bad(fmt.Sprintf("at most %d choices — a person picks from a short list or writes their own", workChoiceMax))
	}
	for _, offered := range review.Choices {
		if strings.TrimSpace(offered.Label) == "" || len(offered.Label) > workChoiceLabelMax {
			return bad(fmt.Sprintf("each choice label must be 1..%d characters", workChoiceLabelMax))
		}
		if strings.TrimSpace(offered.RequestText) == "" || len(offered.RequestText) > workChoiceTextMax {
			return bad(fmt.Sprintf("each choice request_text must be 1..%d characters", workChoiceTextMax))
		}
	}
	return nil
}

// reviewFieldReason says why a field cannot be left out, so a refusal teaches
// the shape instead of only rejecting it.
func reviewFieldReason(field string) string {
	switch field {
	case "target":
		return "a decision about a category is not one anybody can give"
	case "scope":
		return "an approval with no reach is open-ended"
	case "route":
		return "a question with no official route leaves the person to find the door"
	case "question":
		return "a review the person cannot answer is a dead end"
	default:
		return "what you saw is what a reader checks the question against"
	}
}

func runReviewTool(ctx context.Context, tx *sql.Tx, name string, a workArgs, now time.Time) (any, error) {
	switch name {
	case taskReviewOpenTool:
		return openTaskReview(ctx, tx, a, now)
	case taskReviewResumeTool:
		return closeTaskReview(ctx, tx, a, now, workStatusRunning)
	case taskReviewCompleteTool:
		return closeTaskReview(ctx, tx, a, now, workStatusCompleted)
	}
	return nil, workFail("invalid_argument", "unknown review tool")
}

// openTaskReview records one decision and moves the task to review in the same
// transaction, so the question and the status cannot land apart.
func openTaskReview(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	task, err := loadWorkTask(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	// Any status may be the one before this. There is no source this refuses,
	// deliberately: the correction the owner's target-state document names is a
	// task sitting at `completed` with no decision ever recorded, and the whole
	// point of that correction is that it carries the person's own statement and
	// the monitor's check — which is this call's arguments. Refusing `completed`
	// here would send that correction through a bare status write, losing the
	// evidence on the one move that exists to preserve it.
	details, err := readReviewDetails(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	// Asking the same thing again is not a second review. This is checked
	// before any counter is taken, so a retry of an answer the caller never saw
	// — and a fresh request from the person about the same decision — costs a
	// repeated call and never a duplicate approval.
	if open, found := openReviewOf(details); found {
		if sameReviewSubject(open, a.Review) {
			answer, answerErr := resolveWorkTaskAnswer(ctx, tx, task)
			if answerErr != nil {
				return nil, answerErr
			}
			return map[string]any{
				"task": answer, "review": open, "already_open": true, "task_version": task.Version,
			}, nil
		}
		return nil, workFail("conflict", "this task is already in review about "+
			reviewSubjectLine(open)+" — settle or resume that one before asking something else, "+
			"so a person is never holding two open decisions on one task")
	}
	turn, err := loadWorkTurn(ctx, tx, a.TaskID, *a.TurnID)
	if err != nil {
		return nil, err
	}
	if turn.EndedAt != nil {
		return nil, workFail("conflict", "that turn has ended — a question cannot be asked on an exchange that is over; "+
			"append the turn you are in and record it there")
	}
	if turn.Question != nil {
		return nil, workFail("conflict", "that turn already asks a question — a person was shown it, "+
			"so it is not rewritten here")
	}
	if _, _, err = requireWorkTaskVersion(ctx, tx, a.TaskID, *a.ExpectedVersion, a.ExpectedTurnVersion); err != nil {
		return nil, err
	}
	if _, err = swapWorkGuard(ctx, tx, a.TaskID, workGuardBook, *a.ExpectedBookVersion); err != nil {
		return nil, err
	}
	// The question goes on the turn verbatim, with the choices beside it. The
	// choices are a draft the person may send; what moves the task is the
	// monitor reading what came back (AC-N8).
	if _, err = tx.ExecContext(ctx,
		"UPDATE sp_work_turns SET question=?,choices=? WHERE task_id=? AND id=?",
		a.Review.Question, encodeWorkChoices(a.Review.Choices), a.TaskID, *a.TurnID); err != nil {
		return nil, err
	}
	record := map[string]any{
		"target": a.Review.Target, "scope": a.Review.Scope, "route": a.Review.Route,
		"question": a.Review.Question, "basis": a.Review.Basis,
		"turn_id": *a.TurnID, "opened_at": workNow(now), "applied": false,
	}
	// A legacy row's own word is kept: it is the only record that this task was
	// once written as blocked, and the move overwrites the status byte.
	if task.Status == workStatusBlockedLegacy {
		record["from_status"] = workStatusBlockedLegacy
	}
	details[workReviewKey] = record
	if err = writeReviewDetails(ctx, tx, a.TaskID, details); err != nil {
		return nil, err
	}
	moved, err := setWorkTaskStatus(ctx, tx, task, workStatusReview, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"task": workTaskAnswer{workTask: moved}, "review": record, "already_open": false,
		"task_version": moved.Version,
	}, nil
}

// closeTaskReview is the one way out of review, with the checks of whichever
// way is being taken. Both ways share everything except what they have to be
// sure of, so they share the code: a resume and a completion that drifted apart
// would be two different ideas of what review means.
func closeTaskReview(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time, to string) (any, error) {
	task, err := loadWorkTask(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	effective, basis, err := resolveWorkStatus(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	details, err := readReviewDetails(ctx, tx, a.TaskID)
	if err != nil {
		return nil, err
	}
	open, hasOpen := openReviewOf(details)
	// A task leaves review when it is **at** review or when it still **carries**
	// an open review — and the second is not the same sentence as the first.
	//
	// A stored request moves a task that was waiting on a person out of review
	// and into `planned` in the transaction that stores it (receipt: the words
	// arrived, nobody has picked them up yet; session_work_views.go). The
	// decision it was waiting on is not settled by somebody sending a sentence,
	// so the recorded review stays open — and the status byte no longer says
	// `review`. Asking only the byte left the monitor with no way to close its
	// own review once the person had answered: the two calls that carry the
	// evidence both refused, measured on the combined tree.
	//
	// So the question is the record, not the word. That also means this is not
	// a rule about `planned`: a task at any status that still carries an open
	// review can be settled, and a task that carries none cannot — which is the
	// old sentence, kept for every task that never had one.
	if effective != workStatusReview && !hasOpen {
		return nil, workFail("conflict", "this task is at "+effective+
			" and carries no open review, and only a task in review leaves review"+
			legacyBasisSuffix(task, basis))
	}
	// Checked here as well as in the conditional UPDATE below, so a caller
	// holding a version the task has moved past is told that before anything
	// is read or built on it. The UPDATE is still the swap that excludes.
	if task.Version != *a.ExpectedVersion {
		return nil, workFail("conflict", "expected_version does not match the task's current version")
	}
	// A completion closes the open review rather than requiring it to have been
	// closed already — that is what completing *is*: the person accepted the
	// result, so the decision they were asked about is settled by the same act.
	//
	// An earlier revision refused a completion while a review was open, meaning
	// a formally opened review could only ever be completed by first resuming it
	// — review → running → review → completed — and the only completions this
	// code could actually reach were on tasks whose review nobody had recorded.
	// Measured on the combined tree, which is where it showed: the receipt
	// state is an open review by construction.
	//
	// What keeps an *unsettled* decision from completing is not this check but
	// the four below plus the evidence the caller has to name: a submission
	// alone leaves a request turn with no ended_at, an unapplied permission is
	// one nobody can name a confirmation for, and an explanation or a deferral
	// satisfies neither. None of them is a completion.
	if to == workStatusCompleted {
		if err = requireCompletableTask(ctx, tx, task, a.Outcome); err != nil {
			return nil, err
		}
	}
	entry := map[string]any{
		"closed_at": workNow(now), "to_status": to,
		"evidence": a.Outcome.Evidence, "applied": true,
	}
	if a.Outcome.Remaining != "" {
		entry["remaining"] = a.Outcome.Remaining
	}
	if a.Outcome.Accepted != nil {
		entry["accepted"] = *a.Outcome.Accepted
	}
	// A legacy row carries no recorded review, so what is kept is the reading
	// that got it here: the stored word and the field that decided. Without
	// that, resuming a legacy row would erase the only account of why it was
	// ever held.
	if hasOpen {
		entry["review"] = open
	} else {
		entry["review"] = map[string]any{
			"from_status": task.Status, "status_basis": basis,
			"note": "no review was recorded on this task; the status was read from the stored word",
		}
	}
	// Where it was when it was settled. For a task the receipt had moved to
	// `planned` this is the only record that the decision was closed from the
	// queue rather than from review, and it is a fact about this closing rather
	// than about the review it closes — so it sits on the entry, not inside it.
	entry["from_status"] = task.Status
	details[workReviewHistoryKey] = appendReviewHistory(details[workReviewHistoryKey], entry)
	delete(details, workReviewKey)
	if _, err = swapWorkGuard(ctx, tx, a.TaskID, workGuardBook, *a.ExpectedBookVersion); err != nil {
		return nil, err
	}
	if err = writeReviewDetails(ctx, tx, a.TaskID, details); err != nil {
		return nil, err
	}
	moved, err := setWorkTaskStatus(ctx, tx, task, to, now)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"task": workTaskAnswer{workTask: moved}, "closed": entry, "task_version": moved.Version,
	}, nil
}

// requireCompletableTask is everything a completion has to be sure of beyond
// the status: the result was accepted, the criteria it was measured against are
// the task's own, nothing is left, and no request is still waiting for an
// answer.
func requireCompletableTask(ctx context.Context, tx *sql.Tx, task workTask, outcome *workReviewOutcome) error {
	if task.CompletionCriteria == nil || strings.TrimSpace(*task.CompletionCriteria) == "" {
		return workFail("conflict", "this task records no completion criteria, so there is nothing to have met — "+
			"write them with task_put first")
	}
	if *outcome.CompletionCriteria != *task.CompletionCriteria {
		return workFail("conflict", "outcome.completion_criteria is not this task's criteria verbatim — "+
			"read the task again and echo what it says, so \"the criteria were met\" is about these criteria")
	}
	open, err := taskOpenRequestCount(ctx, tx, task.ID)
	if err != nil {
		return err
	}
	if open != 0 {
		return workFail("conflict", fmt.Sprintf(
			"%d request(s) on this task have no ended_at — a completion over an unanswered request claims work nobody did. "+
				"Close them with turn_patch, or say what is left and resume", open))
	}
	return nil
}

// taskOpenRequestCount counts the turns that still need somebody: a request
// arrived and the turn has no ended_at. It is the same rule turn_list({open:
// true}) selects on, so a completion cannot disagree with the list a parent
// reads its work from.
func taskOpenRequestCount(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	var open int
	err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_turns WHERE task_id=? AND request IS NOT NULL AND ended_at IS NULL",
		taskID).Scan(&open)
	return open, err
}

// setWorkTaskStatus writes one status under the version the caller held. The
// expected version is in the WHERE clause and the row count is checked, so a
// status move is the same single-winner swap task_put is.
func setWorkTaskStatus(ctx context.Context, tx *sql.Tx, task workTask, status string, now time.Time) (workTask, error) {
	stamp := workNow(now)
	result, err := tx.ExecContext(ctx,
		"UPDATE sp_work_tasks SET status=?,version=?,updated_at=? WHERE id=? AND version=?",
		status, task.Version+1, stamp, task.ID, task.Version)
	if err != nil {
		return workTask{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return workTask{}, err
	}
	if changed != 1 {
		return workTask{}, workFail("conflict", "expected_version does not match the task's current version")
	}
	moved := task
	moved.Status = status
	moved.Version = task.Version + 1
	moved.UpdatedAt = &stamp
	return moved, nil
}

// openReviewOf answers the review this task is waiting on, if any. A record
// whose `applied` is true is one that has been settled and is not open.
func openReviewOf(details map[string]any) (map[string]any, bool) {
	record, ok := details[workReviewKey].(map[string]any)
	if !ok {
		return nil, false
	}
	if applied, isBool := record["applied"].(bool); isBool && applied {
		return nil, false
	}
	return record, true
}

// sameReviewSubject reports whether a call is asking what is already open. The
// subject is the target, the scope and the route — what the decision *is*. The
// question's wording is deliberately not part of it: a monitor that rephrases
// the same ask on a retry must not thereby create a second approval.
func sameReviewSubject(open map[string]any, asked *workReviewInput) bool {
	text := func(key string) string {
		value, _ := open[key].(string)
		return value
	}
	return text("target") == asked.Target && text("scope") == asked.Scope && text("route") == asked.Route
}

func reviewSubjectLine(open map[string]any) string {
	text := func(key string) string {
		value, _ := open[key].(string)
		return value
	}
	return fmt.Sprintf("target %q, scope %q, route %q", text("target"), text("scope"), text("route"))
}

// appendReviewHistory adds one closed review, oldest first, dropping whole
// entries from the front when there are too many. Entries are dropped whole
// because half an account of why a task was held is worse than one fewer.
func appendReviewHistory(current any, entry map[string]any) []any {
	history, _ := current.([]any)
	history = append(history, entry)
	if len(history) > workReviewHistoryKept {
		history = history[len(history)-workReviewHistoryKept:]
	}
	return history
}

// readReviewDetails reads the parent's own details JSON for one task. A task
// with no bookkeeping yet answers an empty map rather than not_found: a task
// created after the cutover simply has nothing recorded beside it.
func readReviewDetails(ctx context.Context, tx *sql.Tx, taskID string) (map[string]any, error) {
	book, found, err := readTaskBook(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	details := map[string]any{}
	if !found || book.details == "" {
		return details, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(book.details), &decoded); err != nil || decoded == nil {
		// Unparseable bookkeeping is not an empty record: overwriting it would
		// destroy the only copy of whatever is in there. The classifier reads
		// the same silence as "nothing stated", which lands on review.
		return details, errUnparseableBook
	}
	return decoded, nil
}

// errUnparseableBook is bookkeeping this code cannot read. It is distinct from
// absent on purpose: absent is written from scratch, unparseable is left alone.
var errUnparseableBook = workFail("conflict",
	"this task's bookkeeping is not JSON this image can read, so it is left exactly as it is — "+
		"nothing here overwrites a record that may be the only copy of something. Read it with "+
		taskBookGetTool+" and repair it with "+taskBookPutTool)

// writeReviewDetails replaces the details half of one task's bookkeeping,
// leaving the stage, owner and session relations the parent keeps there alone.
// It goes to the owned record, which is where a parent's write goes
// (session_work_parent.go).
func writeReviewDetails(ctx context.Context, tx *sql.Tx, taskID string, details map[string]any) error {
	book, _, err := readTaskBook(ctx, tx, taskID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if len(encoded) > workLegacyBytesMax {
		return workFail("invalid_argument",
			"this task's bookkeeping would pass 64KiB — it is a record, not an output archive. "+
				"Trim it with "+taskBookPutTool)
	}
	sessions := book.sessions
	if sessions == "" {
		sessions = "[]"
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sp_work_task_book (task_id,stage,owner,sessions,details)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(task_id) DO UPDATE SET details=excluded.details`,
		taskID, book.stage, book.owner, sessions, string(encoded))
	return err
}

// --------------------------------------------------------------- the alias

// workStatusFilter resolves a status filter to the effective status it selects.
// `blocked` is the one alias: a filter somebody saved while that word was a
// status still selects the work it was saved to find, which is now review.
// Anything outside the domain is refused rather than answered with an empty
// page — "no such status" and "no such work" are different answers.
func workStatusFilter(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if raw == workStatusBlockedLegacy {
		return workStatusReview, true
	}
	if isWorkStatus(raw) {
		return raw, true
	}
	return "", false
}

// workTaskInputStatus resolves a status a writer gives. `blocked` is accepted
// and stored as review: an image older than this one cannot know the new word,
// and refusing it would stop work a person left rather than move it. The
// answer says the alias was applied, so a writer is never left believing it
// stored something this slot does not hold.
func workTaskInputStatus(raw string) (status string, aliased bool) {
	if raw == workStatusBlockedLegacy {
		return workStatusReview, true
	}
	return raw, false
}

// --------------------------------------------------- resolving a stored row

// workTaskAnswer is one task as a reader gets it: the agreed columns, with the
// status resolved, and — only when a byte on disk had to be resolved — what the
// slot holds and what decided.
//
// The two extra fields are not columns and are not part of the agreed task: the
// table still has exactly the columns it agreed to
// (TestWorkSchemaHasOnlyTheAgreedColumns fixes that), and they are absent from
// every answer about a task whose status needed no deciding, which is every
// task written by this image. They are here rather than hidden because the
// resolution is a judgement this code made about somebody's work, and a
// judgement nobody can check is one nobody should have to accept.
type workTaskAnswer struct {
	workTask
	// StoredStatus is the word the slot actually holds, present only when the
	// answer's own status is not it. Today that is `blocked` and nothing else.
	StoredStatus string `json:"stored_status,omitempty"`
	// StatusBasis names the field that decided, so a reader can go and read the
	// same field. It is present exactly when StoredStatus is.
	StatusBasis string `json:"status_basis,omitempty"`
}

// resolveWorkTaskAnswer is the one place a stored row becomes an answer, so the
// tool path, the sidecar view and the control plane cannot disagree about what
// a task's status is.
func resolveWorkTaskAnswer(ctx context.Context, tx *sql.Tx, task workTask) (workTaskAnswer, error) {
	if task.Status != workStatusBlockedLegacy {
		return workTaskAnswer{workTask: task}, nil
	}
	effective, basis, err := resolveWorkStatus(ctx, tx, task)
	if err != nil {
		return workTaskAnswer{}, err
	}
	answer := workTaskAnswer{workTask: task, StoredStatus: task.Status, StatusBasis: basis}
	answer.Status = effective
	return answer, nil
}

// resolveWorkStatus answers the status a task actually has, and what decided it.
//
// A row this image wrote needs nothing decided: its status is the answer and
// the basis is empty. A row an older image wrote as `blocked` is the whole
// reason this function exists, and the order of the cases below is the
// judgement:
//
//  1. Two statements that disagree are a disagreement, not a tie to break.
//     It goes to a person.
//  2. A recorded, unsettled decision is a person's to make.
//  3. A question asked on a turn nobody has closed is a person's to answer,
//     whatever the bookkeeping says about waits.
//  4. A technical wait that does not state what a wait has to state is not one
//     this code will act on — it says so rather than resuming the work.
//  5. A technical wait that states all three facts is a wait, not a question.
//     This is the only way to `running`.
//  6. Silence is the ambiguous case, and it goes to a person. Nothing is
//     resumed on the strength of a row that explains nothing, and no question
//     is invented for it either — the answer says there was no basis, which is
//     the honest account of what this code found.
func resolveWorkStatus(ctx context.Context, tx *sql.Tx, task workTask) (string, string, error) {
	if task.Status != workStatusBlockedLegacy {
		return task.Status, "", nil
	}
	details, err := readReviewDetails(ctx, tx, task.ID)
	if errors.Is(err, errUnparseableBook) {
		return workStatusReview, "details is not readable JSON, so the bookkeeping states nothing either way", nil
	}
	if err != nil {
		return "", "", err
	}
	_, reviewOpen := openReviewOf(details)
	wait := readTechnicalWait(details)
	asked, err := taskHasOpenQuestion(ctx, tx, task.ID)
	if err != nil {
		return "", "", err
	}
	switch {
	case reviewOpen && wait.present:
		return workStatusReview, "details." + workReviewKey + " and details." + workTechnicalWaitKey +
			" disagree, and a disagreement is for a person to settle", nil
	case reviewOpen:
		return workStatusReview, "details." + workReviewKey, nil
	case asked:
		return workStatusReview, "a question on an open turn", nil
	case wait.present && !wait.stated:
		return workStatusReview, "details." + workTechnicalWaitKey + " " + wait.why, nil
	case wait.stated:
		return workStatusRunning, "details." + workTechnicalWaitKey, nil
	}
	return workStatusReview, "no recorded basis", nil
}

// legacyBasisSuffix adds what decided to a refusal about a legacy row, so
// somebody told "this task is at review" can see which field said so.
func legacyBasisSuffix(task workTask, basis string) string {
	if task.Status != workStatusBlockedLegacy || basis == "" {
		return ""
	}
	return fmt.Sprintf(" (this slot holds %q on it; %s is what reads it as %s)",
		workStatusBlockedLegacy, basis, workStatusReview)
}

// technicalWait is what one task's bookkeeping says about being in a
// pre-approved technical wait: whether it says anything, whether what it says
// is the shape a wait has to be, and why not when it is not.
type technicalWait struct {
	// present is a details.technical_wait key of any shape. Silence is the
	// ordinary case — most tasks are not waiting on a machine.
	present bool
	// stated is a statement this code will act on: all three facts, each in the
	// shape it has to be.
	stated bool
	// why is what is missing or wrong, for a reader of the basis.
	why string
}

// readTechnicalWait reads the one statement that resolves a legacy `blocked`
// row to `running`.
//
// The three facts are not decoration. A wait with no owner is one nobody will
// come back to; a wait with no due is one that cannot be told from a wait that
// has hung; and a wait with no retry condition is one where "try again" is a
// guess rather than a policy. A row missing any of them is a row that cannot be
// distinguished from work that quietly stopped, and this code will not call
// that running.
//
// Nothing is interpreted. A string that reads like a wait is not an object, a
// due date is not parsed into a time (that would be this code deciding what
// "overdue" means on somebody else's work), and prose anywhere in details is
// not a statement. Anything that is not the shape is reported and goes to a
// person.
func readTechnicalWait(details map[string]any) technicalWait {
	raw, named := details[workTechnicalWaitKey]
	if !named {
		return technicalWait{}
	}
	stated, isObject := raw.(map[string]any)
	if !isObject {
		return technicalWait{present: true, why: "is not a JSON object, so the bookkeeping states nothing either way"}
	}
	missing := []string{}
	for _, field := range []string{"owner", "due"} {
		value, _ := stated[field].(string)
		if strings.TrimSpace(value) == "" {
			missing = append(missing, field)
		}
	}
	if !hasRetryCondition(stated["retry"]) {
		missing = append(missing, "retry")
	}
	if len(missing) != 0 {
		return technicalWait{present: true,
			why: "names no " + strings.Join(missing, ", ") + ", so it cannot be told from work that stopped"}
	}
	return technicalWait{present: true, stated: true}
}

// hasRetryCondition reports whether a retry condition is actually stated. The
// three shapes it takes are the three a parent writes without ambiguity: `true`
// for "the policy allows it", a sentence for one condition, and an object for a
// condition with parts. `false` is a statement too — that no retry is allowed —
// and a wait nobody may retry is one somebody has to look at.
func hasRetryCondition(raw any) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		return strings.TrimSpace(value) != ""
	case map[string]any:
		return len(value) != 0
	}
	return false
}

// taskHasOpenQuestion reports whether a question is sitting on a turn nobody has
// closed. It is asked about a legacy row because a question is the plainest
// evidence there is that somebody is waiting on a person — plainer than any
// bookkeeping, since it is what the person was actually shown.
func taskHasOpenQuestion(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var asked int
	err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM sp_work_turns WHERE task_id=? AND question IS NOT NULL AND ended_at IS NULL",
		taskID).Scan(&asked)
	return asked != 0, err
}
