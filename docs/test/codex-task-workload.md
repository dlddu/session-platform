# 테스트 문서: 세션 워크로드 — Codex 작업용 (codex-task)

## 검증 대상 AC
- AC-M1: 워크로드 타입 `codex-task` — claude-code 수준의 도구 표면 (PRD: Codex 작업용 워크로드)
- AC-M2: 로그인은 슬롯의 `auth.json`, 빈 슬롯은 첫 프롬프트에서 device code (PRD: Codex 작업용 워크로드)
- AC-M3: 파일시스템 아카이브, 로그인 제외, 전용 게이트 (PRD: Codex 작업용 워크로드)
- AC-M4: 모델과 추론 수준을 세션마다 정한다 (PRD: Codex 작업용 워크로드)

> ⚠️ **e2e 범위 주의**: kind SUT는 `codex-task` 파드를 실물로 띄우지만(같은 로컬 빌드 이미지, 풀 크기 2) ChatGPT 로그인은
> 할 수 없다. 로그인 뒤의 턴(시나리오 3)은 `e2e.md`의 예외이고, 러너의 작업 모드 옵션·로그인 흐름은 data plane 단위 테스트가 맡는다.

## 테스트 시나리오

### 시나리오 1: `codex-task` 타입을 골라 세션을 만들면 에이전트 작업 화면이 열린다
- **사전 조건**: `DATA_PLANE_CODEX_TASK_IMAGE`와 풀이 설정된 SUT를 운영자로 연 브라우저
- **실행 단계**: (a) 새 세션 화면을 연다 → (b) `Codex task` 카드를 고르고 추론 수준을 하나 골라 세션을 만든다 → (c) 만든 세션의 작업 화면을 본다
- **기대 결과**: (a) 타입 카드에 `Codex task`가 있고, 고르면 추론 수준 선택지(기본 「CLI default」)가 보인다. (b) 세션이 `active`로 만들어지고 목록 카드의 타입 태그가 `codex-task`다. (c) 프롬프트 입력과 대화 기록이 있는 에이전트 작업 화면이다
- **검증 AC**: AC-M1, AC-M4

### 시나리오 2: 파드 모양 — 사이드카 하나, SA 토큰, 슬롯 PVC
- **사전 조건**: SUT와 그 클러스터의 kube 접근
- **실행 단계**: `codex-task` 세션을 만들고 그 워크로드 파드를 조회한다
- **기대 결과**: 보조 파드가 없다. 컨테이너가 둘(주 컨테이너, `k3s-mcp-proxy`)이고 `automountServiceAccountToken`이 true다. 주 컨테이너에 Secret 참조가 없고 `DATA_PLANE_WORKLOAD=codex-task`, `K3S_MCP_PROXY_URL=http://127.0.0.1:8093`, `AGENT_AUTH_SLOT_DIR`을 받는다. `k3s-mcp-proxy`만 `codex-task-credentials`의 선택 `k3s-mcp-token`을 읽고 `K3S_MCP_TOOL_POLICY=allow-all`을 받는다
- **검증 AC**: AC-M1

### 시나리오 3: 빈 슬롯을 받은 첫 프롬프트가 device code 로그인을 띄우고, 다음 세션은 로그인하지 않는다
- **사전 조건**: 빈 슬롯 하나뿐인 풀, ChatGPT 구독 계정
- **실행 단계**: (a) `codex-task` 세션을 만들어 프롬프트를 보낸다 → (b) 안내된 주소에서 코드를 넣고 승인한다 → (c) 세션을 지우고 새 `codex-task` 세션에 프롬프트를 보낸다
- **기대 결과**: (a) 출력에 확인 주소와 일회용 코드가 플랫폼 안내 줄로 나타난다. (b) 같은 턴이 실행된다. (c) 로그인 안내 없이 실행된다
- **검증 AC**: AC-M2

### 시나리오 4: 추론 수준은 생성 시 정해지고 파드까지 간다
- **사전 조건**: SUT와 그 클러스터의 kube 접근
- **실행 단계**: (a) `GET /config`를 읽는다 → (b) `reasoningEffort=high`로 `codex-task` 세션을 만든다 → (c) `reasoningEffort=max`로, 그리고 `""`로 만든다
- **기대 결과**: (a) `codex-task` 항목의 `reasoningEfforts`가 `minimal`·`low`·`medium`·`high`·`xhigh`다. (b) 조회 응답의 `reasoningEffort`가 `high`이고 주 컨테이너가 `CODEX_REASONING_EFFORT=high`를 받는다. (c) 둘 다 400이고 파드가 없다
- **검증 AC**: AC-M4

### 시나리오 5: 동결·복원 뒤에도 대화와 출력이 이어지고 로그인 파일은 아카이브에 없다
- **사전 조건**: `CODEX_TASK_ARCHIVE_ENABLED` on, 로그인된 슬롯
- **실행 단계**: (a) 프롬프트를 보낸 세션을 동결한다 → (b) 세션에 접근해 복원한다 → (c) 프롬프트를 보낸다
- **기대 결과**: (a) 파드가 회수되고 슬롯이 반납되며 아카이브에 `home/.codex/auth.json`이 없다. (b) 새로 임대한 슬롯으로 `active`가 되고 출력·커서가 이어진다. (c) 로그인 안내 없이 앞 대화를 이어 답한다
- **검증 AC**: AC-M3
