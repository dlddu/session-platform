package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The one status a person's judgement puts a task in, and what a slot that
// still holds the older word reads as (AC-N13).
//
// These go through the real entry points on purpose: the session's own `db` MCP
// (tools/call, the arguments a model actually sends), the sidecar's loopback
// views the control plane hops onto, and raw SQL — the last one only to build
// the state an *older* image left, which is the whole thing under test and
// which no tool in this image can write.

// seedStoredStatus writes a task row with a status this image does not write,
// the way an older image left it. It is raw SQL because that is the only way to
// get the old word onto disk: task_put turns it into review.
func seedStoredStatus(t *testing.T, s *sessionDB, id, status string) {
	t.Helper()
	workExec(t, s,
		`INSERT INTO sp_work_tasks
		 (id,session_id,title,description,completion_criteria,status,version,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,1,'2026-11-01T00:00:00Z','2026-11-01T00:00:00Z')`,
		id, "worker-1", "옛 이미지가 적은 작업 "+id, "목적과 요구", "PR 이 머지되면 끝", status)
}

// seedTaskBook writes the parent's own bookkeeping beside a task, the way an
// older image left it.
func seedTaskBook(t *testing.T, s *sessionDB, id string, details map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	workExec(t, s,
		`INSERT INTO sp_work_task_book (task_id,stage,owner,sessions,details) VALUES (?,?,?,'[]',?)
		 ON CONFLICT(task_id) DO UPDATE SET details=excluded.details`,
		id, "진행", "silas", string(encoded))
}

// storedStatusOf reads the byte on disk, which is what "nothing was rewritten"
// means.
func storedStatusOf(t *testing.T, s *sessionDB, id string) string {
	t.Helper()
	row := workRow(t, s, "SELECT status FROM sp_work_tasks WHERE id=?", id)
	if row == nil {
		t.Fatalf("no task %s", id)
	}
	return row["status"].(string)
}

func taskStatusOf(t *testing.T, s *sessionDB, id string) string {
	t.Helper()
	return mustWork(t, s, "task_get", map[string]any{"id": id})["task"].(map[string]any)["status"].(string)
}

func taskVersionOf(t *testing.T, s *sessionDB, id string) float64 {
	t.Helper()
	return mustWork(t, s, "task_get", map[string]any{"id": id})["task"].(map[string]any)["version"].(float64)
}

func taskGuardsOf(t *testing.T, s *sessionDB, id string) (float64, float64) {
	t.Helper()
	got := mustWork(t, s, "task_get", map[string]any{"id": id})
	return got["turn_version"].(float64), got["book_version"].(float64)
}

func taskBookDetails(t *testing.T, s *sessionDB, id string) map[string]any {
	t.Helper()
	record := mustWork(t, s, taskBookGetTool, map[string]any{"task_id": id})
	details, _ := record["details"].(map[string]any)
	if details == nil {
		details = map[string]any{}
	}
	return details
}

// openOneReview takes a task from running to review the way the monitor does,
// with a turn to ask on.
func openOneReview(t *testing.T, s *sessionDB, id, tag string, review map[string]any) map[string]any {
	t.Helper()
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": id, "request_id": "ask-turn-" + tag,
		"turn": map[string]any{"instruction": "무엇이 막혔는지 확인한다"},
	})["turn"].(map[string]any)
	turnVersion, bookVersion := taskGuardsOf(t, s, id)
	return mustWork(t, s, taskReviewOpenTool, map[string]any{
		"task_id": id, "request_id": "open-" + tag, "turn_id": turn["id"],
		"expected_version": taskVersionOf(t, s, id), "expected_turn_version": turnVersion,
		"expected_book_version": bookVersion, "review": review,
	})
}

// resumeOneReview takes a task in review back to running the way the monitor
// does once it has read what came back.
func resumeOneReview(t *testing.T, s *sessionDB, id, tag, evidence, remaining string) map[string]any {
	t.Helper()
	_, bookVersion := taskGuardsOf(t, s, id)
	return mustWork(t, s, taskReviewResumeTool, map[string]any{
		"task_id": id, "request_id": "resume-" + tag,
		"expected_version": taskVersionOf(t, s, id), "expected_book_version": bookVersion,
		"outcome": map[string]any{"evidence": evidence, "remaining": remaining},
	})
}

func repoApproval() map[string]any {
	return map[string]any{
		"target": "dlddu/session-platform", "scope": "contents:read 한 레포, 이 작업 동안만",
		"route": "저장소 소유자가 GitHub App 설치 범위에 레포를 더한다",
		"question": "dlddu/session-platform 읽기 권한을 이 작업 범위로 더해 주시겠습니까? " +
			"지금 상태로는 main 을 받아올 수 없어 대조를 시작할 수 없습니다.",
		"basis": "git ls-remote 가 403 으로 거절됐고, 토큰의 설치 범위에 이 레포가 없다",
	}
}

// The domain carries one status for "a person has to settle this", and the
// older word is an alias on the way in that is never stored.
func TestWorkStatusDomainFoldsBlockedIntoReview(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	if containsString(workStatuses, workStatusBlockedLegacy) {
		t.Fatalf("workStatuses still offers the folded word: %v", workStatuses)
	}
	// An older writer's word is accepted, answered as what landed, and stored
	// as review — so nothing new accumulates under the old name.
	put := mustWork(t, s, "task_put", map[string]any{
		"id": "task-old-writer", "request_id": "c1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-1", "title": "옛 writer 가 만든 작업", "description": "d",
			"completion_criteria": "c", "status": workStatusBlockedLegacy,
		},
	})
	if put["alias_applied"] != workStatusBlockedLegacy {
		t.Fatalf("put did not report the alias: %v", put)
	}
	if got := put["task"].(map[string]any)["status"]; got != workStatusReview {
		t.Fatalf("stored status = %v, want review", got)
	}
	if got := storedStatusOf(t, s, "task-old-writer"); got != workStatusReview {
		t.Fatalf("on disk = %q, want review — the old word must not be written here", got)
	}
	// The task_list schema and the filter both take the alias, and it selects
	// the same work review does.
	for _, filter := range []string{workStatusReview, workStatusBlockedLegacy} {
		page := mustWork(t, s, "task_list", map[string]any{"status": filter})
		tasks := page["tasks"].([]any)
		if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "task-old-writer" {
			t.Fatalf("status=%s selected %v", filter, tasks)
		}
	}
	// And so does the sidecar view the control plane hops onto.
	for _, filter := range []string{workStatusReview, workStatusBlockedLegacy} {
		rec := workViewGet(t, h, sessionDBWorkTasksPath, "status="+filter)
		if rec.Code != http.StatusOK {
			t.Fatalf("view status=%s = %d %s", filter, rec.Code, rec.Body.String())
		}
		var page workTasksPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Tasks) != 1 || page.Tasks[0].Task.Status != workStatusReview {
			t.Fatalf("view status=%s = %+v", filter, page.Tasks)
		}
	}
	// A status that is in neither the domain nor the alias is still refused
	// rather than answered with an empty page.
	wantWorkCode(t, s, "task_list", map[string]any{"status": "held"}, "invalid_argument")
	if rec := workViewGet(t, h, sessionDBWorkTasksPath, "status=held"); rec.Code != http.StatusBadRequest {
		t.Fatalf("view status=held = %d", rec.Code)
	}
	// task_put has no transition constraint, in either direction, and the new
	// word is not treated worse than the legacy alias: both land.
	newWorkTask(t, s, "task-running", "c2", map[string]any{"status": workStatusRunning})
	intoReview := mustWork(t, s, "task_put", map[string]any{
		"id": "task-running", "request_id": "to-review", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "t", "description": "d",
			"completion_criteria": "c", "status": workStatusReview,
		},
	})
	if intoReview["task"].(map[string]any)["status"] != workStatusReview {
		t.Fatalf("task_put into review = %v", intoReview)
	}
	if _, named := intoReview["alias_applied"]; named {
		t.Fatalf("the new word was reported as an alias: %v", intoReview)
	}
	outOfReview := mustWork(t, s, "task_put", map[string]any{
		"id": "task-old-writer", "request_id": "out-of-review", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "t", "description": "d",
			"completion_criteria": "c", "status": workStatusCompleted,
		},
	})
	if outOfReview["task"].(map[string]any)["status"] != workStatusCompleted {
		t.Fatalf("task_put out of review = %v", outOfReview)
	}
	// A review written bare records no decision, which is the whole difference
	// between it and the call that carries one — and it is what a reader is
	// then shown rather than something hidden.
	if details := taskBookDetails(t, s, "task-running"); len(details) != 0 {
		t.Fatalf("a bare review wrote bookkeeping: %v", details)
	}
}

// Registration synthesizes nothing beside the task: no flag that prohibits
// execution, no record that has to be cleared, no approval for ordinary work.
// And planned → running takes task_put alone.
//
// This is here because it is the thing most easily broken by accident — the
// review calls could grow into a gate on every status write, as an earlier
// revision of task_put in fact did. A general feature addition must get none of
// that.
func TestWorkRegistrationSynthesizesNoExecutionGate(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-feature", "reg-1", map[string]any{"status": workStatusPlanned})

	// Exactly two rows exist: the task and its receipt. Every other record in
	// the namespace is still empty, so there is nothing a reader could take for
	// "this task may not start".
	for table, want := range map[string]int64{
		"sp_work_tasks": 1, "sp_work_receipts": 1,
		"sp_work_task_book": 0, "sp_work_task_guard": 0, "sp_work_task_origin": 0,
		"sp_work_turns": 0, "sp_work_turn_claim": 0, "sp_work_turn_admin": 0,
		"sp_work_turn_origin": 0, "sp_work_monitor": 0, "sp_work_retired": 0,
	} {
		if got := workRow(t, s, "SELECT count(*) AS n FROM "+table)["n"].(int64); got != want {
			t.Fatalf("%s has %d rows after one registration, want %d", table, got, want)
		}
	}
	// The row itself says only what the caller wrote.
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-feature"})["task"].(map[string]any)
	if task["status"] != workStatusPlanned {
		t.Fatalf("registered as %v", task["status"])
	}
	for _, field := range []string{"stored_status", "status_basis", "alias_applied"} {
		if _, named := task[field]; named {
			t.Fatalf("registration synthesized %q: %v", field, task)
		}
	}
	// planned → running needs no second call, no approval and no token: the
	// answer carries the task and nothing else.
	moved := mustWork(t, s, "task_put", map[string]any{
		"id": "task-feature", "request_id": "reg-2", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": workStatusRunning,
		},
	})
	if len(moved) != 1 || moved["task"].(map[string]any)["status"] != workStatusRunning {
		t.Fatalf("planned → running = %v", moved)
	}
	// And so does running → completed, for work that never needed a judgement.
	done := mustWork(t, s, "task_put", map[string]any{
		"id": "task-feature", "request_id": "reg-3", "expected_version": 2,
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": workStatusCompleted,
		},
	})
	if done["task"].(map[string]any)["status"] != workStatusCompleted {
		t.Fatalf("running → completed = %v", done)
	}
	// No tool on the offered surface asks for an authorization argument; the
	// only approval-shaped object belongs to the three calls that are about a
	// person's decision.
	for _, tool := range workToolDefinitions(laneMonitor) {
		schema := tool["inputSchema"].(map[string]any)
		required, _ := schema["required"].([]string)
		for _, name := range required {
			if strings.Contains(name, "authoriz") || strings.Contains(name, "execution") {
				t.Fatalf("%v requires %q", tool["name"], name)
			}
		}
		if name, _ := tool["name"].(string); !isReviewTool(name) {
			properties := schema["properties"].(map[string]any)
			if _, named := properties["review"]; named {
				t.Fatalf("%s offers a review argument", name)
			}
		}
	}
}

// The correction the owner's target-state document names: a task recorded
// `completed` when the person had in fact decided nothing goes back to review —
// in one call, carrying their statement and the monitor's check. It is not a
// normal transition, which is why no arrow is drawn for it; what matters is
// that the evidence is not lost on the way.
func TestWorkCompletedIsCorrectedBackIntoReviewInOneCall(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	// The monitor delivered an investigation result and wrote completed without
	// the person having decided anything.
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t1",
		"turn": map[string]any{
			"instruction": "조사만 한다", "response": "조사 결과",
			"explanation": "결과를 전달했다", "ended_at": "2026-11-28T09:00:00Z",
		},
	})
	mustWork(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "done-early", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": workStatusCompleted,
		},
	})
	// The person says they never decided. The monitor checks the turns and puts
	// it back — through the call that records what it is asking, not a bare
	// status write.
	corrected := openOneReview(t, s, "task-a", "correction", map[string]any{
		"target": "조사 결과를 완료로 적은 작업", "scope": "이 작업 하나의 상태 정정",
		"route": "사용자의 정정 진술 + 모니터의 턴 기록 대조",
		"question": "조사 결과를 보셨고 아직 결정하지 않으셨다고 하셨습니다 — " +
			"어느 쪽으로 이어갈까요?",
		"basis": "턴 기록에 사용자의 수락·보류가 없는데 완료로 적혀 있었다",
	})
	if corrected["task"].(map[string]any)["status"] != workStatusReview {
		t.Fatalf("correction = %v", corrected["task"])
	}
	if got := corrected["review"].(map[string]any)["basis"]; got == "" {
		t.Fatalf("the correction lost its basis: %v", corrected["review"])
	}
	// The turns are untouched: the result that was delivered is still the
	// record of what happened.
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	if first := turns[0].(map[string]any); first["response"] != "조사 결과" || first["ended_at"] == nil {
		t.Fatalf("the correction rewrote the turns: %v", first)
	}
}

// A slot an older image wrote resolves each of its `blocked` rows from evidence
// that is already beside it, says what decided, and leaves the byte alone.
func TestWorkLegacyBlockedResolvesFromEvidenceAndIsNeverLost(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	// Make the namespace first, so the seeds land in a database this image has
	// already opened — which is also what a slot that has been served looks
	// like.
	newWorkTask(t, s, "task-a-live", "c0", map[string]any{"status": workStatusRunning})

	// (1) nothing recorded at all — the ambiguous case
	seedStoredStatus(t, s, "task-b-silent", workStatusBlockedLegacy)
	// (2) a recorded, unsettled decision
	seedStoredStatus(t, s, "task-c-review", workStatusBlockedLegacy)
	seedTaskBook(t, s, "task-c-review", map[string]any{
		workReviewKey: map[string]any{"target": "dlddu/session-platform", "applied": false},
	})
	// (3) a technical wait that states all three facts
	seedStoredStatus(t, s, "task-d-wait", workStatusBlockedLegacy)
	seedTaskBook(t, s, "task-d-wait", map[string]any{
		workTechnicalWaitKey: map[string]any{
			"owner": "ci", "due": "2026-11-28T10:00:00Z",
			"retry": map[string]any{"when": "Maven Central 429", "max": 2},
		},
	})
	// (4) a technical wait missing one of them
	seedStoredStatus(t, s, "task-e-halfwait", workStatusBlockedLegacy)
	seedTaskBook(t, s, "task-e-halfwait", map[string]any{
		workTechnicalWaitKey: map[string]any{"owner": "ci", "retry": true},
	})
	// (5) both, which is a disagreement
	seedStoredStatus(t, s, "task-f-disagree", workStatusBlockedLegacy)
	seedTaskBook(t, s, "task-f-disagree", map[string]any{
		workReviewKey: map[string]any{"target": "t", "applied": false},
		workTechnicalWaitKey: map[string]any{
			"owner": "ci", "due": "2026-11-28T10:00:00Z", "retry": true,
		},
	})
	// (6) a wait that says so in prose, which is not a statement
	seedStoredStatus(t, s, "task-g-prose", workStatusBlockedLegacy)
	seedTaskBook(t, s, "task-g-prose", map[string]any{
		workTechnicalWaitKey: "CI 가 도는 중입니다",
		"approval":           "PR only",
	})
	// (7) an open question on a turn, which outranks any wait the bookkeeping
	// claims — it is what the person was actually shown
	seedStoredStatus(t, s, "task-h-asked", workStatusBlockedLegacy)
	seedTaskBook(t, s, "task-h-asked", map[string]any{
		workTechnicalWaitKey: map[string]any{
			"owner": "ci", "due": "2026-11-28T10:00:00Z", "retry": true,
		},
	})
	workExec(t, s,
		`INSERT INTO sp_work_turns (task_id,request,question,choices,artifacts,requested_at)
		 VALUES (?,?,?,'[]','[]','2026-11-01T00:00:00Z')`,
		"task-h-asked", "이거 해 줘", "어느 쪽으로 할까요?")

	want := map[string]struct{ status, basis string }{
		"task-b-silent":   {workStatusReview, "no recorded basis"},
		"task-c-review":   {workStatusReview, "details." + workReviewKey},
		"task-d-wait":     {workStatusRunning, "details." + workTechnicalWaitKey},
		"task-e-halfwait": {workStatusReview, "names no due"},
		"task-f-disagree": {workStatusReview, "disagree"},
		"task-g-prose":    {workStatusReview, "is not a JSON object"},
		"task-h-asked":    {workStatusReview, "a question on an open turn"},
	}
	for id, expected := range want {
		got := mustWork(t, s, "task_get", map[string]any{"id": id})["task"].(map[string]any)
		if got["status"] != expected.status {
			t.Fatalf("%s status = %v, want %s", id, got["status"], expected.status)
		}
		if got["stored_status"] != workStatusBlockedLegacy {
			t.Fatalf("%s did not carry the stored word: %v", id, got)
		}
		basis, _ := got["status_basis"].(string)
		if !strings.Contains(basis, expected.basis) {
			t.Fatalf("%s basis = %q, want it to name %q", id, basis, expected.basis)
		}
		// Nothing was rewritten: the resolution is a reading, not a migration.
		if on := storedStatusOf(t, s, id); on != workStatusBlockedLegacy {
			t.Fatalf("%s on disk = %q — the row must be left exactly as it is", id, on)
		}
	}
	// A row this image wrote needs nothing decided and carries neither field.
	live := mustWork(t, s, "task_get", map[string]any{"id": "task-a-live"})["task"].(map[string]any)
	if _, named := live["stored_status"]; named {
		t.Fatalf("a task written here carries a compatibility field: %v", live)
	}
	if _, named := live["status_basis"]; named {
		t.Fatalf("a task written here carries a compatibility field: %v", live)
	}
	// The same resolution reaches a reader outside the session, with the same
	// two fields — the three hops must not disagree about a status.
	rec := workViewGet(t, h, sessionDBWorkTasksPath, "limit=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("view = %d %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		Tasks []struct {
			Task struct {
				ID           string `json:"id"`
				Status       string `json:"status"`
				StoredStatus string `json:"stored_status"`
				StatusBasis  string `json:"status_basis"`
			} `json:"task"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, record := range raw.Tasks {
		seen[record.Task.ID] = record.Task.Status
		if expected, legacy := want[record.Task.ID]; legacy {
			if record.Task.StoredStatus != workStatusBlockedLegacy || record.Task.StatusBasis == "" {
				t.Fatalf("view %s = %+v, want the stored word and the basis", record.Task.ID, record.Task)
			}
			if record.Task.Status != expected.status {
				t.Fatalf("view %s status = %q, want %q", record.Task.ID, record.Task.Status, expected.status)
			}
		}
	}
	if len(seen) != len(want)+1 {
		t.Fatalf("the view dropped tasks: %v", seen)
	}
	for _, status := range seen {
		if status == workStatusBlockedLegacy || status == "unknown" {
			t.Fatalf("a legacy row was answered as %q: %v", status, seen)
		}
	}
	// The turn page carries the same resolved task, so a reader that opens one
	// task does not see a different status than the list it came from.
	rec = workViewGet(t, h, sessionDBWorkTurnsPath, "task_id=task-d-wait")
	if rec.Code != http.StatusOK {
		t.Fatalf("turn view = %d %s", rec.Code, rec.Body.String())
	}
	var turnPage struct {
		Task struct {
			Status       string `json:"status"`
			StoredStatus string `json:"stored_status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &turnPage); err != nil {
		t.Fatal(err)
	}
	if turnPage.Task.Status != workStatusRunning || turnPage.Task.StoredStatus != workStatusBlockedLegacy {
		t.Fatalf("turn page task = %+v", turnPage.Task)
	}
}

// The statuses partition the tasks: paging each one in turn sees every task
// once and no task twice, legacy rows included, across several pages.
func TestWorkLegacyBlockedFiltersPartitionTheTasksAcrossPages(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-01", "c1", map[string]any{"status": workStatusPlanned})
	newWorkTask(t, s, "task-02", "c2", map[string]any{"status": workStatusRunning})
	newWorkTask(t, s, "task-03", "c3", map[string]any{"status": workStatusCompleted})
	newWorkTask(t, s, "task-04", "c4", map[string]any{"status": "unknown"})
	// Four legacy rows: two that read as review and two as running, interleaved
	// with the live ones so a page boundary falls between them.
	for _, id := range []string{"task-05", "task-07"} {
		seedStoredStatus(t, s, id, workStatusBlockedLegacy)
	}
	for _, id := range []string{"task-06", "task-08"} {
		seedStoredStatus(t, s, id, workStatusBlockedLegacy)
		seedTaskBook(t, s, id, map[string]any{
			workTechnicalWaitKey: map[string]any{
				"owner": "ci", "due": "2026-11-28T10:00:00Z", "retry": true,
			},
		})
	}
	all := map[string]int{}
	pageAll := func(filter string, limit int) []string {
		ids := []string{}
		cursor := ""
		for round := 0; round < 20; round++ {
			args := map[string]any{"limit": limit}
			if filter != "" {
				args["status"] = filter
			}
			if cursor != "" {
				args["after_id"] = cursor
			}
			page := mustWork(t, s, "task_list", args)
			for _, entry := range page["tasks"].([]any) {
				ids = append(ids, entry.(map[string]any)["id"].(string))
			}
			cursor = page["next_cursor"].(string)
			if cursor == "" {
				return ids
			}
		}
		t.Fatalf("status=%q never reached the end", filter)
		return nil
	}
	// Every bucket, one page at a time, at a limit that forces the refill path.
	for _, status := range workStatuses {
		for _, id := range pageAll(status, 1) {
			all[id]++
		}
	}
	if len(all) != 8 {
		t.Fatalf("the buckets do not cover the tasks: %v", all)
	}
	for id, count := range all {
		if count != 1 {
			t.Fatalf("%s appeared in %d buckets — the statuses must partition", id, count)
		}
	}
	// The unfiltered page sees the same set, and the alias sees exactly what
	// review sees.
	if got := pageAll("", 3); len(got) != 8 {
		t.Fatalf("unfiltered = %v", got)
	}
	review := strings.Join(pageAll(workStatusReview, 2), ",")
	if alias := strings.Join(pageAll(workStatusBlockedLegacy, 2), ","); alias != review {
		t.Fatalf("alias paged %q, review paged %q", alias, review)
	}
	if review != "task-05,task-07" {
		t.Fatalf("review = %q, want the two legacy rows with no basis", review)
	}
	if running := strings.Join(pageAll(workStatusRunning, 2), ","); running != "task-02,task-06,task-08" {
		t.Fatalf("running = %q", running)
	}
	// A page that comes back full is not the end by itself: the cursor says.
	first := mustWork(t, s, "task_list", map[string]any{"status": workStatusRunning, "limit": 2})
	if len(first["tasks"].([]any)) != 2 || first["next_cursor"] != "task-06" {
		t.Fatalf("first running page = %v", first)
	}
	last := mustWork(t, s, "task_list", map[string]any{
		"status": workStatusRunning, "limit": 2, "after_id": "task-06",
	})
	if len(last["tasks"].([]any)) != 1 || last["next_cursor"] != "" {
		t.Fatalf("last running page = %v", last)
	}
}

// The common path: a new task finds it cannot read the repository, one review
// goes in front of the person with the real target, a pick or a submitted
// request moves nothing by itself, the confirmed approval resumes it, and the
// accepted result with the criteria met completes it.
func TestWorkReviewIsOneApprovalFromPermissionToCompletion(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})

	opened := openOneReview(t, s, "task-a", "a", repoApproval())
	if opened["already_open"] != false {
		t.Fatalf("first open = %v", opened)
	}
	task := opened["task"].(map[string]any)
	if task["status"] != workStatusReview {
		t.Fatalf("status = %v, want review", task["status"])
	}
	record := opened["review"].(map[string]any)
	for field, want := range map[string]string{
		"target": "dlddu/session-platform", "route": "저장소 소유자가 GitHub App 설치 범위에 레포를 더한다",
	} {
		if record[field] != want {
			t.Fatalf("review.%s = %v", field, record[field])
		}
	}
	// The question is on the turn, verbatim.
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	asked := turns[len(turns)-1].(map[string]any)
	if asked["question"] != repoApproval()["question"] {
		t.Fatalf("question = %v — it is stored as written", asked["question"])
	}

	// Asking the same decision again is not a second approval, even under a new
	// request_id, and it writes nothing: the counters do not move.
	turnVersion, bookVersion := taskGuardsOf(t, s, "task-a")
	version := taskVersionOf(t, s, "task-a")
	again := mustWork(t, s, taskReviewOpenTool, map[string]any{
		"task_id": "task-a", "request_id": "open-again", "turn_id": asked["id"],
		"expected_version": version, "expected_turn_version": turnVersion,
		"expected_book_version": bookVersion, "review": repoApproval(),
	})
	if again["already_open"] != true {
		t.Fatalf("the same decision was opened twice: %v", again)
	}
	if gotTurn, gotBook := taskGuardsOf(t, s, "task-a"); gotTurn != turnVersion || gotBook != bookVersion {
		t.Fatalf("a duplicate open moved the counters: %v/%v", gotTurn, gotBook)
	}
	if taskVersionOf(t, s, "task-a") != version {
		t.Fatalf("a duplicate open moved the task version")
	}
	// A different decision while one is open is refused rather than stacked.
	other := repoApproval()
	other["target"] = "dlddu/pocket-aide"
	_, code := workCall(t, s, taskReviewOpenTool, map[string]any{
		"task_id": "task-a", "request_id": "open-other", "turn_id": asked["id"],
		"expected_version": version, "expected_turn_version": turnVersion,
		"expected_book_version": bookVersion, "review": other,
	})
	if code != "conflict" {
		t.Fatalf("a second decision = %q, want conflict", code)
	}

	// A person submitting a request — picked from the choices or typed — becomes
	// the next turn and moves nothing. The monitor reading it is the separate
	// step.
	body, err := json.Marshal(workRequestBody{
		ID: "req-1", TaskID: "task-a", Text: "네, 더해 뒀습니다", Author: "silas",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, sessionDBWorkRequestsPath, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request = %d %s", rec.Code, rec.Body.String())
	}
	if got := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)["status"]; got != workStatusReview {
		t.Fatalf("a submitted request moved the task to %v on its own", got)
	}

	// A resume that names no confirmation is refused and the task stays in
	// review — this is the "approval not applied" case.
	version = taskVersionOf(t, s, "task-a")
	_, bookVersion = taskGuardsOf(t, s, "task-a")
	wantWorkCode(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-a", "request_id": "resume-empty",
		"expected_version": version, "expected_book_version": bookVersion,
		"outcome": map[string]any{"evidence": "  ", "remaining": "대조를 이어 간다"},
	}, "invalid_argument")
	if got := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)["status"]; got != workStatusReview {
		t.Fatalf("a refused resume moved the task to %v", got)
	}
	// So is one that says nothing is left: that is a completion, not a resume.
	wantWorkCode(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-a", "request_id": "resume-nothing-left",
		"expected_version": version, "expected_book_version": bookVersion,
		"outcome": map[string]any{"evidence": "git ls-remote 가 이제 답한다", "remaining": ""},
	}, "invalid_argument")

	resumed := mustWork(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-a", "request_id": "resume-1",
		"expected_version": version, "expected_book_version": bookVersion,
		"outcome": map[string]any{
			"evidence":  "git ls-remote 가 200 으로 답하고 설치 범위에 레포가 보인다",
			"remaining": "main 과 대조하고 구현을 올린다",
		},
	})
	if resumed["task"].(map[string]any)["status"] != workStatusRunning {
		t.Fatalf("resumed = %v", resumed["task"])
	}
	// What the review was about is kept, with the confirmation beside it, and
	// the open record is gone so the next ask is not a duplicate of a settled
	// one.
	details := taskBookDetails(t, s, "task-a")
	if _, open := details[workReviewKey]; open {
		t.Fatalf("the settled review is still open: %v", details)
	}
	history := details[workReviewHistoryKey].([]any)
	if len(history) != 1 {
		t.Fatalf("history = %v", history)
	}
	entry := history[0].(map[string]any)
	if entry["to_status"] != workStatusRunning || entry["evidence"] == "" {
		t.Fatalf("history entry = %v", entry)
	}
	if kept := entry["review"].(map[string]any); kept["target"] != "dlddu/session-platform" {
		t.Fatalf("the review it closed was not kept: %v", kept)
	}
	// The turns are exactly as they were: the question stays asked.
	turns = mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	if turns[0].(map[string]any)["question"] == nil && len(turns) < 2 {
		t.Fatalf("the resume rewrote the turns: %v", turns)
	}

	// Completion: the open request has to be closed first, the criteria have to
	// be the task's own, and the result has to have been accepted.
	version = taskVersionOf(t, s, "task-a")
	_, bookVersion = taskGuardsOf(t, s, "task-a")
	criteria := "PR 이 머지되면 끝"
	// Still in running — completion is for a task in review.
	wantWorkCode(t, s, taskReviewCompleteTool, map[string]any{
		"task_id": "task-a", "request_id": "done-too-early",
		"expected_version": version, "expected_book_version": bookVersion,
		"outcome": map[string]any{
			"accepted": true, "completion_criteria": criteria,
			"evidence": "사용자가 수락했다",
		},
	}, "conflict")
}

// A completion is about *this* task's criteria and *this* task's open work, and
// an unapplied decision outranks all of it.
func TestWorkReviewCompletionChecksTheCriteriaAndTheOpenWork(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	openOneReview(t, s, "task-a", "a", repoApproval())
	criteria := "PR 이 머지되면 끝"
	accepted := "사용자가 「이대로 좋습니다」라고 답한 턴"
	attempt := func(requestID string, outcome map[string]any) (map[string]any, string) {
		_, bookVersion := taskGuardsOf(t, s, "task-a")
		return workCall(t, s, taskReviewCompleteTool, map[string]any{
			"task_id": "task-a", "request_id": requestID,
			"expected_version": taskVersionOf(t, s, "task-a"), "expected_book_version": bookVersion,
			"outcome": outcome,
		})
	}
	// A completion that cannot say how the acceptance was observed is a claim
	// rather than a record, whatever else is in order.
	decoded, code := attempt("done-no-evidence", map[string]any{
		"accepted": true, "completion_criteria": criteria,
	})
	if code != "invalid_argument" || !strings.Contains(decoded["error"].(string), "observed") {
		t.Fatalf("completion with no named acceptance = %q %v", code, decoded)
	}
	if got := taskStatusOf(t, s, "task-a"); got != workStatusReview {
		t.Fatalf("a refused completion moved the task to %v", got)
	}
	resumeOneReview(t, s, "task-a", "a", "소유자가 설치 범위에 레포를 더했다", "구현을 올린다")

	// An older writer's word is the one door into review that records no
	// decision, which is exactly the state the rest of the checks are about.
	mustWork(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "legacy-hold", "expected_version": taskVersionOf(t, s, "task-a"),
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": criteria, "status": workStatusBlockedLegacy,
		},
	})
	if got := taskStatusOf(t, s, "task-a"); got != workStatusReview {
		t.Fatalf("the alias did not land in review: %v", got)
	}
	// An unanswered request is work nobody did; a completion over it would
	// claim otherwise.
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "open-request",
		"turn": map[string]any{"request": "아직 답하지 않은 요청"},
	})["turn"].(map[string]any)
	decoded, code = attempt("done-open-request", map[string]any{
		"accepted": true, "completion_criteria": criteria, "evidence": accepted,
	})
	if code != "conflict" || !strings.Contains(decoded["error"].(string), "ended_at") {
		t.Fatalf("completion over an open request = %q %v", code, decoded)
	}
	// A result that was not accepted, criteria that are not this task's, and
	// something left are each refused on their own.
	if _, code = attempt("done-unaccepted", map[string]any{
		"accepted": false, "completion_criteria": criteria, "evidence": accepted,
	}); code != "invalid_argument" {
		t.Fatalf("unaccepted completion = %q", code)
	}
	if _, code = attempt("done-wrong-criteria", map[string]any{
		"accepted": true, "completion_criteria": "대충 다 됐으면 끝", "evidence": accepted,
	}); code != "conflict" {
		t.Fatalf("completion against other criteria = %q", code)
	}
	if _, code = attempt("done-remaining", map[string]any{
		"accepted": true, "completion_criteria": criteria, "evidence": accepted,
		"remaining": "배포가 남았다",
	}); code != "invalid_argument" {
		t.Fatalf("completion with work left = %q", code)
	}
	// Close the request, and the completion lands.
	turnVersion, _ := taskGuardsOf(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turn["id"], "request_id": "close-request",
		"expected_version": taskVersionOf(t, s, "task-a"), "expected_turn_version": turnVersion,
		"turn": map[string]any{
			"response": "세션이 쓴 최종 답", "explanation": "사람에게 쓰는 설명",
			"ended_at": "2026-11-28T09:00:00Z",
		},
	})
	done, code := attempt("done-1", map[string]any{
		"accepted": true, "completion_criteria": criteria, "evidence": accepted,
	})
	if code != "" {
		t.Fatalf("completion = %q %v", code, done)
	}
	if got := done["task"].(map[string]any)["status"]; got != workStatusCompleted {
		t.Fatalf("completed = %v", done["task"])
	}
	entry := done["closed"].(map[string]any)
	if entry["to_status"] != workStatusCompleted || entry["accepted"] != true {
		t.Fatalf("closed = %v", entry)
	}
	// What held the task is kept: two entries, the recorded decision and the
	// legacy reading that followed it.
	history := taskBookDetails(t, s, "task-a")[workReviewHistoryKey].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %v", history)
	}
}

// A legacy row leaves review through the same calls, and the only account of
// why it was ever held — the stored word and the field that read it — is kept
// rather than erased by the move.
func TestWorkReviewOnALegacyBlockedTaskKeepsWhyItWasHeld(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-live", "c0", nil)
	seedStoredStatus(t, s, "task-legacy", workStatusBlockedLegacy)
	version := taskVersionOf(t, s, "task-legacy")
	_, bookVersion := taskGuardsOf(t, s, "task-legacy")
	resumed := mustWork(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-legacy", "request_id": "resume-legacy",
		"expected_version": version, "expected_book_version": bookVersion,
		"outcome": map[string]any{
			"evidence":  "소유자가 2026-11-20 에 승인했고 기장에 적히지 않았다",
			"remaining": "원래 작업을 이어 간다",
		},
	})
	moved := resumed["task"].(map[string]any)
	if moved["status"] != workStatusRunning {
		t.Fatalf("resumed = %v", moved)
	}
	if on := storedStatusOf(t, s, "task-legacy"); on != workStatusRunning {
		t.Fatalf("on disk = %q — the resume is a real write", on)
	}
	entry := taskBookDetails(t, s, "task-legacy")[workReviewHistoryKey].([]any)[0].(map[string]any)
	kept := entry["review"].(map[string]any)
	if kept["from_status"] != workStatusBlockedLegacy || kept["status_basis"] != "no recorded basis" {
		t.Fatalf("the legacy reading was not kept: %v", kept)
	}
	// Opening a review on a legacy row records the same fact.
	seedStoredStatus(t, s, "task-legacy-2", workStatusBlockedLegacy)
	opened := openOneReview(t, s, "task-legacy-2", "legacy-2", repoApproval())
	if got := opened["review"].(map[string]any)["from_status"]; got != workStatusBlockedLegacy {
		t.Fatalf("open on a legacy row did not keep the stored word: %v", opened["review"])
	}
	if on := storedStatusOf(t, s, "task-legacy-2"); on != workStatusReview {
		t.Fatalf("on disk = %q, want review", on)
	}
}

// A pre-approved technical wait is nobody's question: it stays running with its
// own basis, and the call that puts a decision in front of a person refuses the
// shape a wait would send.
func TestWorkTechnicalWaitStaysRunningWithItsBasis(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-ci", "c1", map[string]any{"status": workStatusRunning})
	version := taskVersionOf(t, s, "task-ci")
	_, bookVersion := taskGuardsOf(t, s, "task-ci")
	wait := map[string]any{
		"owner": "ci", "due": "2026-11-28T10:00:00Z",
		"retry": map[string]any{"when": "Maven Central 429", "max": 2},
	}
	mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-ci", "request_id": "wait-1", "expected_book_version": bookVersion,
		"record": map[string]any{
			"stage": "CI 대기", "owner": "ci", "sessions": []any{},
			"details": map[string]any{workTechnicalWaitKey: wait},
		},
	})
	// The bookkeeping lane does not move the task: a wait is recorded, not a
	// status change.
	got := mustWork(t, s, "task_get", map[string]any{"id": "task-ci"})["task"].(map[string]any)
	if got["status"] != workStatusRunning || got["version"].(float64) != version {
		t.Fatalf("recording a wait moved the task: %v", got)
	}
	if _, named := got["stored_status"]; named {
		t.Fatalf("a running task carries a compatibility field: %v", got)
	}
	// The basis survives a round trip, with owner, due and the retry condition.
	kept := taskBookDetails(t, s, "task-ci")[workTechnicalWaitKey].(map[string]any)
	if kept["owner"] != "ci" || kept["due"] != "2026-11-28T10:00:00Z" {
		t.Fatalf("the wait lost its basis: %v", kept)
	}
	if kept["retry"].(map[string]any)["when"] != "Maven Central 429" {
		t.Fatalf("the wait lost its retry condition: %v", kept)
	}
	// A review with no route and no question is not something a person can act
	// on, so the shape a technical wait would send is refused.
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-ci", "request_id": "t1", "turn": map[string]any{"instruction": "CI 를 기다린다"},
	})["turn"].(map[string]any)
	turnVersion, bookVersion := taskGuardsOf(t, s, "task-ci")
	wantWorkCode(t, s, taskReviewOpenTool, map[string]any{
		"task_id": "task-ci", "request_id": "no-route", "turn_id": turn["id"],
		"expected_version": taskVersionOf(t, s, "task-ci"), "expected_turn_version": turnVersion,
		"expected_book_version": bookVersion,
		"review": map[string]any{
			"target": "CI", "scope": "이 작업", "route": "", "question": "", "basis": "CI 가 돈다",
		},
	}, "invalid_argument")
	// A task with nothing to decide is not in review.
	page := mustWork(t, s, "task_list", map[string]any{"status": workStatusReview})
	if len(page["tasks"].([]any)) != 0 {
		t.Fatalf("a technical wait was put in front of a person: %v", page)
	}
}

// The pre-approved path that never needed anybody's judgement is untouched:
// task_put alone carries it from planned to completed.
func TestWorkPreApprovedPathNeedsNoJudgement(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	for version, status := range []string{workStatusRunning, workStatusCompleted} {
		mustWork(t, s, "task_put", map[string]any{
			"id": "task-a", "request_id": "move-" + status, "expected_version": version + 1,
			"task": map[string]any{
				"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
				"completion_criteria": "PR 이 머지되면 끝", "status": status,
			},
		})
	}
	got := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)
	if got["status"] != workStatusCompleted || got["version"].(float64) != 3 {
		t.Fatalf("pre-approved path = %v", got)
	}
}

// Two parents holding the same counters cannot both open a review, and the
// loser's retry — after re-reading, which is what it is told to do — finds the
// decision already open rather than making a second one.
func TestWorkReviewOpenHasOneWinnerAndTheLoserDoesNotDuplicate(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t1",
		"turn": map[string]any{"instruction": "권한을 확인한다"},
	})["turn"].(map[string]any)
	turnVersion, bookVersion := taskGuardsOf(t, s, "task-a")
	firstTurnVersion, firstBookVersion := turnVersion, bookVersion
	version := taskVersionOf(t, s, "task-a")
	open := func(requestID string) (map[string]any, string) {
		return workCall(t, s, taskReviewOpenTool, map[string]any{
			"task_id": "task-a", "request_id": requestID, "turn_id": turn["id"],
			"expected_version": version, "expected_turn_version": turnVersion,
			"expected_book_version": bookVersion, "review": repoApproval(),
		})
	}
	if _, code := open("parent-1"); code != "" {
		t.Fatalf("the first parent lost: %s", code)
	}
	// The second parent is still holding the counters it read before the first
	// wrote. It does not get through and it does not get a conflict either: the
	// decision it is asking about is already in front of the person, which is
	// the answer it needed — and that is settled *before* any counter is taken,
	// so a stale holder cannot stack an approval by racing.
	second, code := open("parent-2")
	if code != "" || second["already_open"] != true {
		t.Fatalf("the second parent = %q %v, want already_open", code, second)
	}
	// A *different* decision from a stale holder is refused, which is where the
	// counters do the excluding.
	other := repoApproval()
	other["target"] = "dlddu/pocket-aide"
	otherResult, code := workCall(t, s, taskReviewOpenTool, map[string]any{
		"task_id": "task-a", "request_id": "parent-2-other", "turn_id": turn["id"],
		"expected_version": version, "expected_turn_version": turnVersion,
		"expected_book_version": bookVersion, "review": other,
	})
	if code != "conflict" {
		t.Fatalf("a different decision from a stale holder = %q %v", code, otherResult)
	}
	// A write that really has to move a counter is refused on the stale value.
	if _, code = workCall(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-a", "request_id": "parent-2-resume",
		"expected_version": version, "expected_book_version": bookVersion,
		"outcome": map[string]any{"evidence": "e", "remaining": "r"},
	}); code != "conflict" {
		t.Fatalf("a stale resume = %q, want conflict", code)
	}
	// Reading again and retrying — what the refusal tells it to do — still
	// finds the decision open, so there is no second approval and no second
	// turn.
	turnVersion, bookVersion = taskGuardsOf(t, s, "task-a")
	retry := mustWork(t, s, taskReviewOpenTool, map[string]any{
		"task_id": "task-a", "request_id": "parent-2-retry", "turn_id": turn["id"],
		"expected_version": taskVersionOf(t, s, "task-a"), "expected_turn_version": turnVersion,
		"expected_book_version": bookVersion, "review": repoApproval(),
	})
	if retry["already_open"] != true {
		t.Fatalf("the retry opened a second review: %v", retry)
	}
	if turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any); len(turns) != 1 {
		t.Fatalf("turns = %d, want the one the question was asked on", len(turns))
	}
	history, named := taskBookDetails(t, s, "task-a")[workReviewHistoryKey]
	if named {
		t.Fatalf("an open review was closed by a retry: %v", history)
	}
	// A replay of the winner's own request_id — the same arguments it sent —
	// answers what it committed and moves nothing.
	replay := mustWork(t, s, taskReviewOpenTool, map[string]any{
		"task_id": "task-a", "request_id": "parent-1", "turn_id": turn["id"],
		"expected_version": version, "expected_turn_version": firstTurnVersion,
		"expected_book_version": firstBookVersion, "review": repoApproval(),
	})
	if replay["task"].(map[string]any)["status"] != workStatusReview {
		t.Fatalf("replay = %v", replay)
	}
	if got := taskVersionOf(t, s, "task-a"); got != version+1 {
		t.Fatalf("the replay moved the task version to %v", got)
	}
}

// Opening the same database again changes nothing about the legacy rows: the
// namespace step is idempotent and the resolution is a reading, not a one-off
// that has to run.
func TestWorkLegacyBlockedSurvivesReopeningTheSlot(t *testing.T) {
	dir := t.TempDir()
	first, err := newSessionDB(dir, false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	newWorkTask(t, first, "task-live", "c0", nil)
	seedStoredStatus(t, first, "task-legacy", workStatusBlockedLegacy)
	seedTaskBook(t, first, "task-legacy", map[string]any{
		workTechnicalWaitKey: map[string]any{
			"owner": "ci", "due": "2026-11-28T10:00:00Z", "retry": true,
		},
	})
	before := mustWork(t, first, "task_list", map[string]any{"limit": 100})
	versionBefore := workRow(t, first, "SELECT version AS v FROM sp_work_meta")["v"]
	first.Close()

	second, err := newSessionDB(dir, false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	after := mustWork(t, second, "task_list", map[string]any{"limit": 100})
	if versionAfter := workRow(t, second, "SELECT version AS v FROM sp_work_meta")["v"]; versionAfter != versionBefore {
		t.Fatalf("the schema version moved on reopen: %v -> %v", versionBefore, versionAfter)
	}
	if storedStatusOf(t, second, "task-legacy") != workStatusBlockedLegacy {
		t.Fatalf("reopening rewrote the legacy row")
	}
	encode := func(page map[string]any) string {
		raw, marshalErr := json.Marshal(page)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return string(raw)
	}
	if encode(before) != encode(after) {
		t.Fatalf("the page changed on reopen:\n%s\n%s", encode(before), encode(after))
	}
	got := mustWork(t, second, "task_get", map[string]any{"id": "task-legacy"})["task"].(map[string]any)
	if got["status"] != workStatusRunning || got["stored_status"] != workStatusBlockedLegacy {
		t.Fatalf("after reopen = %v", got)
	}
}

// Bookkeeping this image cannot parse is left exactly as it is, and the row it
// belongs to still answers — on the side a person looks at.
func TestWorkUnreadableBookkeepingIsLeftAloneAndStillAnswers(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-live", "c0", nil)
	seedStoredStatus(t, s, "task-broken", workStatusBlockedLegacy)
	workExec(t, s,
		`INSERT INTO sp_work_task_book (task_id,stage,owner,sessions,details) VALUES (?,?,?,'[]',?)`,
		"task-broken", "진행", "silas", "{not json")
	got := mustWork(t, s, "task_get", map[string]any{"id": "task-broken"})["task"].(map[string]any)
	if got["status"] != workStatusReview {
		t.Fatalf("unreadable bookkeeping resolved to %v, want review", got["status"])
	}
	if basis, _ := got["status_basis"].(string); !strings.Contains(basis, "not readable JSON") {
		t.Fatalf("basis = %q", basis)
	}
	if kept := workRow(t, s, "SELECT details AS d FROM sp_work_task_book WHERE task_id=?", "task-broken"); kept["d"] != "{not json" {
		t.Fatalf("the unreadable record was rewritten: %v", kept)
	}
	// A write through it is refused rather than overwriting what may be the
	// only copy.
	version := taskVersionOf(t, s, "task-broken")
	wantWorkCode(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-broken", "request_id": "resume-broken",
		"expected_version": version, "expected_book_version": 1,
		"outcome": map[string]any{"evidence": "e", "remaining": "r"},
	}, "conflict")
}

// The three moves over the wire a model actually speaks: JSON-RPC `tools/call`
// on the sidecar's own MCP address, with the request arriving on the sidecar's
// loopback route in between. Everything else in this file reaches the tools one
// layer in; this is here so "the real entry point" is a claim with a test
// behind it rather than an assumption about the layer above.
func TestWorkReviewMovesOverTheSidecarWire(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		resp := sessionDBCall(t, h, "tools/call", map[string]any{"name": name, "arguments": args})
		if resp.Error != nil {
			t.Fatalf("%s: JSON-RPC error %+v", name, resp.Error)
		}
		result := resp.Result.(map[string]any)
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatalf("%s: %v (%s)", name, err, text)
		}
		return decoded, result["isError"].(bool)
	}
	// The one tool list a model is offered carries all three moves.
	listed := strings.Join(sessionDBToolNames(t, h), ",")
	for _, name := range []string{taskReviewOpenTool, taskReviewResumeTool, taskReviewCompleteTool} {
		if !strings.Contains(listed, name) {
			t.Fatalf("%s is not on the offered surface: %s", name, listed)
		}
	}
	criteria := "권한이 붙고 대조가 끝나면 끝"
	if _, isErr := call("task_put", map[string]any{
		"id": "task-wire", "request_id": "c1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-1", "title": "와이어로 도는 작업", "description": "d",
			"completion_criteria": criteria, "status": workStatusRunning,
		},
	}); isErr {
		t.Fatal("task_put over the wire failed")
	}
	appended, isErr := call("turn_append", map[string]any{
		"task_id": "task-wire", "request_id": "t1", "expected_version": 1, "expected_turn_version": 1,
		"turn": map[string]any{"instruction": "레포 읽기 권한을 확인한다"},
	})
	if isErr {
		t.Fatalf("turn_append over the wire = %v", appended)
	}
	turnID := appended["turn"].(map[string]any)["id"]
	opened, isErr := call(taskReviewOpenTool, map[string]any{
		"task_id": "task-wire", "request_id": "open-1", "turn_id": turnID,
		"expected_version": 1, "expected_turn_version": 2, "expected_book_version": 1,
		"review": repoApproval(),
	})
	if isErr || opened["task"].(map[string]any)["status"] != workStatusReview {
		t.Fatalf("open over the wire = %v", opened)
	}
	// A person's request arrives on the sidecar's own loopback route and makes
	// the next turn without moving the status.
	body, err := json.Marshal(workRequestBody{
		ID: "req-wire", TaskID: "task-wire", Text: "더해 뒀습니다", Author: "silas",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, sessionDBWorkRequestsPath, strings.NewReader(string(body)))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("request over the wire = %d %s", rec.Code, rec.Body.String())
	}
	read, _ := call("task_get", map[string]any{"id": "task-wire"})
	if read["task"].(map[string]any)["status"] != workStatusReview {
		t.Fatalf("the request moved the status: %v", read["task"])
	}
	version := read["task"].(map[string]any)["version"]
	resumed, isErr := call(taskReviewResumeTool, map[string]any{
		"task_id": "task-wire", "request_id": "resume-1",
		"expected_version": version, "expected_book_version": read["book_version"],
		"outcome": map[string]any{"evidence": "ls-remote 가 답한다", "remaining": "대조를 끝낸다"},
	})
	if isErr || resumed["task"].(map[string]any)["status"] != workStatusRunning {
		t.Fatalf("resume over the wire = %v", resumed)
	}
}

// receiveRequestState puts a task into the state a stored request leaves behind:
// the review is still open and recorded, and the status byte has moved to
// `planned` — received, nobody has picked it up yet.
//
// It is built with task_put rather than through the request route so that this
// file does not depend on the commit that introduces that move (#285,
// session_work_views.go). The state is the same one: the status is `planned`,
// the version has moved with it, and the recorded review is untouched. The real
// route was walked in a combined tree; what is pinned here is this slot's half.
func receiveRequestState(t *testing.T, s *sessionDB, id, tag string, review map[string]any) {
	t.Helper()
	openOneReview(t, s, id, tag, review)
	mustWork(t, s, "task_put", map[string]any{
		"id": id, "request_id": "receipt-" + tag, "expected_version": taskVersionOf(t, s, id),
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": workStatusPlanned,
		},
	})
	if got := taskStatusOf(t, s, id); got != workStatusPlanned {
		t.Fatalf("receipt state = %q, want planned", got)
	}
	if _, open := taskBookDetails(t, s, id)[workReviewKey]; !open {
		t.Fatalf("the receipt state lost the open review")
	}
}

// A review is left by the record it is, not by the word the status happens to
// hold. A stored request moves a task out of `review` and into `planned` in the
// transaction that stores it, and the decision it was waiting on is not settled
// by somebody sending a sentence — so the monitor has to be able to close its
// own review from there.
func TestWorkReviewIsSettledFromTheReceiptStatus(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	receiveRequestState(t, s, "task-a", "a", repoApproval())

	// Submission alone is not a resume: a call that names no confirmation is
	// refused and the task stays at the receipt with its review still open.
	_, bookVersion := taskGuardsOf(t, s, "task-a")
	wantWorkCode(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-a", "request_id": "resume-empty",
		"expected_version": taskVersionOf(t, s, "task-a"), "expected_book_version": bookVersion,
		"outcome": map[string]any{"evidence": "  ", "remaining": "이어 간다"},
	}, "invalid_argument")
	if got := taskStatusOf(t, s, "task-a"); got != workStatusPlanned {
		t.Fatalf("a refused resume moved the receipt to %q", got)
	}
	if _, open := taskBookDetails(t, s, "task-a")[workReviewKey]; !open {
		t.Fatalf("a refused resume closed the review")
	}
	// A deferral is the same: the person left words, the permission is not in
	// force, and nothing here counts that as execution.
	wantWorkCode(t, s, taskReviewResumeTool, map[string]any{
		"task_id": "task-a", "request_id": "resume-deferred",
		"expected_version": taskVersionOf(t, s, "task-a"), "expected_book_version": bookVersion,
		"outcome": map[string]any{"evidence": "승인하겠다고 말씀하셨다", "remaining": ""},
	}, "invalid_argument")
	if got := taskStatusOf(t, s, "task-a"); got != workStatusPlanned {
		t.Fatalf("a deferral moved the receipt to %q", got)
	}

	// The monitor reads what came back, confirms the decision is in force and
	// that there is a follow-on, and closes it. From `planned`.
	resumed := resumeOneReview(t, s, "task-a", "a",
		"ls-remote 가 200 으로 답하고 설치 범위에 레포가 보인다", "대조를 끝내고 구현을 올린다")
	if resumed["task"].(map[string]any)["status"] != workStatusRunning {
		t.Fatalf("resume from the receipt = %v", resumed["task"])
	}
	details := taskBookDetails(t, s, "task-a")
	if _, open := details[workReviewKey]; open {
		t.Fatalf("the settled review is still open: %v", details)
	}
	history := details[workReviewHistoryKey].([]any)
	if len(history) != 1 {
		t.Fatalf("history = %v", history)
	}
	entry := history[0].(map[string]any)
	// Where it was settled from is the only record that the decision was closed
	// out of the queue rather than out of review.
	if entry["from_status"] != workStatusPlanned {
		t.Fatalf("history entry does not say where it was settled from: %v", entry)
	}
	if kept := entry["review"].(map[string]any); kept["target"] != "dlddu/session-platform" {
		t.Fatalf("the review it closed was not kept: %v", kept)
	}
	// The exchanges are untouched: the question stays asked, its choices stay
	// beside it, and the turn is not closed.
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	asked := turns[0].(map[string]any)
	if asked["question"] == nil || asked["ended_at"] != nil {
		t.Fatalf("the resume rewrote what was asked: %v", asked)
	}
}

// The other way out, from the same receipt state: accepted, criteria met,
// nothing left and no request still open — and the history lands with the
// status in one transaction.
func TestWorkReviewCompletesFromTheReceiptStatus(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-b", "c1", map[string]any{"status": workStatusRunning})
	receiveRequestState(t, s, "task-b", "b", repoApproval())
	// The person's request is answered and closed — a completion over an
	// unanswered one is refused, which is its own test.
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-b", "request_id": "answer-1",
		"turn": map[string]any{"request": "이대로 끝내 주세요"},
	})["turn"].(map[string]any)
	turnVersion, _ := taskGuardsOf(t, s, "task-b")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-b", "turn_id": turn["id"], "request_id": "close-1",
		"expected_version": taskVersionOf(t, s, "task-b"), "expected_turn_version": turnVersion,
		"turn": map[string]any{
			"response": "세션의 최종 답", "explanation": "사람에게 쓴 설명",
			"ended_at": "2026-11-28T12:00:00Z",
		},
	})
	criteria := "PR 이 머지되면 끝"
	accepted := "사용자가 「이대로 끝내 주세요」라고 보낸 턴을 읽었다"
	_, bookVersion := taskGuardsOf(t, s, "task-b")
	done, code := workCall(t, s, taskReviewCompleteTool, map[string]any{
		"task_id": "task-b", "request_id": "done-1",
		"expected_version": taskVersionOf(t, s, "task-b"), "expected_book_version": bookVersion,
		"outcome": map[string]any{
			"accepted": true, "completion_criteria": criteria, "evidence": accepted,
		},
	})
	if code != "" || done["task"].(map[string]any)["status"] != workStatusCompleted {
		t.Fatalf("complete from the receipt = %q %v", code, done)
	}
	details := taskBookDetails(t, s, "task-b")
	if _, open := details[workReviewKey]; open {
		t.Fatalf("completion left the review open: %v", details)
	}
	entry := details[workReviewHistoryKey].([]any)[0].(map[string]any)
	if entry["to_status"] != workStatusCompleted || entry["accepted"] != true ||
		entry["from_status"] != workStatusPlanned {
		t.Fatalf("history entry = %v", entry)
	}
	// Atomic: the status and the history are one write. A completion refused
	// for an unanswered request leaves neither.
	newWorkTask(t, s, "task-c", "c2", map[string]any{"status": workStatusRunning})
	receiveRequestState(t, s, "task-c", "c", repoApproval())
	// The person's words are still sitting there unanswered — which is what a
	// stored request leaves behind, and a completion over one claims work
	// nobody did.
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-c", "request_id": "unanswered-c",
		"turn": map[string]any{"request": "아직 답하지 않은 요청"},
	})
	_, bookVersion = taskGuardsOf(t, s, "task-c")
	if _, code = workCall(t, s, taskReviewCompleteTool, map[string]any{
		"task_id": "task-c", "request_id": "done-c",
		"expected_version": taskVersionOf(t, s, "task-c"), "expected_book_version": bookVersion,
		"outcome": map[string]any{
			"accepted": true, "completion_criteria": criteria, "evidence": accepted,
		},
	}); code != "conflict" {
		t.Fatalf("completion over an open request = %q", code)
	}
	if got := taskStatusOf(t, s, "task-c"); got != workStatusPlanned {
		t.Fatalf("a refused completion moved the task to %q", got)
	}
	if _, kept := taskBookDetails(t, s, "task-c")[workReviewHistoryKey]; kept {
		t.Fatalf("a refused completion wrote history")
	}
}

// A bare status write may not walk away from a decision this slot recorded and
// nobody settled. This is not the transition fence that was removed — a task
// with no recorded review still moves anywhere — it is about not orphaning the
// record.
func TestWorkStatusWriteWillNotOrphanAnOpenReview(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	receiveRequestState(t, s, "task-a", "a", repoApproval())
	put := func(status, requestID string) (map[string]any, string) {
		return workCall(t, s, "task_put", map[string]any{
			"id": "task-a", "request_id": requestID, "expected_version": taskVersionOf(t, s, "task-a"),
			"task": map[string]any{
				"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
				"completion_criteria": "PR 이 머지되면 끝", "status": status,
			},
		})
	}
	// The two statuses that say the work moved on are refused, and the refusal
	// names both ways past it.
	for _, status := range []string{workStatusRunning, workStatusCompleted} {
		decoded, code := put(status, "orphan-"+status)
		if code != "conflict" {
			t.Fatalf("task_put → %s over an open review = %q %v", status, code, decoded)
		}
		message := decoded["error"].(string)
		for _, name := range []string{taskReviewResumeTool, taskReviewCompleteTool, taskBookPutTool} {
			if !strings.Contains(message, name) {
				t.Fatalf("the refusal does not name %s: %s", name, message)
			}
		}
		if got := taskStatusOf(t, s, "task-a"); got != workStatusPlanned {
			t.Fatalf("a refused write moved the task to %q", got)
		}
	}
	// The receipt's own status and review itself are not refused: one is where
	// a stored request legitimately puts such a task, the other is where it is.
	if _, code := put(workStatusPlanned, "again-planned"); code != "" {
		t.Fatalf("task_put → planned over an open review = %q", code)
	}
	if _, code := put(workStatusReview, "back-to-review"); code != "" {
		t.Fatalf("task_put → review over an open review = %q", code)
	}
	// "this slot cannot say" is not refused either — insisting on a certainty
	// from a writer who is admitting it has none would strand the task.
	if _, code := put("unknown", "to-unknown"); code != "" {
		t.Fatalf("task_put → unknown over an open review = %q", code)
	}
	// Settling it with the call that carries the evidence opens the way.
	mustWork(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "to-review-2", "expected_version": taskVersionOf(t, s, "task-a"),
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": workStatusReview,
		},
	})
	resumeOneReview(t, s, "task-a", "a", "권한이 붙었다", "구현을 올린다")
	if _, code := put(workStatusCompleted, "after-settled"); code != "" {
		t.Fatalf("task_put after the review was settled = %q", code)
	}

	// The other way past it: a task whose bookkeeping says there is nothing to
	// settle. Replacing the record is the parent's own lane and always was.
	newWorkTask(t, s, "task-b", "c2", map[string]any{"status": workStatusRunning})
	receiveRequestState(t, s, "task-b", "b", repoApproval())
	_, bookVersion := taskGuardsOf(t, s, "task-b")
	mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-b", "request_id": "clear-1", "expected_book_version": bookVersion,
		"record": map[string]any{
			"stage": "접수", "owner": "silas", "sessions": []any{},
			"details": map[string]any{"note": "물어볼 것이 없어진 것을 확인했다"},
		},
	})
	moved, code := workCall(t, s, "task_put", map[string]any{
		"id": "task-b", "request_id": "run-b", "expected_version": taskVersionOf(t, s, "task-b"),
		"task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": workStatusRunning,
		},
	})
	if code != "" || moved["task"].(map[string]any)["status"] != workStatusRunning {
		t.Fatalf("task_put after the record was replaced = %q %v", code, moved)
	}

	// And the ordinary path is untouched: a task that never had a review moves
	// planned → running → completed with task_put alone.
	newWorkTask(t, s, "task-c", "c3", map[string]any{"status": workStatusPlanned})
	for version, status := range []string{workStatusRunning, workStatusCompleted} {
		if _, code := workCall(t, s, "task_put", map[string]any{
			"id": "task-c", "request_id": "plain-" + status, "expected_version": version + 1,
			"task": map[string]any{
				"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
				"completion_criteria": "PR 이 머지되면 끝", "status": status,
			},
		}); code != "" {
			t.Fatalf("the ordinary path was refused at %s: %q", status, code)
		}
	}
}

// Settling a review from the receipt is still one processing and one history
// entry under a race, still refused on a stale counter, and still the monitor
// lane's to do.
func TestWorkReviewFromTheReceiptIsExclusiveAndRoleBound(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	receiveRequestState(t, s, "task-a", "a", repoApproval())
	version := taskVersionOf(t, s, "task-a")
	_, bookVersion := taskGuardsOf(t, s, "task-a")
	resume := func(requestID string, v, b float64) (map[string]any, string) {
		return workCall(t, s, taskReviewResumeTool, map[string]any{
			"task_id": "task-a", "request_id": requestID,
			"expected_version": v, "expected_book_version": b,
			"outcome": map[string]any{"evidence": "권한이 붙었다", "remaining": "구현을 올린다"},
		})
	}
	if _, code := resume("monitor-1", version, bookVersion); code != "" {
		t.Fatalf("the first monitor lost: %q", code)
	}
	// A second monitor holding the values it read before that write is refused,
	// so the review is processed once and the history has one entry.
	if _, code := resume("monitor-2", version, bookVersion); code != "conflict" {
		t.Fatalf("a stale resume = %q, want conflict", code)
	}
	history := taskBookDetails(t, s, "task-a")[workReviewHistoryKey].([]any)
	if len(history) != 1 {
		t.Fatalf("history = %v, want exactly one entry", history)
	}
	// And it is refused for having nothing open now, not admitted a second time.
	_, bookVersion = taskGuardsOf(t, s, "task-a")
	if _, code := resume("monitor-3", taskVersionOf(t, s, "task-a"), bookVersion); code != "conflict" {
		t.Fatalf("a resume with nothing open = %q, want conflict", code)
	}

	// Role: the lane that talks to people may not settle a review even from the
	// receipt — the records are the monitor lane's (AC-N11 3).
	db, parent, monitor := newLaneTestDB(t)
	if _, refusal := laneTool(t, db, monitor, "task_put", map[string]any{
		"id": "task-x", "request_id": "c1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-1", "title": "t", "description": "d",
			"completion_criteria": "c", "status": workStatusPlanned,
		},
	}); refusal != "" {
		t.Fatalf("the monitor could not write the task: %s", refusal)
	}
	for _, name := range []string{taskReviewOpenTool, taskReviewResumeTool, taskReviewCompleteTool} {
		_, refusal := laneTool(t, db, parent, name, map[string]any{"task_id": "task-x"})
		if refusal == "" {
			t.Fatalf("the parent lane was allowed to call %s", name)
		}
	}
}
