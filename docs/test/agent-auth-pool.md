# 테스트 문서: 에이전트 인증 풀

## 검증 대상 AC
- AC-N1: 슬롯 = 이름이 고정된 PVC, control plane이 만들고 지우지 않는다 (PRD: 에이전트 인증 풀)
- AC-N2: 독점 임대 = 슬롯마다 Lease 하나, holder는 세션 id (PRD: 에이전트 인증 풀)
- AC-N3: 반납 = 동결 완료·삭제 시 (PRD: 에이전트 인증 풀)
- AC-N4: 로그인은 슬롯 파일, 에이전트가 HOME과 동기화하고 아카이브에서 뺀다 (PRD: 에이전트 인증 풀)
- AC-N5: 권한 — control plane은 PVC·Lease만, 세션은 자기 슬롯만 (PRD: 에이전트 인증 풀)
- AC-N6: `codex-manager` 슬롯 = 로그인 + 세션 DB, 컨테이너마다 제 몫만 마운트 (PRD: 에이전트 인증 풀)
- AC-N7: 사용자 요청 — 앱이 원장에 남기고, 부모가 접수해 처리한다 (PRD: 에이전트 인증 풀)
- AC-N8: 보낼 선택지는 세션이 쓰고, 작업에는 이력이 남는다 (PRD: 에이전트 인증 풀)
- AC-N11: 매니저 파드의 두 레인 — 사람과 말하는 부모, 작업을 움직이는 모니터 (PRD: 에이전트 인증 풀)
- AC-N13: 사람이 판단하는 상태는 `review` 하나다 — 옛 `blocked` 는 입력의 별칭이고 출력의 판정이다 (PRD: 에이전트 인증 풀)

> 📌 **SUT 설정**: kind e2e 오버레이가 `codex-task`를 켜고 풀 크기를 2로 둔다(`CODEX_TASK_AUTH_SLOTS`). 슬롯 PVC는 kind 기본
> 스토리지 클래스(local-path)로 만들어진다. `claude-code`는 어느 모드에서도 풀을 쓰지 않는다(AC-E7) — 풀 시나리오는
> 모두 `codex-task` 세션으로 관측한다. 실 계정 로그인은 할 수 없으므로 로그인 파일이 실제로 갱신되는 경로(AC-N4)는 단위 테스트가 맡는다.

## 테스트 시나리오

### 시나리오 1: 첫 세션이 슬롯을 임대하고 그 PVC만 마운트한다
- **사전 조건**: 풀 크기 2, 쥔 슬롯이 없는 SUT와 그 클러스터의 kube 접근
- **실행 단계**: `codex-task` 세션 하나를 만들고 슬롯 Lease·PVC·세션 파드를 조회한다
- **기대 결과**: Lease `codex-task-auth-<i>` 하나의 `holderIdentity`가 그 세션 id다. PVC `<네임스페이스>-codex-task-auth-<i>`가 라벨 `session-platform.dev/auth-pool=codex-task`·`session-platform.dev/auth-slot=<i>`와 함께 있다. 세션 파드의 PVC 볼륨은 그 슬롯 하나뿐이고 주 컨테이너만 `/var/run/session-platform/auth-slot`에 마운트하며 `AGENT_AUTH_SLOT_DIR`이 그 경로다
- **검증 AC**: AC-N1, AC-N2, AC-N5

### 시나리오 2: 슬롯이 다 차면 생성이 503이고 파드가 생기지 않는다
- **사전 조건**: 풀 크기 2, 쥔 슬롯이 없는 SUT
- **실행 단계**: `codex-task` 세션을 셋 만든다
- **기대 결과**: 앞의 둘은 서로 다른 슬롯을 쥔다. 셋째는 503(`auth pool exhausted`)이고 그 이름의 세션·파드가 없다
- **검증 AC**: AC-N2

### 시나리오 3: 삭제가 슬롯을 반납하고 PVC는 남는다
- **사전 조건**: 시나리오 2를 마친 SUT(슬롯 둘이 다 찼다)
- **실행 단계**: (a) 슬롯을 쥔 세션 하나를 지운다 → (b) `codex-task` 세션을 하나 더 만든다
- **기대 결과**: (a) 그 슬롯의 Lease에 holder가 없고 PVC는 남아 있다. (b) 새 세션이 그 슬롯을 쥐고 생성이 성공한다
- **검증 AC**: AC-N3, AC-N1

### 시나리오 4: 로그인 파일은 턴 앞뒤로 슬롯과 맞춰지고 아카이브에 들지 않는다
- **사전 조건**: 슬롯 디렉터리와 HOME을 가진 에이전트(`codex-task`·`claude-code` subscription)
- **실행 단계**: (a) 슬롯에 로그인 파일을 두고 턴을 돌린다 → (b) 턴 중에 HOME의 파일을 바꾼다 → (c) 턴 중에 HOME의 파일을 지운다 → (d) 동결 아카이브를 만든다 → (e) `GET /auth-slot`을 부른다
- **기대 결과**: (a) 턴이 시작될 때 HOME에 같은 내용이 있다. 슬롯에 파일이 없으면 HOME의 것도 지워진다. (b) 1초 안에, 그리고 턴 끝에 슬롯이 새 내용이다. (c) 턴 끝에 슬롯의 파일도 없다. (d) 아카이브에 HOME의 로그인 파일이 없다. (e) 슬롯 파일 유무가 `signedIn`으로 나온다
- **검증 AC**: AC-N4

### 시나리오 5: `codex-manager` 슬롯은 로그인과 세션 DB로 나뉘고, 세션 DB는 사이드카 MCP로만 닿으며 기본 표면은 원장뿐이다
- **사전 조건**: `codex-chat` 풀이 있는 SUT(`CODEX_CHAT_AUTH_SLOTS` ≥ 1)와 그 클러스터의 kube 접근
- **실행 단계**: (a) `codex-manager` 세션을 만들고 그 워크로드 파드를 조회한다 → (b) 사이드카의 `/session-db/mcp`에 `tools/list`를 부르고, 이어 `raw_exec`를 불러 본다 → (c) 배포가 `session-db-raw-sql: debug`를 준 사이드카에서 `raw_exec`로 테이블을 만들고 행을 넣은 뒤 `raw_query`로 읽는다 → (d) 없는 테이블을 `raw_query`한다 → (e) 세션을 지우고 같은 슬롯을 받은 새 `codex-manager` 세션에서 (c)의 읽기를 다시 한다
- **기대 결과**: (a) PVC 볼륨은 슬롯 `<네임스페이스>-codex-chat-auth-<i>` 하나다. 주 컨테이너는 subPath `auth`를 `/var/run/session-platform/auth-slot`에, `k3s-mcp-proxy`는 subPath `db`를 `/var/run/session-platform/session-db`에 마운트하고, 슬롯 전체를 마운트하는 것은 초기화 컨테이너 `slot-init`뿐이다. 주 컨테이너는 `SESSION_DB_MCP_URL=http://127.0.0.1:8093/session-db/mcp`를 받고 `SESSION_DB_DIR`·`SESSION_DB_RAW_SQL`은 받지 않는다 — 그 스위치는 사이드카에만 ConfigMap 키 참조로 붙는다. (b) 도구 목록은 `ledger_get`·`ledger_list`·`ledger_put`·`ledger_delete` 넷뿐이고, `raw_exec` 호출은 `SESSION_DB_RAW_SQL`을 가리키는 오류로 거절된다. (c) `rowsAffected`가 1이고 넣은 행이 돌아온다. (d) `isError` 도구 결과에 SQLite의 메시지가 있다. (e) 앞 세션이 넣은 행이 그대로 있다
- **검증 AC**: AC-N6

### 시나리오 6: 사용자 요청은 원장에 남고, 부모가 접수해 처리하며, 죽은 뒤에는 재실행이 아니라 대조로 이어진다
- **사전 조건**: 파드가 살아 있는 `codex-chat` 세션 하나와 그 원장의 작업 하나(`t1`), control plane 접근
- **실행 단계**: (a) `POST /api/v1/sessions/{id}/ledger/tasks/t1/requests`로 `{id: "req-1", text: "이 작업 진행해 줘", author: "user-7"}`을 저장하고, 같은 본문을 한 번 더 보낸다 → (b) 없는 작업 id로, 그리고 남의 테넌트 세션 id로 같은 호출을 한다 → (c) 세션 안에서 `ledger_get t1`과 `ledger_requests_list`를 부른다 → (d) `ledger_requests_claim`을 두 번(서로 다른 `request_id`, 같은 `expected_version`) 부른다 → (e) 이긴 쪽이 `ledger_requests_dispatch`로 `intended`·`dispatched`를 적은 뒤 **ack 없이 claim을 만료시킨다** → (f) 다른 부모가 다시 claim하고, 적힌 것과 **다른** `operation_key`로 dispatch를 시도한 뒤 `observed=not_accepted`를 붙여 다시 시도한다 → (g) `ledger_requests_ack`로 결과와 `related_seq`를 적는다 → (h) 작업 `t1`을 `ledger_delete`한 뒤 `ledger_requests_list`를 부른다
- **기대 결과**: (a) 첫 호출 201·`status: pending`, 두 번째 200·`replayed: true`이고 행은 하나다. (b) 둘 다 404이며 아무것도 저장되지 않는다. (c) `ledger_get`의 작업 레코드에 `requests.pending`이 1이고, 목록은 도착 순으로 그 요청을 보여 주며 조회만으로는 `status`가 변하지 않는다. (d) 한 쪽만 성공해 `claimed`와 `claim_token`을 받고 다른 쪽은 `conflict`다. (e) 상태는 `claimed`·`dispatch_state: dispatched`로 남는다. (f) 다시 claim한 응답은 `reconcile: true`와 앞선 `operation_key`를 주고 상태는 `reconciling`이며, 다른 operation key의 dispatch는 `conflict`로 거절되고 `observed=not_accepted`를 붙이면 받아들여진다. 만료된 쪽의 토큰으로는 ack도 dispatch도 되지 않는다. (g) `processed`(또는 `failed`)·`result`·`related_seq`가 적히고 그 뒤 claim은 거절된다. (h) 요청 행은 그대로 조회되고, 지워진 작업에 대한 **새** 요청은 409다
- **검증 AC**: AC-N7

### 시나리오 7: 선택지는 세션이 쓰고 작업이 바뀌면 사라지며, 이력은 일어난 순서대로 남는다
- **사전 조건**: 파드가 살아 있는 `codex-chat` 세션과 그 원장의 작업 하나(`t1`, version 1)
- **실행 단계**: (a) 세션에서 `ledger_choices_put`으로 `t1`의 선택지 둘을 현재 version 기준으로 쓴다 → (b) `ledger_get t1`과 control plane의 작업 목록을 본다 → (c) `ledger_put`으로 `t1`의 상태를 옮긴다 → (d) 다시 (b)를 본다 → (e) 낡은 version 기준으로 `ledger_choices_put`을 시도한다 → (f) 앱에서 선택지 하나를 골라 요청을 저장한다 → (f-2) (c)에서 작업이 움직이기 전에 열어 둔 화면처럼, **낡은 version 기준의 `choice_id`와 `choice_basis_version`으로** 요청 저장을 다시 시도한다 → (g) 세션이 (f)의 요청을 claim→dispatch→ack 한다 → (h) `GET /api/v1/sessions/{id}/ledger/tasks/t1/events`를 `limit=2`로 두 페이지 읽는다
- **기대 결과**: (a) 응답에 `basis_version`과 쓴 순서의 선택지가 담긴다. (b) 작업 레코드가 그 선택지를 `request_text`까지 함께 싣는다. (c) version이 오른다. (d) 선택지가 **하나도 서빙되지 않는다**(지운 호출 없이). (e) `conflict`. (f) 요청이 `choice_id`와 함께 저장된다. (f-2) **409 `stale_choice`로 거부되고 아무것도 저장되지 않는다** — 재사용된 id 와 구별되므로 앱은 초안을 지우지 않고 작업을 다시 읽으면 된다. (g) 각 단계가 성공한다. (h) 이력이 `task_created`·`task_updated`(무엇이 움직였는지)·`choices_published`·`request_stored`(사람 원문)·`request_claimed`·`request_dispatched`·`request_settled` 순으로 보이고, 첫 페이지의 `next_cursor`로 이어 읽은 둘째 페이지가 겹치거나 빠지지 않으며, 어디에도 claim token·자격증명이 없다
- **검증 AC**: AC-N8

### 시나리오 8: 원장이 작업 레코드로 옮겨 가고, 앱은 그대로 쓰며, 되돌릴 길이 남는다
- **사전 조건**: 파드가 살아 있는 `codex-chat` 세션과 그 원장의 작업 둘(하나는 선택지와 접수된 사용자 요청이 있고, 하나는 모니터 행), control plane 접근, 앱은 **갱신하지 않은 상태**
- **실행 단계**: (a) 세션에서 `work_migrate({dry_run: true})`를 부르고 control plane의 작업 목록을 다시 읽는다 → (b) `work_migrate`를 실제로 부른다 → (c) 같은 `request_id`로 한 번 더, 이어서 다른 `request_id`로 한 번 더 부른다 → (d) **앱을 갱신하지 않은 채** control plane의 작업 목록을 읽고, 작업 하나에 `POST .../requests`로 사용자 요청을 저장한다 → (e) (a)에서 보던 화면의 낡은 `choice_id`로 요청 저장을 시도한다 → (f) 세션에서 `ledger_put`과 `ledger_get`을, 이어서 `task_get`·`turn_list({open: true})`를 부른다 → (g) `work_migration_report`를 부르고, 적힌 스냅샷 파일의 SHA-256을 직접 재계산한다 → (h) 이 세션을 아카이브했다가 **이 작업 전 이미지로** 복원해 control plane의 작업 목록을 읽는다
- **기대 결과**: (a) 쓸 행 수와 못 옮길 요청이 보고되지만 목록은 **(a) 전과 한 글자도 다르지 않다**(아무것도 쓰이지 않았다). (b) 상태가 `migrated`가 되고, 응답에 스냅샷 경로·크기·체크섬과 행 수가 담기며, 모니터 행은 **작업이 되지 않고** 보존 수로만 세어지고, 못 옮긴 요청에는 사유가 붙는다. (c) **두 호출 모두** `already_migrated`와 현재 보고를 돌려주고 행이 늘지 않는다 — 옮기기가 끝난 DB 는 `request_id`를 보기 전에 상태로 답하므로 같은 id 인지는 영향이 없다(`request_id`의 replay 가 걸리는 곳은 펜스가 올라 있는 **재개** 경로다). (d) 목록은 **옮기기 전과 같은 모양**이고(stage·owner·세션 관계까지 제자리에 있다) 요청 저장은 201이며, 그 요청은 작업의 **다음 턴**으로 들어간다. (e) `stale_choice`(409)로 거부되고 아무것도 저장되지 않는다. (f) `ledger_put`은 쓸 작업 도구를 이름으로 알려 주며 `conflict`로 거절되고 `ledger_get`은 **그대로 읽히며**(모니터 행·작업 이력 포함), `task_get`은 합의한 컬럼만·`turns_total`·최신 턴을 주고 `turn_list({open: true})`가 (d)의 턴을 집는다. (g) 보고의 체크섬이 재계산한 값과 같고, 현재 행 수가 (b)의 수와 맞는다. (h) 옛 이미지가 **DB를 열고** 원장을 그대로 서빙해 옮기기 전 작업이 다시 보인다 — 커토버 뒤에 쓴 턴은 원장에 없고, 원장에 만들어 넣지도 않는다
- **검증 AC**: AC-N9

### 시나리오 9: 매니저 파드가 대화 둘을 한 파드에서 동시에 돌리고, 기록은 모니터가 쓰고 부모는 읽는다
- **사전 조건**: `codex-chat` 풀이 있는 SUT와 배포가 `manager-monitor-lane: "1"`을 준 codex ConfigMap, control plane 접근, 그 클러스터의 kube 접근
- **실행 단계**: (a) `codex-manager` 세션을 만들고 `GET /api/v1/sessions/{id}/lanes`를 부르고 그 워크로드 파드를 조회한다 → (b) 레인을 지목하지 않은 `write`와 `lane: "monitor"`인 `write`를 **거의 동시에** 보내고, 각 레인을 `read`로 읽는다 → (c) 한 레인의 `nextOffset`으로 다른 레인을 `read`한다 → (d) `lane: "Monitor"`(오타)로, 그리고 `lane`과 `toolApprovals`를 함께 써서 `write`를 보낸다 → (e) 모니터 레인에서 `task_put`으로 작업을 쓰고, 부모 레인에서 `task_get`·`task_list`로 읽은 뒤 부모 레인에서 `task_put`을 시도한다 → (f) 사람이 부모 레인에 말을 걸어 **그 턴이 도는 동안** 부모 레인에서 `lane_ask({request, request_id})`를 부르고, 돌아오는 데 걸린 시간을 재고, 모니터 레인을 `read`하고 두 레인의 큐 상태를 **같은 시점에** 본다 → (g) 같은 `request_id`·같은 원문으로 `lane_ask`를 다시 부르고, 이어 같은 id·다른 원문으로 부른다 → (h) (f)의 접수 번호로 `lane_answer`를 부른다. 이어 **앞 런의 번호**(런 부분이 다른 번호)·**발급되지 않은 번호**·**형식이 틀린 번호**로 각각 부른다 → (i) 모니터 레인의 큐를 채운 상태에서 `lane_ask`를 불러 레인이 요청을 받지 못하게 한다 → (j) 세션을 동결한 뒤 `GET …/lanes`를 부르고, 복원해서 부모 레인과 모니터 레인을 각각 `read`한다 → (k) 모니터 레인이 돌고 있는 접수 번호를 들고 **워크로드 컨테이너를 재시작**시킨 뒤 그 번호로 `lane_answer`를 부른다 → (l) 모니터 레인에서 `lane_ask`·`lane_answer`를 부르고, `manager-monitor-lane`을 `"true"`로 바꾼 ConfigMap으로 파드를 다시 띄운다
- **기대 결과**: (a) `paired: true`이고 레인이 둘이며 **두 레인의 `pod`가 같은 하나**다. 파드의 PVC 볼륨은 여전히 슬롯 하나이고, 컨테이너 수·포트·Service·NetworkPolicy가 짝 없는 매니저와 같다. 워크로드 컨테이너가 `MANAGER_MONITOR_LANE`·`MANAGER_MONITOR_STATE_DIR`을 받고, 사이드카는 `MANAGER_MONITOR_STATE_DIR`을 받지 않는다 — 사이드카가 받는 것은 요청을 올릴 레인의 주소(`MANAGER_MONITOR_WRITE_URL`, 이 파드의 루프백)와 같은 옵트인뿐이다. (b) **두 턴이 동시에 돈다**(부모의 긴 턴이 모니터의 턴을 붙잡지 않는다). 각 레인의 읽기에 **자기 프롬프트만** 있고 상대의 것은 없다. (c) 빈 payload가 오거나 그 레인의 자기 커서 기준으로 답하며, 한 레인의 offset이 다른 레인에서 뜻을 갖지 않는다. (d) 둘 다 400이고 아무 레인에도 아무것도 쓰이지 않는다. (e) 부모의 `task_get`·`task_list`가 모니터가 쓴 작업을 **그대로 읽고**, 부모의 `task_put`은 그 기록이 모니터 레인의 것이라는 말과 `lane_ask`·`lane_answer`를 이름으로 알려 주며 거절된다. 부모 레인의 `tools/list`에 기록 쓰기 도구가 **없다**. (f) `lane_ask`가 **턴을 기다리지 않고 즉시** `ticket`·`lane: "monitor"`·`replayed: false`로 돌아오고(부모의 턴은 계속 돌고 있다), 모니터 레인의 읽기에 **요청 원문이 그대로** 나타나며 — 아무도 그 레인에 `write`하지 않았는데 — **두 레인의 큐가 같은 시점에 모두 non-idle**이다. (g) 같은 id·같은 원문은 **같은 `ticket`에 `replayed: true`**이고 모니터에게 프롬프트가 다시 올라가지 않는다. 같은 id·다른 원문은 `conflict`다. (h) 접수 번호가 그 턴을 가리킨다 — `queued`/`running`/`done` 중 하나와 그 턴의 `seq`, 끝났으면 **그 턴이 쓴 출력과 종료 상태**이며 다른 턴의 말이나 자기 입력 표식은 섞이지 않는다. 앞 런의 번호는 **`unknown_run`**(그 런의 같은 번호 턴을 답하지 않는다), 발급되지 않은 번호는 **`absent`**, 형식이 틀린 번호는 **거절**이다. (i) **접수 번호가 발급되지 않는다** — 요청을 받지 못했다는 오류만 돌아오고, 나중에 물어볼 번호가 생기지 않으며 그 `request_id`는 기억되지도 않는다. (j) 동결 중 `/lanes`는 **422**이고 파드를 되살리지 않는다. 복원 뒤 부모 레인은 자기 대화를 이어 가고 **모니터 레인은 새 대화로 시작**하며, **작업·턴 원장은 슬롯 DB에 그대로** 있다. (k) 그 번호가 **`unknown_run`**이다 — 미전달 요청의 자동 재전송은 없고, 다시 물을지는 부모가 정한다. 원장에 남은 작업·턴은 그대로다. (l) 모니터의 `lane_ask`·`lane_answer`는 **그 답이 자기가 들어 있는 턴**이라는 말과 함께 거절되고, 오타 값 ConfigMap으로는 파드가 **시작에 실패**한다 — 레인 하나인 매니저로 조용히 돌지 않는다
- **검증 AC**: AC-N11
### 시나리오 11: 사람이 판단하는 상태는 review 하나이고, 옛 blocked 는 근거로 갈린다
- **사전 조건**: 파드가 살아 있는 `codex-chat` 세션과 그 슬롯의 작업 — ⑴ 상태 `running` 의 새 작업, 그리고 **옛 이미지가 `blocked` 로 적어 둔 다섯**: ⑵ 기장 없음, ⑶ 기장 `details.review: {target, applied: false}`, ⑷ 기장 `details.technical_wait: {owner, due, retry}`(셋 다), ⑸ 기장 `details.technical_wait: {owner, retry}`(`due` 없음), ⑹ 기장 `details.technical_wait: "CI 가 도는 중입니다"`(산문). control plane 접근
- **실행 단계**: (a) 세션에서 `task_put` 으로 어떤 작업의 상태를 `blocked` 로 쓰고, 그 작업을 `task_get` 으로 읽고 슬롯의 저장된 값을 본다 → (b) `task_list({status: "review"})` 와 `task_list({status: "blocked"})` 를, 이어서 `task_list({status: "held"})` 를 부른다 → (c) 다섯 레거시 작업을 `task_get` 으로 읽는다 → (d) `task_list({limit: 1})` 로 모든 상태를 하나씩 끝까지 페이지한다 → (e) ⑴ 에 턴을 하나 더하고 `task_review_open` 으로 실제 대상(레포 읽기 권한)·범위·승인 경로·질문·근거를 기록한다. 이어서 **같은 target·scope·route** 로 다른 `request_id` 로 다시 부르고, 다른 target 으로도 부른다 → (f) 앱에서 그 작업에 요청을 제출하고(`POST …/work/tasks/{id}/requests`) 작업을 다시 읽는다 → (g) `task_review_resume` 을 `outcome.evidence` 없이, 이어서 `outcome.remaining` 없이 부르고 작업을 읽는다. 그다음 둘을 채워 부른다 → (h) `task_review_complete` 를 미수락·다른 완료 기준·남은 일 있음·열린 요청 있음·미적용 결정 있음으로 각각 부르고, 전부 갖춘 뒤 부른다 → (i) ⑵ 를 `task_review_resume` 으로 재개하고 그 작업의 기장과 턴을 본다 → (j) `task_put` 으로 ⑴ 을 `running` → `review` 로, 그리고 review 인 작업을 `completed` 로 쓰려 한다 → (k) control plane 의 작업 목록을 `status=review` 와 `status=blocked` 로 읽는다
- **기대 결과**: (a) 답이 `alias_applied: "blocked"` 를 담고 **상태는 `review`** 이며, 슬롯에 저장된 값도 `review` 다 — 이 이미지는 옛 낱말로 쓰지 않는다. (b) 두 필터가 **같은 집합**을 돌려주고, `held` 는 `invalid_argument` 로 거절된다(빈 페이지가 아니다). (c) ⑵ 는 `review`·근거 「근거 없음」, ⑶ 은 `review`·근거 `details.review`, ⑷ 는 **`running`**·근거 `details.technical_wait`, ⑸ 는 `review`·근거가 `due` 가 없다고 말하고, ⑹ 은 `review`·근거가 JSON 객체가 아니라고 말한다. 다섯 모두 `stored_status: "blocked"` 를 함께 담고, **어느 것도 `blocked`·`unknown` 으로 답하지 않으며**, 슬롯의 저장된 값은 다섯 다 **그대로 `blocked`** 다. ⑴ 에는 `stored_status`·`status_basis` 가 **없다**. (d) 상태별 페이지의 합집합이 작업 전체와 같고 **어느 작업도 두 번 나오지 않는다**. 꽉 찬 페이지는 커서를 들고, 마지막 페이지의 커서는 빈 문자열이다. (e) 첫 호출은 `already_open: false` 로 작업을 `review` 로 옮기고 질문이 그 턴에 **축자** 들어간다. 같은 결정의 재요청은 **`already_open: true` 로 아무것도 쓰지 않고**(작업 version·turn_version·book_version 불변) 둘째 승인이 생기지 않으며, 다른 결정은 `conflict` 다. (f) 요청은 **턴 하나**가 되고 작업은 **여전히 `review`** 다 — 제출만으로는 권한이 생기지도 상태가 움직이지도 않는다. (g) 증거 없는 resume 과 남은 일 없는 resume 은 모두 거절되고 작업은 **`review` 에 머문다**(승인 미적용은 review). 둘을 채우면 `running` 이 되고, 그 승인은 `details.review_history` 에 **확인 근거와 함께** 남고 열린 기록은 사라지며 **턴은 손대지 않았다**(질문이 그대로 있다). (h) 다섯 거절이 각각 제 이유를 말하고 작업은 `review` 를 벗어나지 않는다. 전부 갖추면 `completed` 가 되고 닫힌 항목이 이력에 쌓인다. (i) ⑵ 는 `running` 이 되고(저장된 값도 `running`), 이력 항목이 **`from_status: "blocked"` 와 그것을 읽은 근거**를 담는다 — 왜 붙잡혀 있었는지의 유일한 기록이 지워지지 않는다. (j) 둘 다 `conflict` 이고 거절문이 `task_review_open`·`task_review_resume`·`task_review_complete` 를 이름으로 말한다. 판단이 필요 없던 기승인 경로(planned → running → completed)는 `task_put` 만으로 그대로 된다. (k) 두 필터가 모두 200 이고, 레거시 행의 레코드가 `stored_status`·`status_basis` 를 담아 **판정을 바깥에서도 검증할 수 있다**
- **검증 AC**: AC-N13
