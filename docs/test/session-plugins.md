# 테스트 문서: 세션별 플러그인 선택

## 검증 대상 AC
- AC-L1: 생성 시 플러그인 선택 — 고른 것만 받고, 세션 수명 동안 고정 (PRD: 세션별 플러그인 선택)

> 📌 **SUT 설정**: kind e2e는 어느 타입에도 K3s MCP 토큰을 주지 않아 시더가 마켓플레이스를 받지 않는다(2026-11-13 인클러스터 마켓플레이스 철거).
> 이름이 마켓플레이스에 있는지는 AC-L1이 control plane의 검사 대상에서 뺐으므로, 두 시나리오 모두 마켓플레이스 없이 돈다 — 시나리오 1은 선택이 **실리는 자리**를 보고, 설치 결과는 보지 않는다.

## 테스트 시나리오

### 시나리오 1: 고른 플러그인이 그 타입의 설치 지점에 실리고, 동결·복원을 건너 유지된다
- **사전 조건**: `claude-code`·`codex-chat`·`codex-task`·`minimax-code` 이미지가 있는 SUT
- **실행 단계**: (a) 세 타입 각각에 `plugins`를 주어 세션 생성 → (b) 같은 타입을 `plugins` 없이 생성 → (c) (a)의 `claude-code` 세션을 동결한 뒤 switch로 복원 → (d) `GET /config`
- **기대 결과**: (a) `201`, 응답 `plugins`가 요청과 같은 순서의 같은 목록. `claude-code`는 워크로드 컨테이너의 `CLAUDE_CODE_PLUGINS`와 세션 파드 `k3s-mcp-proxy`의 `PLUGIN_SEED_PLUGINS`, `codex-chat`·`codex-task`는 세션 파드 `k3s-mcp-proxy`의 `PLUGIN_SEED_PLUGINS`, `minimax-code`는 헬퍼 파드 `k3s-mcp-proxy`의 `PLUGIN_SEED_PLUGINS`가 그 목록이고, 그 밖의 컨테이너에는 없다. `claude-code` 세션의 관리형 설정은 목록의 플러그인 가운데 사이드카 시드로 설치된 것(`<이름>@dlddu-plugins`)만 켠다 — 토큰 없는 SUT에서는 하나도 켜지지 않는다. (b) 응답에 `plugins`가 없다. `claude-code`는 `CLAUDE_CODE_PLUGINS`가 없고 사이드카 `PLUGIN_SEED_PLUGINS`가 비어 있으며, `minimax-code`는 변수가 없고 `codex-chat`·`codex-task`는 비어 있다 — 어느 타입이든 플러그인 없이 뜬다(`codex-chat`은 2026-11-14부터, AC-A5). (c) 복원된 세션의 `plugins`가 같고 복원 pod에도 같은 목록이 실린다. (d) `takesPlugins`가 `claude-code`·`codex-chat`·`codex-task`·`minimax-code`만 `true`
- **검증 AC**: AC-L1

### 시나리오 2: 받을 수 없는 선택은 pod 없이 400이다
- **사전 조건**: 아무 SUT
- **실행 단계**: 다음 본문으로 각각 세션 생성 — `plugins: null` · `plugins: []` · `plugins: "development"` · 중복 · `a/b`·`dev@other`·`,`가 든 이름 · 17개 · `shell`(타입 생략 포함)에 목록
- **기대 결과**: 전부 `400`이고 세션 목록과 pod 수가 변하지 않는다
- **검증 AC**: AC-L1
