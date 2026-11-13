# 테스트 문서: 세션 워크로드 — Codex SDK 채팅용 (codex-chat)

> 📌 **2026-11-07**: 타입 이름이 `codex`에서 `codex-chat`으로 바뀌었다(AC-J1). 아래 시나리오의 「`codex` 세션」은 `codex-chat` 세션이다. 작업용 `codex-task`는 `codex-task-workload.md`.

## 검증 대상 AC
- AC-J1: 워크로드 타입 `codex-chat` 선택 (옛 이름 `codex`) (PRD: Codex 워크로드)
- AC-J2: write = Codex SDK 턴 1회, read = 에이전트 메시지 텍스트 (PRD: Codex 워크로드)
- AC-J3: ChatGPT 로그인(device code), 로그인은 슬롯에 남는다 (PRD: Codex 워크로드)
- AC-J4: 파일시스템 아카이브, 전용 게이트 (PRD: Codex 워크로드)
- AC-J5: 도구 표면 — 대화, 플랫폼 플러그인 스킬, K3s MCP, 세션 DB뿐 (PRD: Codex 워크로드)
- AC-J6: K3s MCP 토큰은 loopback 사이드카에만 (PRD: Codex 워크로드)
- AC-J7: 수동 플러그인 새로고침 — 대화를 유지한 채 스킬을 다시 받는다 (PRD: Codex 워크로드)

> ⚠️ **e2e 범위 주의**: kind SUT는 codex 세션 파드를 실물로 띄울 수 있지만(같은 로컬 빌드 data plane 이미지,
> 오버레이가 `DATA_PLANE_CODEX_IMAGE`를 그 이미지로 바꾼다) **ChatGPT 로그인은 할 수 없다** — device code 승인은
> 사람의 브라우저와 실 ChatGPT 계정이 필요하다. 그래서 로그인 뒤의 대화 왕복·복원 후 로그인 유지(시나리오 3·5)는
> `e2e.md`의 예외이고, 그 부분의 현재 검증은 data plane 단위 테스트(러너 투영·킬 스위치·아카이브 대상 트리·슬롯 동기화)다.

## 테스트 시나리오

### 시나리오 1: `codex-chat` 타입을 골라 세션을 만들면 에이전트 작업 화면이 열린다
- **사전 조건**: `DATA_PLANE_CODEX_IMAGE`가 설정된 SUT를 운영자로 연 브라우저
- **실행 단계**: (a) 새 세션 화면을 연다 → (b) `Codex chat` 타입 카드를 골라 세션을 만든다 → (c) 만든 세션의 작업 화면을 본다
- **기대 결과**: (a) 타입 카드에 `Codex chat`이 있다. (b) 세션이 `active`로 만들어지고 목록 카드의 타입 태그가 `codex-chat`이다. (c) 셸 콘솔이 아니라 프롬프트 입력과 대화 기록이 있는 에이전트 작업 화면이고, 첫 프롬프트가 ChatGPT 로그인을 시작한다는 안내가 보인다
- **검증 AC**: AC-J1

### 시나리오 2: 파드 모양 — 사이드카 하나, 자격 증명은 사이드카에만
- **사전 조건**: SUT와 그 클러스터의 kube 접근
- **실행 단계**: `codex` 세션을 만들고 그 워크로드 파드를 조회한다
- **기대 결과**: 세션의 보조 파드가 없다. 파드에 컨테이너가 둘(주 컨테이너, `k3s-mcp-proxy`)이고 `automountServiceAccountToken`이 false다. 주 컨테이너의 `K3S_MCP_PROXY_URL`은 `http://127.0.0.1:8093`이고 Secret 참조가 없다. `k3s-mcp-proxy`만 `codex-credentials`의 선택 `k3s-mcp-token`을 읽고, `K3S_MCP_TOOL_POLICY=allow-all`을 받는다(거부 목록 없음). *2026-11-09*: 파드의 PVC는 `codex-chat` 풀의 슬롯 하나이고, 주 컨테이너는 그 `auth`만, 사이드카는 `db`만 마운트한다(`agent-auth-pool.md` AC-N6 — 상세는 `agent-auth-pool.md#시나리오 5`)
- **검증 AC**: AC-J1, AC-J6

### 시나리오 3: 첫 프롬프트가 device code 로그인을 띄우고, 승인하면 턴이 이어진다
- **사전 조건**: 로그인하지 않은 `codex` 세션, ChatGPT 구독 계정(보안 설정에서 device code 로그인 허용)
- **실행 단계**: (a) 프롬프트 하나를 보낸다 → (b) 출력에 나타난 주소를 브라우저로 열어 코드를 넣고 승인한다 → (c) 이어서 두 번째 프롬프트를 보낸다
- **기대 결과**: (a) 출력에 확인 주소와 일회용 코드가 플랫폼 안내 줄로 나타난다. (b) 같은 턴이 실행돼 에이전트 메시지가 출력된다. (c) 로그인 안내 없이 실행되고 앞 대화를 이어 간다
- **검증 AC**: AC-J3, AC-J2

### 시나리오 4: 도구 표면 — 셸·파일·웹·이미지 도구가 모델에 보이지 않는다
- **사전 조건**: SUT와 그 클러스터의 kube 접근
- **실행 단계**: `codex` 세션을 만들고, 그 주 컨테이너 안에서 관리형 설정으로 `codex debug prompt-input`을 렌더한다(세션의 `HOME`·`CODEX_HOME` 그대로)
- **기대 결과**: 모델에 보이는 도구에 `exec_command`·`write_stdin`·`view_image`가 없고 번들 시스템 스킬이 없다. 관리형 `config.toml`에 `skills` MCP 서버가 등록돼 있다
- **검증 AC**: AC-J5

### 시나리오 5: 동결·복원 뒤에도 로그인과 대화가 이어진다
- **사전 조건**: 시나리오 3을 마친 세션, `CODEX_ARCHIVE_ENABLED` on
- **실행 단계**: (a) 세션을 동결한다 → (b) 세션에 접근해 복원한다 → (c) 프롬프트를 보낸다
- **기대 결과**: (a) 파드가 회수된다. (b) 새 파드로 `active`가 되고 출력·커서가 이어진다. (c) 로그인 안내 없이 앞 대화를 이어 답한다
- **검증 AC**: AC-J3, AC-J4

### 시나리오 6: 금지 도구 아이템이 나오면 턴이 멈춘다
- **사전 조건**: 러너에 `command_execution`·`file_change`·`web_search` 아이템을 내는 이벤트 열
- **실행 단계**: 러너가 그 이벤트를 받는다
- **기대 결과**: 안내 한 줄이 출력에 남고 턴이 즉시 중단되며 러너가 0이 아닌 코드로 끝난다
- **검증 AC**: AC-J5

### 시나리오 7: 작업 화면에서 플러그인 스킬을 새로 받는다
- **사전 조건**: `codex` 세션 하나와 `shell` 세션 하나가 있는 SUT를 운영자로 연 브라우저
- **실행 단계**: (a) `codex` 세션의 작업 화면을 연다 → (b) `Refresh plugin skills`를 누른다 → (c) 같은 API를 `shell` 세션에 부른다
- **기대 결과**: (a) Actions 패널에 `Refresh plugin skills` 버튼이 있다(`shell`·다른 에이전트 세션에는 없다). (b) 세션이 `active`로 남고 결과가 알림으로 보이며, 세션 출력에 새로고침 안내 한 줄이 남는다 — 마켓플레이스가 없는 SUT에서는 "새로 받을 마켓플레이스가 없다"는 결과다. (c) 422로 거절된다
- **검증 AC**: AC-J7

> 실 마켓플레이스의 추가·수정·삭제가 같은 세션에 반영되고 다음 턴의 프롬프트에 플랫폼 문구가 붙지 않는 것은 data plane 단위 테스트
> (`codex_plugin_refresh_test.go`·`minimax_marketplace_test.go`)와 운영 프리뷰 확인이 맡는다 — kind SUT 에는 마켓플레이스 토큰이 없다.
>
> 턴 안 새로고침(`refresh_skills`, 2026-11-08)도 같은 이유로 단위 테스트가 맡는다 — `TestCodexPluginRefreshInTurn`(턴당 한 번·턴 밖 409·
> 안내 줄이 `end` 표식 뒤·성공·실패와 무관하게 다음 턴 프롬프트는 사용자의 것 그대로)·`TestFromLoopback`(파드 IP 403)·`TestCodexSkillsMCPRefreshSkills`
> (에이전트 URL이 있을 때만 도구가 보이고 결과·거절을 도구 결과로 돌려줌)·`TestRenderCodexConfig`/`TestCodexEnsureHome`(마켓플레이스가
> 있을 때만 스킬 MCP env에 에이전트 URL·도구 시간 상한). 실 ChatGPT 턴이 필요해 e2e 로 돌리지 않는다.
