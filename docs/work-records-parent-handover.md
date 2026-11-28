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
| 작업 만들기·갱신 | `ledger_put` | `task_put` (단, `review` 를 드나드는 것은 §8-1 의 세 호출) |
| 작업 지우기 | `ledger_delete` | `task_delete` |
| 사람 요청 발견 | `ledger_requests_list` | `turn_list({open:true})` |
| 요청 접수 | `ledger_requests_claim` | `turn_patch` 로 `started_at` 쓰기 = 접수, 응답의 `claim_token` 보관(§3) |
| 외부 행동 기장 | `ledger_requests_dispatch` | `turn_dispatch` + `claim_token`(§3) |
| 결과 기록 | `ledger_requests_ack` | `turn_patch`(response·explanation·ended_at) + `expected_turn_version` |
| 보낼 선택지 | `ledger_choices_put` | `turn_patch`의 `choices` |
| 승인·커서·보존 기장 | `ledger_put`의 `details`·`sessions` | **`work_legacy_put`** + `expected_book_version` (§4) |
| 모니터 루프 기록 | `ledger_put` | **`ledger_put` 그대로** (§4) |
| 작업 이력 읽기 | `ledger_events_list` | 옮겨온 작업은 그대로, **새 작업은 그 턴들이 이력**(§5) |
| 롤백 | — | **없다 — 이관은 끝났고 되돌리는 단계는 제거됐다**(§6) |

**상태를 묻는 도구는 없다.** 이관이 끝났으므로 슬롯이 「어느 단계에 있는지」는
더 이상 사실이 아니고, 그것을 보고하던 `work_state` 도 제거됐다 — 다른 이름으로
되살리지도 않았다. 대신 **부모 자신의 호출이 답이다**: 모든 work 호출은 스키마
게이트를 먼저 지나므로, 호출이 답을 받았다는 것이 곧 「이 슬롯은 서비스된다」는
진술이고, 거절은 이유를 이름으로 말해 준다.

## 1. 전환 순서 (부모가 직접 하는 일)

이관은 끝났다. 전환 도구(`work_migration_report`·`work_migrate`·
`work_rollback_export`)와 퇴역 도구(`work_retire`·`work_backups`·
`work_backups_prune`), 그리고 상태 보고(`work_state`)는 이 이미지에 **없다**.
전환을 위해 부모가 **먼저 불러야 하는 호출은 하나도 없다**:

1. 평소 하던 일을 그대로 한다 — `task_list`·`turn_list` 로 읽고, 이 문서의
   도구로 쓴다. 상태 분기를 위한 선행 호출은 필요하지 않다.
2. 제거된 이름을 부르면 refusal 이 **무엇이 사실인지** 말해 준다. 그 메시지를
   사람에게 그대로 전한다. 그 답은 다른 관리 도구를 가리키지 않는다 — 가리킬
   것이 없다.
3. 거절 코드 `schema_version` 은 **이 슬롯은 아직 원장을 들고 있다**(또는 옛
   이미지가 이관을 끝내지 못했다)는 뜻이다. 아무것도 읽거나 바꾸지 않았고 원장은
   그대로 있다. refusal 원문에 **업그레이드 경로**가 담겨 있다 — 그 슬롯은
   `work_retire` 가 아직 있는 이미지로 먼저 옮긴 뒤 이 이미지를 올린다. 사람에게
   그 사실과 원문을 전하고, 임의로 재시도하거나 우회하지 않는다(§6).

## 2. 발견 — 전수로 읽는 법

```
task_list({limit: 100})                  # 전체. next_cursor 를 after_id 로 이어 읽는다
task_list({status: "review"})            # 사람이 판단해야 하는 것만
task_list({status: "blocked"})           # 같은 집합. 옛 낱말은 받아들여지는 별칭이다
task_list({session_id: "none"})          # 배정되지 않은 것만 ("none" 은 리터럴)
```

- **상태는 `planned`·`running`·`review`·`completed`·`unknown` 다섯이다.**
  `blocked` 는 상태가 아니고 **입력의 별칭**이다 — 필터와 `task_put` 에서 받아
  `review` 로 읽히고, 그 말로 저장되는 행은 없다(`task_put` 의 답이
  `alias_applied` 로 말해 준다). 옛 이미지가 그 말로 적어 둔 행은 읽을 때
  근거로 갈려 `review` 또는 `running` 으로 답하고, 저장된 낱말과 무엇이 그렇게
  읽었는지가 `stored_status`·`status_basis` 로 함께 온다(§8-1).
- 페이지는 **스냅샷이 아니다** — id 로 dedupe 하고, `next_cursor` 가 빈
  문자열일 때만 끝이다. **짧게 온 페이지도 커서가 있으면 끝이 아니다** —
  상태 판정이 섞인 페이지는 스캔 상한에서 끊길 수 있고, 그때 빈 커서 대신
  커서를 준다.
- `task_get({id})` 는 `turns_total` 과 `latest_turn` 을 함께 준다. 사람이
  무언가 물어봤는지 알려고 턴 목록을 따로 읽을 필요가 없다.
- **모니터 행은 `task_list` 에 없다.** 그것은 부모의 기장이고 작업이 아니다
  (§4). 모니터 행은 `ledger_list` 로 계속 읽는다.

## 3. 사람 요청 소비 — claim 이 없어진 이유와 대신 할 일

원장에서는 요청이 작업 **옆의 행**이었으므로 누가 가져갔는지 표시할
`claim` 이 필요했다. 턴은 작업 **안의 사건**이지만, **소유는 여전히 토큰으로
표시해야 한다** — 카운터는 동시 쓰기를 가릴 뿐 누가 쥐었는지 말하지 못한다.
그래서 접수는 이렇게 한다:

```
1) turn_list({task_id, open: true})
   → request 가 있고 ended_at 이 없는 턴 = 아직 부모가 할 일
   → 응답의 turn_version 을 기억한다 (이 레인의 CAS 값)
2) 그 턴을 집는다  turn_patch({task_id, turn_id, request_id,
      expected_version: <task_get 의 현재 version>,
      expected_turn_version: <1)에서 읽은 turn_version>,
      turn: {started_at: <지금>, instruction: <세션에 넘긴 원문>}})
   → 응답의 **claim_token** 을 보관한다. 이것이 「이 턴을 내가 쥐고 있다」는
     증거이고 turn_dispatch 가 요구한다. 읽기 뷰에는 절대 담기지 않는다.
3) 외부 행동 전에  turn_dispatch({..., expected_turn_version: <다시 읽은 값>,
      claim_token: <2)에서 받은 토큰>,
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
- **소유** = 집은 부모만 그 턴을 위해 외부 행동을 기장할 수 있고, 그것을
  증명하는 것은 **`claim_token`** 이다. 아무도 집지 않은 턴의 `turn_dispatch`
  는 거절되고, 토큰이 없거나 맞지 않는 호출도 거절된다. 턴을 반납하면
  (`started_at: null`) 토큰은 **파기**되므로 놓은 일에 계속 행동할 수 없고,
  다시 집으면 **다른 토큰**이 발급돼 낡은 토큰은 계속 거절된다.

  > **이전 문서는 이것을 보장하지 않은 채 주장했다.** `dispatchWorkTurn` 은
  > `started_at != nil` 만 보았고 `started_at` 은 시각이지 신원이 아니다.
  > 실측 결과 **카운터만 다시 읽은 다른 호출자의 dispatch 가 그대로 허용**
  > 됐다. 그래서 claim 토큰을 내부관리 영역(`sp_work_turn_claim`)에 넣어
  > 실제로 막는다 — 합의한 task/turn 에는 필드를 더하지 않았다.
- **종료된 턴**: `turn_dispatch` 는 거절된다(끝난 일에 행동을 허가할 수 없다).
  `turn_patch` 로 `request`·`instruction`·`response`·`explanation` 을 **다시
  쓰는 것도 거절**된다 — 그 네 개는 사람이 이미 본 원문이다. 고쳐야 하면
  `ended_at: null` 로 다시 열거나 다음 턴을 추가한다. 질문·선택지·산출물·
  session_seq 정리는 그대로 허용된다.
- **멱등 replay**: 같은 `request_id` 는 그때 커밋된 답을 돌려주고 **카운터를
  다시 올리지 않는다**. 같은 operation_key 라도 **낡은 카운터**로는 거절된다.
- **같은 key·같은 target 의 재기장은 허용된다**(새 `request_id` 로도). 그것이
  idempotency key 의 용도이고, 중복을 걸러내는 쪽은 **외부 시스템**이다.
- **되감기 금지**: 한 번 `dispatched` 로 적힌 턴은 `intended` 로 돌아갈 수
  없다 — 넘어갔다는 사실을 지우면 뒤에 읽는 쪽이 아무것도 나가지 않은 것으로
  믿는다.
- **key 는 target 을 바꿀 수 없다**: 같은 operation_key 로 다른
  `operation_target` 을 적으려 하면 거절된다. key 가 가리키는 행동이 달라지면
  기록이 한 일과 다른 것을 말하게 된다.
- **다른 operation_key**: 이미 기장된 턴에 다른 key 는 `observed=not_accepted`
  (앞 시도가 닿지 않았음을 확인했다는 진술) 없이는 거절된다.

> **이 플랫폼은 외부 exactly-once 를 보장하지 않는다.** 보장하는 것은 이것뿐
> 이다: 한 턴에 **한 번에 하나의 기록된 의도**, 그것을 쓸 수 있는 **한 명의
> 소유자**, **되감기 불가**, **key→target 불변**. 바깥에서 그 행동이 한 번만
> 일어나게 하는 것은 `operation_key` 로 중복을 걸러내는 **외부 시스템의
> 몫**이고, 그 key 를 먼저 적고 나서 행동하는 것이 부모의 몫이다. CAS 카운터는
> 동시 쓰기를 가려 주지만 바깥 호출을 되돌리지는 못한다.

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

## 6. 되돌릴 수 없다 — 그래서 무엇을 하는가

롤백 경로는 제거됐다. 그것은 손실이 아니라 **이미 사실인 것을 코드가 인정한
것**이다: 커토버 뒤에 쓴 작업·턴·요청은 옛 이미지가 읽지 않는 테이블에 있고,
되돌리기는 그것을 사람 화면에서 사라지게 만든다. 그래서 되돌리는 단계 대신
**닫히는 쪽으로 실패하는 게이트**가 있다.

- 이미지를 되돌리는 것 자체가 막힌다: 퇴역한 데이터베이스는 스키마 버전 3을
  들고 있고, 그 버전을 모르는 옛 이미지는 **첫 호출에서 거절한다** — 없어진
  원장을 빈 원장으로 되살려 서비스하는 대신이다.
- 아직 원장을 들고 있는 슬롯은 이 이미지가 **거절한다**(`schema_version`).
  원장 행은 한 바이트도 건드리지 않으므로, `work_retire` 가 아직 있는 이미지로
  그 슬롯을 옮기는 길은 남아 있다. 그 결정과 배포는 사람의 것이다.
- 어느 쪽이든 부모가 할 일은 같다: refusal 원문과 코드를 사람에게 그대로
  전하고, 우회하지 않는다. **조용히 빈 목록을 보여 주는 것이 가장 나쁜
  결과다.**

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
| 상태 분기 | **제거.** 선행 상태 조회 호출이 없다 — 자기 호출의 응답·거절이 답이다 | `session_work.go`(`migrateWork`) |
| 롤백 절차 | **없다.** 되돌리기는 제거됐고 스키마 버전 게이트가 닫히는 쪽으로 막는다(§6) | `session_work.go`(`migrateWork`) |
| 「최종 원문 저장」 규칙 | `response`=세션 원문, `explanation`=부모 설명 | `session_work.go`(workTurn 주석) |

**생략하면 안 되는 것**: 요청 큐 루프와 기장 경로. 전자를 빼면 사람이 남긴
요청이 처리되지 않고, 후자를 빼면 승인 범위와 세션 커서가 갱신되지 않아 부모의
대조가 틀어진다.

## 8. 아직 하지 않은 것

- 이 문서는 **플랫폼 쪽 계약과 실측**이다. marketplace 레포의 스킬 본문 수정은
  이 저장소 밖이며, 이 세션에서 하지 않았다.
- 슬롯 저장소에서의 스냅샷 복원 리허설은 주장하지 않는다 — 격리 fixture DB
  왕복만 검증했다(세션 원장 계약 §「Verification and rollout boundary」).

## 8-1. 사람이 판단하는 상태는 `review` 하나다 (AC-N13)

`blocked` 와 `review` 는 같은 사실을 두 이름으로 말하고 있었다. 하나로 접었고
(`review`), **대신 세운 새 상태·새 칩은 없다**. 옛 낱말은 §2 의 별칭이고,
옛 이미지가 적어 둔 행은 읽을 때 근거로 갈린다 — 디스크는 고치지 않는다.

### 상태를 바꿀 때 부르는 것

```
# running -> review : 사람이 결정해야 할 것이 생겼다. 같은 논리 턴에 기록하고 옮긴다.
task_review_open({task_id, request_id, turn_id: <지금 쓰고 있는 턴>,
    expected_version, expected_turn_version, expected_book_version,
    review: {
      target:   "dlddu/session-platform",            # 정확한 대상. 분류가 아니다
      scope:    "contents:read 한 레포, 이 작업 동안만",
      route:    "저장소 소유자가 App 설치 범위에 레포를 더한다",   # 공식 승인 경로
      question: "...읽기 권한을 더해 주시겠습니까? ...",          # 축자 저장된다
      basis:    "git ls-remote 가 403 이고 설치 범위에 이 레포가 없다",
      choices:  [{label, request_text}, ...]          # 초안용. 권한이 아니다
    }})

# review -> running : 내용을 확인하고 실제 적용·전제·이어갈 일을 확인한 뒤
task_review_resume({task_id, request_id, expected_version, expected_book_version,
    outcome: {evidence: "무엇으로 확인했는지", remaining: "남은 일"}})

# review -> completed : 최종 결과 수락 + 원래 완료 기준 충족 + 남은 것 없음
task_review_complete({task_id, request_id, expected_version, expected_book_version,
    outcome: {accepted: true,
              evidence: "그 수락을 무엇으로 관측했는지",      # 제출·보류·설명은 수락이 아니다
              completion_criteria: "<작업의 criteria 축자>"}})
```

- **같은 승인을 두 번 만들지 않는다.** 같은 `target·scope·route` 를 다시 물으면
  `already_open: true` 로 **아무것도 쓰이지 않는다**(카운터·version 불변) — 다른
  `request_id` 여도, 사람이 새 요청을 남긴 뒤여도. 열린 승인이 있는데 **다른**
  결정을 물으면 `conflict` 다. 판정은 카운터를 집기 전에 하므로 낡은 값을 쥔
  경합도 승인을 쌓지 못한다.
- **보류·승인 미적용이면 review 다.** `evidence` 나 `remaining` 이 비면 resume
  은 거절되고 작업은 그대로 있다. 플랫폼은 바깥의 적용 여부를 볼 수 없으므로
  **확인을 이름으로 요구**하고 그것을 작업 옆에 남긴다 — 추측으로 옮기지 않는다.
- **리뷰를 떠나는 판정은 상태 낱말이 아니라 열린 기록이다.** 앱 요청이 저장되면
  검토를 기다리던 작업은 같은 트랜잭션에서 `planned` 로 접수되지만(사람이 문장을
  보낸 것으로 결정이 정리되지는 않으므로) 기록된 리뷰는 **열린 채 남는다**. 그래서
  `task_review_resume`·`task_review_complete` 는 **at review 이거나 열린 리뷰를 든**
  작업에서 돈다 — `planned` 에서도 근거를 실어 닫을 수 있고, 이력 항목의
  `from_status` 가 큐에서 닫혔다는 것을 남긴다.
- **열린 리뷰를 버려두고 지나갈 수 없다.** 열린 리뷰가 있는 작업에 `task_put` 으로
  `running`·`completed` 를 쓰면 거절된다 — 「도는 중」으로 읽히는데 사람이 기다린다는
  기록은 읽기 경로에 없는 곳에만 남기 때문이다. 전이 제약이 아니다(어느 상태에서
  어디로가 아니라 **버려두고 갈 기록이 있는가**를 묻는다): `planned`·`review`·
  `unknown` 은 거절되지 않고, 기록이 없는 작업은 전과 같이 어디로든 가며, 지나갈
  길이 둘 있다 — 근거를 실은 두 호출로 닫거나 `task_book_put` 으로 기장을 교체한다.
- **완료는 열린 리뷰를 닫는다** — 사용자가 결과를 수락하는 것이 곧 그 결정의
  정리다. 대신 `outcome.evidence` 로 **그 수락을 무엇으로 관측했는지** 말해야 한다:
  제출만·보류·설명·적용되지 않은 승인은 이름 붙일 수 없으므로 완료가 되지 않는다.
- **완료는 다섯 가지를 다 본다**: 수락 여부와 그 **관측 근거**, 작업 자신의 완료 기준 **축자 일치**,
  남은 것 없음, `ended_at` 없는 요청 0, 미적용 결정 없음. 완료 기준이 적혀 있지
  않은 작업은 이 길로 완료되지 않는다 — `task_put` 으로 먼저 쓴다.
- **이 셋은 울타리가 아니라 완전한 길이다.** `task_put` 은 도메인 안의 어떤 상태든
  **그대로 쓴다** — 작업 레코드는 전체 교체 + version CAS 이고 상태 값에 **전이 제약이
  없다**(목표 상태 문서의 「`completed` 의 정정」이 그 성질에 기댄다). 셋이 더하는 것은
  상태가 뜻을 갖게 하는 **기록**이고, 울타리 없이 쓴 `review` 는 아무도 결정을 기록하지
  않은 작업이라 화면에도 「묻는 것: 미기록」으로 보인다. 판단이 필요 없던 **기승인 정상
  경로는 planned → running → completed 그대로**다.
- **잘못 적힌 `completed` 는 한 번의 `task_review_open` 으로 정정한다.** 사용자가 결정한
  적이 없는데 완료로 적힌 작업은, 사용자의 정정 진술과 모니터의 턴 대조를 `review.basis`
  에 실어 그 호출로 `review` 로 되돌린다 — `task_review_open` 은 **출발 상태를 가리지
  않으므로** 없던 `running` 을 한 번 거치지 않아도 된다. 턴은 손대지 않는다.
- **세 가지는 별개다**: 선택지를 누르는 것(초안 채움) → 사용자가 제출(요청 경로로
  **턴 하나**, 상태 불변) → 모니터가 읽고 확인한 뒤 `task_review_resume`.
- **기술 대기는 running 에 머문다.** CI 진행 중이거나 정책상 재시도 가능한 대기는
  아무에게도 질문이 아니다. 그 근거는 `task_book_put` 의 `details` 에
  `technical_wait: {owner, due, retry}` 로 **세 가지를 전부** 적는다 — 셋이 다
  있는 구조화된 statement 만 옛 `blocked` 행을 `running` 으로 읽는다. 하나라도
  빠지거나 산문이면 `review` 로 읽히고 근거가 무엇이 없는지 말해 준다.
- **보존**: 덮는 승인 기록은 `details.review_history` 에 **누적**되고, 레거시 행을
  resume/complete 하면 **저장된 낱말과 그 근거**가 그 항목에 들어간다. 턴의
  `request`·`instruction`·`response`·`explanation`·`ended_at` 은 어느 호출도
  건드리지 않는다. 읽을 수 없는 JSON 기장은 덮지 않고 거절한다.

### 스킬이 바꿀 것

| 스킬이 바꿀 것 | 무엇으로 | 플랫폼 근거 |
| --- | --- | --- |
| `status: "blocked"` 를 쓰던 자리 | 지우지 않아도 동작한다(별칭). 새로 쓰는 코드는 `review` | `data-plane/cmd/agent/session_work_review.go` |
| 상태 분기에서 `blocked`·`review` 를 둘 다 보던 자리 | `review` 하나. `stored_status` 가 있으면 옛 행이라는 뜻이고 `status_basis` 가 판정 근거다 | 같은 파일 |
| `task_put` 으로 review 를 쓰던 자리 | `task_review_open`·`task_review_resume`·`task_review_complete` | 같은 파일 |
| CI·재시도 대기를 막힌 것으로 적던 자리 | `running` 유지 + `details.technical_wait: {owner, due, retry}` | `session_work_parent.go` |
