# 작업 레코드 전환 인계 — 부모 세션·스킬이 무엇을 어떻게 바꿔야 하는가

> 대상: codex-chat 슬롯의 부모 세션과, 그 부모가 쓰는 관리 스킬
> (`task-session-lifecycle` 계열). 근거 계약은 [세션 원장 계약](session-ledger.md)
> §「Work records (tasks and turns)」와 PRD `AC-N9`.
>
> **이 문서는 스킬 변경을 생략하지 않기 위해 있다.** 플랫폼이 도구를 바꿨는데
> 부모가 옛 도구를 계속 부르면, 막히는 쪽은 사람이 요청을 남긴 작업이다.

## 0. 한 장 요약

| 하던 일 | 커토버 전 | 커토버 후 |
| --- | --- | --- |
| 작업 전체 발견 | `ledger_list` | `task_list` (상태·세션 필터, 키셋) |
| 작업 하나 읽기 | `ledger_get` | `task_get`(`turns_total`·`latest_turn` 포함) |
| 작업 만들기·갱신 | `ledger_put` | `task_put` |
| 작업 지우기 | `ledger_delete` | `task_delete` |
| 사람 요청 발견 | `ledger_requests_list` | `turn_list({open:true})` |
| 요청 접수 | `ledger_requests_claim` | `turn_patch` 로 `started_at` 쓰기 = 접수(§3) |
| 외부 행동 기장 | `ledger_requests_dispatch` | `turn_dispatch` |
| 결과 기록 | `ledger_requests_ack` | `turn_patch`(response·explanation·ended_at) + `expected_turn_version` |
| 보낼 선택지 | `ledger_choices_put` | `turn_patch`의 `choices` |
| 승인·커서·보존 기장 | `ledger_put`의 `details`·`sessions` | **`work_legacy_put`** + `expected_book_version` (§4) |
| 모니터 루프 기록 | `ledger_put` | **`ledger_put` 그대로** (§4) |
| 작업 이력 읽기 | `ledger_events_list` | 옮겨온 작업은 그대로, **새 작업은 그 턴들이 이력**(§5) |
| 롤백 | — | `work_rollback_export` (§6) |

지금 어느 상태인지는 **추측하지 않는다**: `work_migration_report` 의
`state` 가 `ready`·`migrating`·`migrated`·`rolled_back` 중 하나를 말한다.
`ready` 면 아래 전환을 **하지 않는다**(아직 원장이 정본이다).

## 1. 전환 순서 (부모가 직접 하는 일)

1. `work_migration_report` 로 상태를 읽는다.
2. `ready` 면 `work_migrate({request_id, dry_run: true})` 로 **리허설**하고
   보고된 `unmapped` 를 사람에게 보여 준 뒤, 사람의 지시가 있을 때
   `work_migrate({request_id})` 를 부른다. 리허설 없이 부르지 않는다.
3. `migrated` 면 이 문서의 도구로만 쓴다. 옛 도구를 부르면 `conflict` 가
   **무엇을 쓰라고 이름까지 말해 준다** — 그 메시지를 사람에게 그대로 전한다.
4. `migrating` 은 재시도다(손실이 아니다). `rolled_back` 은 §6.

## 2. 발견 — 전수로 읽는 법

```
task_list({limit: 100})                  # 전체. next_cursor 를 after_id 로 이어 읽는다
task_list({status: "blocked"})           # 막힌 것만
task_list({session_id: "none"})          # 배정되지 않은 것만 ("none" 은 리터럴)
```

- 페이지는 **스냅샷이 아니다** — id 로 dedupe 하고, `next_cursor` 가 빈
  문자열일 때만 끝이다.
- `task_get({id})` 는 `turns_total` 과 `latest_turn` 을 함께 준다. 사람이
  무언가 물어봤는지 알려고 턴 목록을 따로 읽을 필요가 없다.
- **모니터 행은 `task_list` 에 없다.** 그것은 부모의 기장이고 작업이 아니다
  (§4). 모니터 행은 `ledger_list` 로 계속 읽는다.

## 3. 사람 요청 소비 — claim 이 없어진 이유와 대신 할 일

원장에서는 요청이 작업 **옆의 행**이었으므로 누가 가져갔는지 표시할
`claim` 이 필요했다. 턴은 작업 **안의 사건**이고, 그 작업의 version 이 이미
경합을 막는다. 그래서 claim 토큰은 없고, 대신 이렇게 한다:

```
1) turn_list({task_id, open: true})
   → request 가 있고 ended_at 이 없는 턴 = 아직 부모가 할 일
   → 응답의 turn_version 을 기억한다 (이 레인의 CAS 값)
2) 그 턴을 집는다  turn_patch({task_id, turn_id, request_id,
      expected_version: <task_get 의 현재 version>,
      expected_turn_version: <1)에서 읽은 turn_version>,
      turn: {started_at: <지금>, instruction: <세션에 넘긴 원문>}})
3) 외부 행동 전에  turn_dispatch({..., expected_turn_version: <다시 읽은 값>,
      operation_key, operation_target, dispatch_state: "intended"})
      →  행동  →  "dispatched"
4) 끝났으면  turn_patch({..., expected_turn_version: <다시 읽은 값>, turn: {
      response: <세션의 최종 답 원문>,
      explanation: <부모가 사람에게 쓰는 설명>,
      ended_at: <지금>}})
```

**모든 쓰기는 직전에 읽은 `turn_version` 을 들고 가야 한다.** 쓰기가 성공하면
그 값이 1 오르고 응답에 새 값이 담긴다. 그래서 같은 값을 든 두 부모 중
**한 쪽만 성공**하고 다른 쪽은 `conflict` 로 「다시 읽으라」는 말을 듣는다.

> **이전 문서의 서술은 틀렸다.** 「expected_version 이 현재 version 이어야
> 하므로 둘 다 집을 수 없다」고 적혀 있었지만, 그 검사는 **비교만 하고 값을
> 올리지 않았다** — 비교만 하는 것은 잠금이 아니다. 실측 결과 같은 version 을
> 읽은 두 부모의 `turn_append` 가 **둘 다 성공**해 턴이 2개 생기고, 두
> `work_legacy_put` 도 둘 다 성공해 **뒤에 쓴 쪽이 앞의 승인 범위를
> 덮어썼다**. 그래서 레인별 CAS 카운터(`turn_version`·`book_version`)를 넣고
> 조건부 UPDATE 한 문장으로 교체했다.

### 접수·소유·외부 행동의 의미

- **접수(claim)** = 그 턴의 `started_at` 을 쓰는 것. 이미 `started_at` 이 있는
  턴에 다시 쓰는 것은 **거절**된다 — 카운터만으로는 막히지 않는다(다른 부모가
  집은 **뒤에** 다시 읽은 부모는 최신 카운터를 들고 있다). 되돌리려면
  `started_at: null` 로 **명시적으로 반납**하고, 그 뒤에 다시 집을 수 있다.
- **소유** = 집은 부모만 그 턴을 위해 외부 행동을 기장할 수 있다. 아무도 집지
  않은 턴의 `turn_dispatch` 는 거절된다(행동할 주체가 없다).
- **종료된 턴**: `turn_dispatch` 는 거절된다(끝난 일에 행동을 허가할 수 없다).
  `turn_patch` 로 `request`·`instruction`·`response`·`explanation` 을 **다시
  쓰는 것도 거절**된다 — 그 네 개는 사람이 이미 본 원문이다. 고쳐야 하면
  `ended_at: null` 로 다시 열거나 다음 턴을 추가한다. 질문·선택지·산출물·
  session_seq 정리는 그대로 허용된다.
- **멱등 replay**: 같은 `request_id` 는 그때 커밋된 답을 돌려주고 **카운터를
  다시 올리지 않는다** — replay 가 두 번째 실행 허가가 되지 않는다. 같은
  operation_key 라도 **낡은 카운터**로는 거절되므로, 자기가 처음이라고 믿는
  재시도가 두 번 행동할 수 없다.
- **다른 operation_key**: 이미 기장된 턴에 다른 key 는 `observed=not_accepted`
  (앞 시도가 닿지 않았음을 확인했다는 진술) 없이는 거절된다.

**`response` 와 `explanation` 을 바꿔 쓰지 않는다.** `response` 는 일을 한
세션이 **스스로 쓴 원문**이고, `explanation` 은 부모가 사람에게 쓴 문장이다.
부모의 요약을 `response` 에 넣으면 세션이 하지 않은 말을 한 것으로 남는다.
원장 시절 `result` 는 후자였으므로, 이관된 턴에서도 그것은 `explanation` 에
있고 `response` 는 **null** 이다.

보낼 선택지는 같은 `turn_patch` 의 `choices`(최대 6개,
`{label, request_text}`)로 쓴다. id 는 없다 — 사람이 고른 문장은 **다음 턴의
`request`** 로 들어오고, 낡은 턴의 선택지는 플랫폼이 거절한다.

## 4. 부모 기장 — 승인·커서·보존·취소·모니터

합의한 task/turn 에는 부모의 기장이 **없다**(그래서 합의한 모양이다). 이관은
그것을 `sp_work_task_legacy` 에 원문 그대로 보존했고, 읽고 쓰는 공식 경로는
두 갈래다. 어느 쪽인지는 **그 행을 작업 레코드가 소유하는지**로 갈린다.

### (a) 작업이 있는 행 — `work_legacy_get` / `work_legacy_put`

```
work_legacy_get({task_id})
  → {task_id, kind, legacy_status, stage, owner,
     sessions: [...커서와 seq 포함...], details: {...승인·보존·취소...},
     task_version,          # 이 기장이 가리키는 작업 상태(참고용)
     book_version}          # 이 레인의 CAS 값 — put 에 들고 갈 값

work_legacy_put({task_id, request_id,
                 expected_book_version: <book_version>,
                 record: {stage, owner, sessions, details}})   # 전체 교체
```

- **전체 교체**다. 커서 하나만 바꾸려면 `work_legacy_get` → 수정 →
  `work_legacy_put`(read-modify-write). 일부만 담아 보내면 나머지가 사라진다.
- `expected_book_version` 은 **이 레인의 카운터**이고, 교체가 성공하면 1 오른다.
  두 부모가 같은 값을 들면 한 쪽만 성공한다(다른 쪽은 `conflict`).
- 이 레인은 **작업 version 도, 턴 레인도 건드리지 않는다**. 그래서 커서를
  전진시켜도 턴이 내놓은 선택지가 무효화되지 않는다 — 선택지는 작업의
  **version** 을 기준으로 삼고, 그 값은 **진짜 작업 변경(`task_put`)** 에서만
  움직인다. 실측으로 고정되어 있다.
- 같은 `request_id` 는 그때 커밋된 답을 되돌려 준다(두 번 쓰지 않는다).
- 작업이 커토버 뒤에 생겨 기장이 아직 없으면 **빈 레코드**가 온다(`not_found`
  가 아니다). 거기서부터 쓰기 시작하면 된다.

### (b) 작업이 없는 보존 행(모니터 등) — `ledger_put` 그대로

모니터 행은 사람의 작업이 아니어서 task 가 되지 않았다. 작업 레코드가 읽지
않으므로 갈릴 것이 없고, **옛 도구가 그대로 유효하다**:
`ledger_get`/`ledger_list` 로 읽고 `ledger_put` 으로 쓴다. `work_legacy_put`
으로 쓰려 하면 `conflict` 가 「그것은 보존 행이고 `ledger_put` 이 쓰기
경로다」라고 말해 준다.

### 막히는 것과 막히지 않는 것 (실측)

| 호출 | 커토버 후 |
| --- | --- |
| `ledger_get` · `ledger_list` · `ledger_events_list` | **허용** (읽기는 그대로) |
| `ledger_put` — 모니터/보존 행 | **허용** |
| `ledger_put` — 작업이 있는 id | 거절 → `task_put`·`turn_*`·`work_legacy_put` |
| `ledger_put` — **새 id 생성** | 거절 → `task_put` (원장에 쓰면 아무도 못 본다) |
| `ledger_choices_*` · `ledger_requests_claim/dispatch/ack` | 거절 → `turn_*` |

(`TestWorkCutoverLeavesTheParentAbleToWork` 가 이 표를 그대로 고정한다.)

## 5. 이력

- **옮겨온 작업**: 원장 이력이 `sp_ledger_task_events` 에 그대로 있고
  `ledger_events_list({id})` 로 읽는다. 이관은 **지난 관리 이벤트를 턴으로
  꾸미지 않았다** — 그러면 사람이 하지 않은 말이 사람의 말 자리에 들어간다.
- **커토버 뒤에 생긴 작업**: 원장 이력이 없고 **만들어 내지 않는다**. 그
  작업의 이력은 `turn_list` 로 읽는 턴들이다(요청→지시→답→설명, 시각 포함).
- 앱이 읽는 경로도 같게 갈린다(§세션 원장 계약 「Reading the work from
  outside the session」).

## 6. 롤백이 필요해지면

옛 이미지로 되돌리는 것은 **단계**다. 그냥 되돌리면 커토버 뒤에 쓴 작업·턴·
요청이 옛 이미지가 읽지 않는 테이블에 남아 **사람 화면에서 사라진다**.

```
work_rollback_export({request_id, dry_run: true})   # 리허설, 아무것도 안 바꾼다
work_rollback_export({request_id})                  # 스냅샷 → 원장 모양으로 되쓰기
                                                    # → state=rolled_back
```

- `rolled_back` 이면 **원장이 정본**이다: 작업 도구 쓰기는 거절되고 원장
  쓰기가 다시 허용된다. 부모는 커토버 전 절차로 돌아간다.
- 다시 앞으로 올 때는 `work_migrate` 를 부른다. 키가 있어 중복되지 않고,
  옛 이미지가 그동안 고친 것은 **reconcile 되어 들어온다**(`tasks_reconciled`).
- `turns_unexportable` 로 보고되는 턴(요청이 없는 턴 — 질문만 있는 턴 등)은
  원장 모양에 담을 자리가 없어 작업 레코드에만 남는다. 사람에게 그 사실을
  말한다. **조용히 버리지 않는다.**

## 7. 스킬이 바뀌어야 하는 지점 (파일 단위)

이 레포에는 부모 스킬 정의가 없다(별도 marketplace 레포). 아래는 그 스킬이
고쳐야 하는 지점의 정확한 목록이며, 플랫폼 쪽 근거 파일을 함께 적는다.

| 스킬이 바꿀 것 | 무엇으로 | 플랫폼 근거 |
| --- | --- | --- |
| 작업 발견·읽기·쓰기 호출부 | `task_list`·`task_get`·`task_put`·`task_delete` | `data-plane/cmd/agent/session_work_tasks.go` |
| 요청 큐 루프(`ledger_requests_*`) | `turn_list({open:true})` → `turn_patch`(접수) → `turn_dispatch` → `turn_patch`(종료). **매 쓰기에 직전 `turn_version`** | `session_work_turns.go`·`session_work_guard.go` |
| 선택지 발행 | `turn_patch` 의 `choices` | `session_work_turns.go` |
| 승인·커서·보존·취소 기장 | `work_legacy_get`/`work_legacy_put` (RMW) | `session_work_parent.go` |
| 모니터 루프 기록 | `ledger_put` **유지** | `session_work_parent.go`(fence 규칙) |
| 상태 분기 | 먼저 `work_migration_report` 의 `state` 확인 | `session_work_migrate.go` |
| 롤백 절차 | `work_rollback_export` → 이미지 되돌리기 → `work_migrate` | `session_work_rollback.go` |
| 「최종 원문 저장」 규칙 | `response`=세션 원문, `explanation`=부모 설명 | `session_work.go`(workTurn 주석) |

**생략하면 안 되는 것**: 요청 큐 루프와 기장 경로. 전자를 빼면 사람이 남긴
요청이 처리되지 않고, 후자를 빼면 승인 범위와 세션 커서가 갱신되지 않아 부모의
대조가 틀어진다.

## 8. 아직 하지 않은 것

- 이 문서는 **플랫폼 쪽 계약과 실측**이다. marketplace 레포의 스킬 본문 수정은
  이 저장소 밖이며, 이 세션에서 하지 않았다.
- 슬롯 저장소에서의 스냅샷 복원 리허설은 주장하지 않는다 — 격리 fixture DB
  왕복만 검증했다(세션 원장 계약 §「Verification and rollout boundary」).
