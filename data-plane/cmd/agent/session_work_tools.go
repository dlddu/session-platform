// The tool surface over the work records: what the parent may say about a task
// and its turns, and what it must hold to say it.
//
// Every mutation takes the task's version. A turn write does not bump it — a
// turn is an event on the task, not a new state of it — but it has to match, so
// two parents that both read the task cannot both write the turn that follows.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func workToolDefinitions() []map[string]any {
	str := func() map[string]any { return map[string]any{"type": "string"} }
	integer := func(min int64) map[string]any { return map[string]any{"type": "integer", "minimum": min} }
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": workListLimitMax, "default": workListLimitDefault}
	choices := map[string]any{"type": "array", "maxItems": workChoiceMax, "items": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label":        map[string]any{"type": "string", "description": fmt.Sprintf("The button's words (1..%d characters).", workChoiceLabelMax)},
			"request_text": map[string]any{"type": "string", "description": fmt.Sprintf("The sentence this choice sends as the next turn's request (1..%d characters).", workChoiceTextMax)},
		},
		"required":             []string{"label", "request_text"},
		"additionalProperties": false,
	}}
	turnProperties := map[string]any{
		"session_seq":  map[string]any{"type": "integer", "minimum": 0, "description": "The assignee session's own request number for this turn. Omit when it is not known; null clears it."},
		"request":      map[string]any{"type": "string", "description": "The person's words, verbatim."},
		"instruction":  map[string]any{"type": "string", "description": "What the parent passed to the session doing the work, verbatim."},
		"response":     map[string]any{"type": "string", "description": "The assignee session's final answer, verbatim. Never a summary written by the parent — that is explanation."},
		"explanation":  map[string]any{"type": "string", "description": "The parent's own account of the result for the person."},
		"question":     map[string]any{"type": "string", "description": "What the parent is asking the person on this turn. null clears it."},
		"choices":      choices,
		"artifacts":    map[string]any{"type": "array", "maxItems": workArtifactMax, "items": str()},
		"requested_at": str(),
		"started_at":   str(),
		"ended_at":     str(),
	}
	return append([]map[string]any{
		{
			"name":        "task_get",
			"description": "One task: session_id (null when unassigned), title, description, completion_criteria, status, version, created_at/updated_at (null means never recorded). The reply also carries turns_total and the task's latest turn, so a parent sees at a glance whether a request is waiting. Reading changes nothing.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"id": str()},
				"required": []string{"id"}, "additionalProperties": false},
		},
		{
			"name":        "task_list",
			"description": "Tasks on this slot, id-keyset ordered. Filters: status, session_id (the literal \"none\" selects the unassigned ones), after_id, limit (1..100, default 20). Pass next_cursor unchanged as after_id; an empty cursor means done. A page is not a snapshot, so dedupe by id.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"status": str(), "session_id": str(), "after_id": str(), "limit": limit,
			}, "required": []string{}, "additionalProperties": false},
		},
		{
			"name":        "task_put",
			"description": "Creates (expected_version 0) or fully replaces (expected_version = the task's current version) one task. Full replacement: every field is required and unnamed fields are not preserved. The version increments on success; a stale version is a conflict. request_id replays the original committed answer. A deleted task id is retired and cannot be reused.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "expected_version": integer(0),
				"task": map[string]any{"type": "object", "properties": map[string]any{
					"session_id":          map[string]any{"type": "string", "description": "The session that has this task. null means unassigned."},
					"title":               str(),
					"description":         map[string]any{"type": "string", "description": "What the task is for and what it requires."},
					"completion_criteria": map[string]any{"type": "string", "description": "How this task will be known to be done."},
					"status":              map[string]any{"type": "string", "description": "planned, running, blocked, review, completed or unknown."},
				}, "required": []string{"session_id", "title", "description", "completion_criteria", "status"}, "additionalProperties": false},
			}, "required": []string{"id", "request_id", "expected_version", "task"}, "additionalProperties": false},
		},
		{
			"name":        "task_delete",
			"description": "Removes one task and its turns. expected_version must be the task's current version. The id is retired permanently, so a later create under the same id is a conflict rather than a revival. Receipts and the preserved legacy record of a migrated task stay.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": str(), "request_id": str(), "expected_version": integer(1),
			}, "required": []string{"id", "request_id", "expected_version"}, "additionalProperties": false},
		},
		{
			"name":        "turn_append",
			"description": "Appends one turn to a task. turn.id is assigned by the database and increases across the whole database, so it is also the order turns happened in. expected_version must be the task's current version; appending a turn moves the task's updated_at and leaves its version alone. Omitted fields are null: a turn is written as it happens rather than all at once.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "request_id": str(), "expected_version": integer(1),
				"expected_turn_version": integer(1),
				"turn":                  map[string]any{"type": "object", "properties": turnProperties, "required": []string{}, "additionalProperties": false},
			}, "required": []string{"task_id", "request_id", "expected_version", "expected_turn_version", "turn"}, "additionalProperties": false},
		},
		{
			"name":        "turn_patch",
			"description": "Fills in fields of one existing turn — the instruction that went out, the session's final answer, the parent's explanation, a question and its choices, timestamps. Only the fields named are touched; an explicit null clears one. Writing started_at takes the turn and the reply carries a claim_token that turn_dispatch requires; clearing started_at hands the turn back and destroys that token. Taking a turn that is already taken is refused.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "turn_id": integer(1), "request_id": str(), "expected_version": integer(1),
				"expected_turn_version": integer(1),
				"turn":                  map[string]any{"type": "object", "properties": turnProperties, "required": []string{}, "additionalProperties": false},
			}, "required": []string{"task_id", "turn_id", "request_id", "expected_version", "expected_turn_version", "turn"}, "additionalProperties": false},
		},
		{
			"name":        "turn_list",
			"description": "One task's turns, oldest id first. open=true selects only the turns that still need the parent: a request arrived and the turn has no ended_at. after_turn_id is the keyset cursor, limit 1..100 (default 20). Reading changes nothing — a turn is consumed by writing its response and ended_at.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "open": map[string]any{"type": "boolean"},
				"after_turn_id": integer(0), "limit": limit,
			}, "required": []string{"task_id"}, "additionalProperties": false},
		},
		{
			"name":        "turn_dispatch",
			"description": "Records what the parent is about to do outside this database for one turn, before it does it: operation_key is the parent's own idempotency key, operation_target names what it acts on, dispatch_state is intended (decided, not sent) or dispatched (handed over). This is bookkeeping beside the turn, not a turn field. A different operation_key on a turn that already recorded a dispatch is refused unless observed=not_accepted states the earlier one was established never to have landed.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"task_id": str(), "turn_id": integer(1), "request_id": str(), "expected_version": integer(1),
				"expected_turn_version": integer(1),
				"claim_token":           str(),
				"operation_key":         str(), "operation_target": str(), "dispatch_state": str(), "observed": str(),
			}, "required": []string{"task_id", "turn_id", "request_id", "expected_version", "expected_turn_version", "claim_token", "operation_key", "dispatch_state"}, "additionalProperties": false},
		},
	}, workSurfaceTail()...)
}

// workSurfaceTail is everything after the agreed records, in the order a reader
// of tools/list should meet it: the parent's own bookkeeping beside a task, its
// monitoring loops, where this slot's records stand, the snapshots the move
// left behind, and the move itself. A retired slot never offers the last one
// (session_db_mcp.go).
func workSurfaceTail() []map[string]any {
	tail := taskBookToolDefinitions()
	tail = append(tail, monitorToolDefinitions()...)
	tail = append(tail, workStateToolDefinition())
	tail = append(tail, workBackupsToolDefinitions()...)
	return append(tail, workRetireToolDefinition())
}

func validateWork(name string, a workArgs) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if !isWorkTool(name) {
		return bad("unknown work tool")
	}
	if isWorkMutation(name) && !workID.MatchString(a.RequestID) {
		return bad("request_id must be 1..128 letters, digits, _ or -")
	}
	switch name {
	case "task_get", "task_put", "task_delete":
		if !workID.MatchString(a.ID) {
			return bad("id must be 1..128 letters, digits, _ or -")
		}
	case "turn_append", "turn_patch", "turn_list", "turn_dispatch",
		taskBookGetTool, taskBookPutTool:
		if !workID.MatchString(a.TaskID) {
			return bad("task_id must be 1..128 letters, digits, _ or -")
		}
	}
	if name == "task_list" {
		if a.Limit < 0 || a.Limit > workListLimitMax {
			return bad("invalid pagination")
		}
		if a.AfterID != "" && !workID.MatchString(a.AfterID) {
			return bad("invalid after_id")
		}
		if a.Status != "" && !isWorkStatus(a.Status) {
			return bad("status must be " + strings.Join(workStatuses, ", "))
		}
		if a.SessionID != "" && a.SessionID != "none" && !workID.MatchString(a.SessionID) {
			return bad("session_id must be 1..128 letters, digits, _ or - (or \"none\")")
		}
		return nil
	}
	if name == "turn_list" {
		if a.Limit < 0 || a.Limit > workListLimitMax {
			return bad("invalid pagination")
		}
		if a.AfterTurnID != nil && (*a.AfterTurnID < 0 || *a.AfterTurnID > requestSafeInt) {
			return bad("invalid after_turn_id")
		}
		return nil
	}
	if name == workBackupsPruneTool {
		return validateWorkBackupPrune(a)
	}
	switch name {
	case monitorGetTool, monitorListTool, monitorPutTool, monitorDeleteTool:
		return validateMonitorTool(name, a)
	}
	// The reads and the runs that take no version: a state report, a transition
	// run, a snapshot listing. Each one either names no record or holds its own
	// fence rather than a caller's counter.
	switch name {
	case "task_get", workStateTool, workRetireTool, workBackupsTool, taskBookGetTool:
		return nil
	}
	// The bookkeeping lane has its own counter and does not take the task
	// version at all: its notes are about the task, not a new state of it.
	if name != taskBookPutTool {
		if a.ExpectedVersion == nil || *a.ExpectedVersion < 0 || *a.ExpectedVersion > requestSafeInt {
			return bad("a safe nonnegative expected_version is required")
		}
		if name != "task_put" && *a.ExpectedVersion == 0 {
			return bad("expected_version must be the task's current version")
		}
	}
	switch name {
	case "turn_append", "turn_patch", "turn_dispatch":
		if a.ExpectedTurnVersion == nil || *a.ExpectedTurnVersion < 1 || *a.ExpectedTurnVersion > requestSafeInt {
			return bad("expected_turn_version must be the turn_version the task's turns were read at")
		}
	case taskBookPutTool:
		if a.ExpectedBookVersion == nil || *a.ExpectedBookVersion < 1 || *a.ExpectedBookVersion > requestSafeInt {
			return bad("expected_book_version must be the book_version " + taskBookGetTool + " returned")
		}
	}
	if name == "turn_patch" || name == "turn_dispatch" {
		if a.TurnID == nil || *a.TurnID < 1 || *a.TurnID > requestSafeInt {
			return bad("turn_id must be a positive turn id")
		}
	}
	switch name {
	case "task_put":
		return validateWorkTaskInput(a.Task)
	case "turn_append", "turn_patch":
		return validateWorkTurnInput(name, a.Turn)
	case "turn_dispatch":
		if !workID.MatchString(a.ClaimToken) {
			return bad("claim_token must be the token turn_patch returned when this turn was taken")
		}
		return validateWorkDispatch(a)
	case taskBookPutTool:
		return validateWorkLegacyInput(a.Legacy)
	}
	return nil
}

// validateWorkLegacyInput bounds the parent's bookkeeping. The details JSON is
// the parent's own and its shape is not this code's business, but its size is:
// the slot's storage budget is shared with the session's archive, so a record
// cannot become an output archive.
func validateWorkLegacyInput(record *workLegacyInput) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if record == nil {
		return bad("record required")
	}
	if len(record.Stage) > workTitleMax || len(record.Owner) > workTitleMax {
		return bad("stage and owner must each be at most 512 characters")
	}
	if len(record.Sessions) > workLegacySessionMax {
		return bad("a task cannot carry more than 64 session relations")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return bad("record must be JSON")
	}
	if len(encoded) > workLegacyBytesMax {
		return bad("the bookkeeping record must be at most 64KiB — it is a record, not an output archive")
	}
	return nil
}

func validateWorkTaskInput(task *workTaskInput) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if task == nil {
		return bad("task required")
	}
	if task.SessionID != nil && !workID.MatchString(*task.SessionID) {
		return bad("session_id must be 1..128 letters, digits, _ or -, or null")
	}
	if task.Title == nil || strings.TrimSpace(*task.Title) == "" || len(*task.Title) > workTitleMax {
		return bad(fmt.Sprintf("title must be 1..%d characters", workTitleMax))
	}
	for field, value := range map[string]*string{"description": task.Description, "completion_criteria": task.CompletionCriteria} {
		if value == nil {
			return bad(field + " required (an empty string says it is not written down)")
		}
		if len(*value) > workTextMax {
			return bad(fmt.Sprintf("%s must be at most %d characters", field, workTextMax))
		}
	}
	if task.Status == nil || !isWorkStatus(*task.Status) {
		return bad("status must be " + strings.Join(workStatuses, ", "))
	}
	return nil
}

func validateWorkTurnInput(name string, turn *workTurnInput) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if turn == nil {
		return bad("turn required")
	}
	if name == "turn_patch" && len(turn.rawFields) == 0 {
		return bad("turn must name at least one field to change")
	}
	for field, value := range map[string]*string{
		"request": turn.Request, "instruction": turn.Instruction,
		"response": turn.Response, "explanation": turn.Explanation, "question": turn.Question,
	} {
		if value != nil && len(*value) > workTurnTextMax {
			return bad(fmt.Sprintf("%s must be at most %d characters", field, workTurnTextMax))
		}
	}
	if turn.SessionSeq != nil && (*turn.SessionSeq < 0 || *turn.SessionSeq > requestSafeInt) {
		return bad("invalid session_seq")
	}
	for field, value := range map[string]*string{
		"requested_at": turn.RequestedAt, "started_at": turn.StartedAt, "ended_at": turn.EndedAt,
	} {
		if value == nil {
			continue
		}
		if _, err := time.Parse(time.RFC3339, *value); err != nil {
			return bad(field + " must be an RFC3339 time")
		}
	}
	if turn.rawChoices != nil {
		if len(*turn.rawChoices) > workChoiceMax {
			return bad(fmt.Sprintf("at most %d choices", workChoiceMax))
		}
		for _, choice := range *turn.rawChoices {
			if strings.TrimSpace(choice.Label) == "" || len(choice.Label) > workChoiceLabelMax {
				return bad(fmt.Sprintf("choice label must be 1..%d characters", workChoiceLabelMax))
			}
			if strings.TrimSpace(choice.RequestText) == "" || len(choice.RequestText) > workChoiceTextMax {
				return bad(fmt.Sprintf("choice request_text must be 1..%d characters", workChoiceTextMax))
			}
		}
	}
	if turn.Artifacts != nil {
		if len(turn.Artifacts) > workArtifactMax {
			return bad(fmt.Sprintf("at most %d artifacts", workArtifactMax))
		}
		for _, artifact := range turn.Artifacts {
			if strings.TrimSpace(artifact) == "" || len(artifact) > workArtifactTextMax {
				return bad(fmt.Sprintf("an artifact must be 1..%d characters", workArtifactTextMax))
			}
		}
	}
	return nil
}

func validateWorkDispatch(a workArgs) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	if !workID.MatchString(a.OperationKey) {
		return bad("operation_key must be 1..128 letters, digits, _ or -")
	}
	if len(a.OperationTarget) > 512 {
		return bad("operation_target too long")
	}
	if a.DispatchState != dispatchIntended && a.DispatchState != dispatchDispatched {
		return bad("dispatch_state must be intended or dispatched")
	}
	if a.Observed != "" && a.Observed != "not_accepted" {
		return bad("observed, when given, must be not_accepted")
	}
	return nil
}
