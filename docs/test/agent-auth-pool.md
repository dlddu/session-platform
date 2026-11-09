# 테스트 문서: 에이전트 인증 풀

## 검증 대상 AC
- AC-N1: 슬롯 = 이름이 고정된 PVC, control plane이 만들고 지우지 않는다 (PRD: 에이전트 인증 풀)
- AC-N2: 독점 임대 = 슬롯마다 Lease 하나, holder는 세션 id (PRD: 에이전트 인증 풀)
- AC-N3: 반납 = 동결 완료·삭제 시 (PRD: 에이전트 인증 풀)
- AC-N4: 로그인은 슬롯 파일, 에이전트가 HOME과 동기화하고 아카이브에서 뺀다 (PRD: 에이전트 인증 풀)
- AC-N5: 권한 — control plane은 PVC·Lease만, 세션은 자기 슬롯만 (PRD: 에이전트 인증 풀)
- AC-N6: `codex-chat` 슬롯 = 로그인 + 세션 DB, 컨테이너마다 제 몫만 마운트 (PRD: 에이전트 인증 풀)

> 📌 **SUT 설정**: kind e2e 오버레이가 `codex-task`를 켜고 풀 크기를 2로 둔다(`CODEX_TASK_AUTH_SLOTS`). 슬롯 PVC는 kind 기본
> 스토리지 클래스(local-path)로 만들어진다. `claude-code`는 e2e에서 `gateway` 모드라 풀을 쓰지 않는다(AC-E7) — 풀 시나리오는
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

### 시나리오 5: `codex-chat` 슬롯은 로그인과 세션 DB로 나뉘고, 세션 DB는 사이드카 MCP로만 닿는다
- **사전 조건**: `codex-chat` 풀이 있는 SUT(`CODEX_CHAT_AUTH_SLOTS` ≥ 1)와 그 클러스터의 kube 접근
- **실행 단계**: (a) `codex-chat` 세션을 만들고 그 워크로드 파드를 조회한다 → (b) 사이드카의 `/session-db/mcp`에 `raw_exec`로 테이블을 만들고 행을 넣는다 → (c) `raw_query`로 읽는다 → (d) 없는 테이블을 `raw_query`한다 → (e) 세션을 지우고 같은 슬롯을 받은 새 `codex-chat` 세션에서 (c)를 다시 한다
- **기대 결과**: (a) PVC 볼륨은 슬롯 `<네임스페이스>-codex-chat-auth-<i>` 하나다. 주 컨테이너는 subPath `auth`를 `/var/run/session-platform/auth-slot`에, `k3s-mcp-proxy`는 subPath `db`를 `/var/run/session-platform/session-db`에 마운트하고, 슬롯 전체를 마운트하는 것은 초기화 컨테이너 `slot-init`뿐이다. 주 컨테이너는 `SESSION_DB_MCP_URL=http://127.0.0.1:8093/session-db/mcp`를 받고 `SESSION_DB_DIR`은 받지 않는다. (b) `rowsAffected`가 1이다. (c) 넣은 행이 돌아온다. (d) `isError` 도구 결과에 SQLite의 메시지가 있다. (e) 앞 세션이 넣은 행이 그대로 있다
- **검증 AC**: AC-N6

