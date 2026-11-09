# 테스트 문서: 세션 스케줄러

## 검증 대상 AC
- AC-P1: 스케줄러 MCP 도구 — 모델이 등록·조회·취소한다 (PRD: 세션 스케줄러)
- AC-P2: 회차는 claudeWorkload 큐로 들어가고, 세션이 한가할 때만 시작한다 (PRD: 세션 스케줄러)
- AC-P3: 예약이 남아 있는 동안은 유휴 회수를 미룬다 (PRD: 세션 스케줄러)

> 📌 **SUT 설정**: 스케줄러 MCP는 세션 파드의 loopback 사이드카에만 열려 있어, 시나리오 1·2는 세션 파드 안(예: `codex-task` 세션의 셸, 또는 그 파드에 exec)에서 `http://127.0.0.1:8093/scheduler/mcp`에 JSON-RPC를 보내 관측한다. 시나리오 2의 「턴」은 codex 러너가 도는 것이므로 그 세션의 ChatGPT 로그인이 필요하다.

## 테스트 시나리오

### 시나리오 1: 도구가 계산한 시각을 돌려주고, 해석하지 않는 입력과 한계를 거부한다
- **사전 조건**: `codex-chat`·`codex-task` 세션이 하나씩 떠 있다
- **실행 단계**: (a) 두 세션의 관리형 `~/.codex/config.toml`을 본다 → (b) `tools/list` → (c) `schedule_once{delay:"PT30M"}`, `schedule_once{at:"<9시간 뒤>+09:00"}`, `loop_create{interval:"PT5M"}`, `loop_create{}`(자체 페이싱) → (d) `schedule_list` → (e) `delay:"half an hour"`, `delay:"PT30S"`, `delay:"P8D"`, `interval:"PT25H"`, 오프셋 없는 `at`, 고정 루프 id로 `loop_next`, 21번째 예약 → (f) `schedule_cancel`로 하나 지운다
- **기대 결과**: (a) 두 설정 모두 `[mcp_servers.scheduler]`가 사이드카의 `/scheduler/mcp`를 가리킨다. (b) 도구 다섯(`schedule_once`·`loop_create`·`loop_next`·`schedule_list`·`schedule_cancel`). (c) 각각 id와 UTC RFC 3339 `nextRunAt`을 돌려주고, 시각은 요청에서 계산한 값과 같다(루프는 지금). 자체 페이싱 루프의 `fallbackInterval`은 `PT10M`, 루프의 `expiresAt`은 7일 뒤다. (d) (c)의 넷이 다음 실행 시각 순으로 나온다. (e) 전부 `isError` 결과이고 목록이 변하지 않는다(모르는 인자는 JSON-RPC 오류). (f) 지운 예약이 목록에서 빠진다
- **검증 AC**: AC-P1

### 시나리오 2: 회차는 큐의 턴이 되고, 바쁠 때는 기다리며, 루프 규칙대로 다음 회차가 정해진다
- **사전 조건**: 로그인된 `codex-task` 세션
- **실행 단계**: (a) `schedule_once{delay:"PT1M"}` 등록 → 1분 뒤 출력 확인 → (b) 긴 턴을 도는 프롬프트를 write한 직후 `schedule_once{delay:"PT1M"}` 등록 → (c) `loop_create{interval:"PT1M"}`을 등록하고, 회차 하나가 3분 넘게 걸리게 한다 → (d) 자체 페이싱 루프를 만들고 첫 회차가 `loop_next{delay:"PT2M"}`을 부르게, 둘째 회차는 아무것도 부르지 않게 한다
- **기대 결과**: (a) 출력에 `[session-platform scheduler] one-time schedule <id>` 첫 줄을 가진 입력 턴이 생기고 끝난 뒤 목록에서 사라진다. (b) 예약 회차는 사람의 턴이 끝난 **뒤에** 시작한다(두 턴이 겹치지 않고 순서가 사람 → 예약). (c) 긴 회차가 끝난 뒤 밀린 칸을 몰아서 돌리지 않고 지금 뒤의 첫 칸에 한 번 돈다. (d) 둘째 회차는 첫 회차가 정한 2분 뒤에 돌고, 셋째 회차는 둘째가 끝난 때부터 기본 간격 뒤로 잡힌다(`schedule_list`)
- **검증 AC**: AC-P2

### 시나리오 3: 예약이 남은 codex 세션은 유휴 회수 경로가 건너뛰고, 예약이 끝나면 회수된다
- **사전 조건**: 유휴 기준(AC-B1)을 짧게 둔 SUT, 동결 게이트가 켜진 `codex-chat` 세션 둘
- **실행 단계**: (a) 한 세션에 7일짜리 루프를 등록하고 클라이언트 접근 없이 유휴 기준을 넘긴다 → (b) 그 루프를 취소하고 다시 유휴 기준을 넘긴다 → (c) 다른 세션에 루프를 등록한 뒤 사용자 동결을 요청한다 → (d) 사이드카가 답하지 않는 세션(예: 사이드카 프로세스 정지)을 유휴 기준 넘게 둔다
- **기대 결과**: (a) 세션은 `active`로 남고 `lastAccess`가 회수 경로 시각으로 갱신된다. (b) 다음 스캔에서 동결된다. (c) 동결되고, 복원된 세션의 `schedule_list`는 비어 있다. (d) 일반 규칙대로 동결된다. `claude-code` 세션은 회수 경로가 예약을 묻지 않는다
- **검증 AC**: AC-P3
