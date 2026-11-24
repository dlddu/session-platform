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
- **실행 단계**: (a) `codex-manager` 세션을 만들고 `GET /api/v1/sessions/{id}/lanes`를 부르고 그 워크로드 파드를 조회한다 → (b) 레인을 지목하지 않은 `write`와 `lane: "monitor"`인 `write`를 **거의 동시에** 보내고, 각 레인을 `read`로 읽는다 → (c) 한 레인의 `nextOffset`으로 다른 레인을 `read`한다 → (d) `lane: "Monitor"`(오타)로, 그리고 `lane`과 `toolApprovals`를 함께 써서 `write`를 보낸다 → (e) 모니터 레인에서 `task_put`으로 작업을 쓰고, 부모 레인에서 `task_get`·`task_list`로 읽은 뒤 부모 레인에서 `task_put`을 시도한다 → (f) 부모 레인에서 `relay_send`로 결정을 넘기고, 모니터 레인에서 `relay_list({open: true})`→`relay_claim`→`relay_ack`를 한 뒤 부모 레인에서 `relay_status`를 부른다 → (g) (f)의 `relay_send`를 같은 `id`·같은 `request_id`로 다시 보내고, 이어 같은 `id`에 다른 본문으로 보낸다 → (h) 모니터 레인이 claim만 한 메시지를 남긴 채 **워크로드 컨테이너를 재시작**시키고, 새 모니터 레인에서 같은 메시지를 claim한다 → (i) 세션을 동결한 뒤 `GET …/lanes`를 부르고, 복원해서 부모 레인과 모니터 레인을 각각 `read`한다 → (j) **부모 레인에만** `relay_send`로 지시를 넘기고, 사람도 control plane도 모니터 레인에 `write`하지 **않은 채** 모니터 레인을 `read`하고 작업 기록과 `relay_status`를 읽는다 → (k) `manager-monitor-lane`을 `"true"`로 바꾼 ConfigMap으로 파드를 다시 띄운다
- **기대 결과**: (a) `paired: true`이고 레인이 둘이며 **두 레인의 `pod`가 같은 하나**다. 파드의 PVC 볼륨은 여전히 슬롯 하나이고, 컨테이너 수·포트·Service·NetworkPolicy가 짝 없는 매니저와 같다. 워크로드 컨테이너가 `MANAGER_MONITOR_LANE`·`MANAGER_MONITOR_STATE_DIR`을 받고, 사이드카는 `MANAGER_MONITOR_STATE_DIR`을 받지 않는다 — 사이드카가 받는 것은 깨울 레인의 주소와 같은 옵트인뿐이다. (b) **두 턴이 동시에 돈다**(부모의 긴 턴이 모니터의 턴을 붙잡지 않는다). 각 레인의 읽기에 **자기 프롬프트만** 있고 상대의 것은 없다. (c) 빈 payload가 오거나 그 레인의 자기 커서 기준으로 답하며, 한 레인의 offset이 다른 레인에서 뜻을 갖지 않는다. (d) 둘 다 400이고 아무 레인에도 아무것도 쓰이지 않는다. (e) 부모의 `task_get`·`task_list`가 모니터가 쓴 작업을 **그대로 읽고**, 부모의 `task_put`은 그 기록이 모니터 레인의 것이라는 말과 `relay_send`·`relay_status`를 이름으로 알려 주며 거절된다. 부모 레인의 `tools/list`에 기록 쓰기 도구가 **없다**. (f) 모니터의 inbox에 그 메시지가 있고(목록만으로는 상태가 변하지 않는다) claim이 token과 `attempts: 1`을 주며, ack 뒤 부모의 `relay_status`가 `settled: true`·`dealt_with: true`와 모니터가 적은 결과를 준다. (g) 같은 인자 재전송은 **그때 커밋된 답**을 돌려주고 메시지는 하나다. 다른 본문은 `conflict`다. (h) 재시작된 모니터의 claim이 `recovered: true`·앞 `prior_incarnation`·`attempts: 2`를 주고, 이전 run의 token으로는 ack가 거절된다. (i) 동결 중 `/lanes`는 **422**이고 파드를 되살리지 않는다. 복원 뒤 부모 레인은 자기 대화를 이어 가고 **모니터 레인은 새 대화로 시작**하며, 미settle 릴레이 메시지와 모니터 행은 슬롯 DB에 그대로 있다. (j) **아무도 모니터 레인에 write하지 않았는데** 모니터 레인의 읽기에 그 턴이 나타난다 — 사이드카가 깨운 것이다. 그 턴이 메시지를 claim해 지시대로 작업하고 기록을 갱신하며, 부모의 `relay_status`가 `dealt_with: true`와 모니터가 적은 결과로 답한다. 사이드카가 `MANAGER_MONITOR_WAKE_URL`과 `MANAGER_MONITOR_LANE`을 받고 그 주소는 이 파드의 루프백이다. (k) 파드가 **시작에 실패**한다 — 레인 하나인 매니저로 조용히 돌지 않는다
- **검증 AC**: AC-N11
