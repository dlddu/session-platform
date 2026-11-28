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
	// task_put may not cross the boundary in either direction. The refusals
	// name the call that does the job.
	newWorkTask(t, s, "task-running", "c2", map[string]any{"status": workStatusRunning})
	_, code := workCall(t, s, "task_put", map[string]any{
		"id": "task-running", "request_id": "to-review", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "t", "description": "d",
			"completion_criteria": "c", "status": workStatusReview,
		},
	})
	if code != "conflict" {
		t.Fatalf("task_put into review = %q, want conflict", code)
	}
	decoded, code := workCall(t, s, "task_put", map[string]any{
		"id": "task-old-writer", "request_id": "out-of-review", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "t", "description": "d",
			"completion_criteria": "c", "status": workStatusCompleted,
		},
	})
	if code != "conflict" || !strings.Contains(decoded["error"].(string), taskReviewCompleteTool) {
		t.Fatalf("task_put out of review = %q %v", code, decoded)
	}
	// Staying in review while other fields change is not a transition.
	mustWork(t, s, "task_put", map[string]any{
		"id": "task-old-writer", "request_id": "retitle", "expected_version": 1,
		"task": map[string]any{
			"session_id": "worker-1", "title": "제목만 고친다", "description": "d",
			"completion_criteria": "c", "status": workStatusReview,
		},
	})
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
		"outcome": map[string]any{"accepted": true, "completion_criteria": criteria},
	}, "conflict")
}

// A completion is about *this* task's criteria and *this* task's open work, and
// an unapplied decision outranks all of it.
func TestWorkReviewCompletionChecksTheCriteriaAndTheOpenWork(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": workStatusRunning})
	openOneReview(t, s, "task-a", "a", repoApproval())
	criteria := "PR 이 머지되면 끝"
	attempt := func(requestID string, outcome map[string]any) (map[string]any, string) {
		_, bookVersion := taskGuardsOf(t, s, "task-a")
		return workCall(t, s, taskReviewCompleteTool, map[string]any{
			"task_id": "task-a", "request_id": requestID,
			"expected_version": taskVersionOf(t, s, "task-a"), "expected_book_version": bookVersion,
			"outcome": outcome,
		})
	}
	// The open decision outranks everything else: it stays in review.
	decoded, code := attempt("done-open", map[string]any{"accepted": true, "completion_criteria": criteria})
	if code != "conflict" || !strings.Contains(decoded["error"].(string), "not applied") {
		t.Fatalf("completion over an open decision = %q %v", code, decoded)
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
	decoded, code = attempt("done-open-request", map[string]any{"accepted": true, "completion_criteria": criteria})
	if code != "conflict" || !strings.Contains(decoded["error"].(string), "ended_at") {
		t.Fatalf("completion over an open request = %q %v", code, decoded)
	}
	// A result that was not accepted, criteria that are not this task's, and
	// something left are each refused on their own.
	if _, code = attempt("done-unaccepted", map[string]any{
		"accepted": false, "completion_criteria": criteria,
	}); code != "invalid_argument" {
		t.Fatalf("unaccepted completion = %q", code)
	}
	if _, code = attempt("done-wrong-criteria", map[string]any{
		"accepted": true, "completion_criteria": "대충 다 됐으면 끝",
	}); code != "conflict" {
		t.Fatalf("completion against other criteria = %q", code)
	}
	if _, code = attempt("done-remaining", map[string]any{
		"accepted": true, "completion_criteria": criteria, "remaining": "배포가 남았다",
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
	done, code := attempt("done-1", map[string]any{"accepted": true, "completion_criteria": criteria})
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
