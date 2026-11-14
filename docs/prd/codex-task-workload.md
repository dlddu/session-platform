# PRD: 세션 워크로드 — Codex 작업용 (codex-task)

> 대상 요구사항: Codex를 **채팅용**(`codex-chat`, `codex-workload.md`)과 **작업용**(`codex-task`, 이 문서)으로 나눈다 (2026-11-07 사용자 결정).
> 채팅용은 장기 단일 세션이라 세션마다 로그인하고 도구를 대화·스킬·K3s MCP로 좁혔다(*2026-11-09*: 채팅용도 자기 풀 `codex-chat`의 슬롯에 로그인과 세션 DB를 둔다 — `agent-auth-pool.md` AC-N6).
> 작업용은 짧은 작업 세션을 여럿 띄우는 용도라 **claude-code와 같은 권한 수준**(셸·파일 편집·네트워크)으로 돌고,
> 로그인은 세션이 아니라 **인증 풀의 슬롯**(`agent-auth-pool.md`)에 둔다.

> 📌 **2026-11-13 (AC-J8)**: 러너를 같이 쓰므로, 이 타입의 턴이 띄운 하위 에이전트도 답 뒤에 마저 돌고 그 결과가 세션 출력에 남는다(`codex-workload.md` AC-J8, *2026-11-14 개정*: 후속 턴은 없다). 킬 스위치가 없는 것은 그대로다.

> 📌 **2026-11-09 (AC-P1~P3)**: 도구 표면(AC-M1)에 사이드카의 **스케줄러 MCP**(`scheduler` — 한 번·고정 루프·자체 페이싱 예약)가 더해진다. 회차는 이 세션 에이전트의 큐로 들어가고, 예약이 남은 동안은 유휴 회수가 미뤄진다(`session-scheduler.md`).

## 달성 가치
- **V8 목적에 맞는 작업 환경 선택** — 여섯 번째 작업 환경(ChatGPT 구독으로 도는 코딩 에이전트)을 고를 수 있게 하고, 모델·추론 수준을 세션마다 정한다 (AC-M1·M4)
- **V3 끊김 없는 세션 연속성** — 대화·작업 디렉터리는 세션 아카이브에, 로그인은 슬롯에 보존된다 (AC-M2·M3)
- **V1 세션 격리** — 실행 경계는 세션 전용 파드이고, 슬롯은 한 번에 한 세션만 쓴다 (AC-M1, AC-N2)

> 📌 **가치 연결 규칙**: AC-M1(타입 선택)만 V8에 직접 연결하고(AC-E1·AC-J1과 같은 이유), 나머지는 구체화 대상의 가치를 상속한다.

> 이 PRD는 **`codex-chat`의 러너·투영·플러그인 배선을 그대로 쓰고** 세 가지만 바꾼다 — 도구 표면(AC-M1), 로그인 자리(AC-M2), 추론 수준(AC-M4).
> 실행 모델(원샷 턴·직렬 큐·커서·대화 연속성)은 AC-E2~E4, 투영은 AC-J2, 플러그인 스킬 배선과 수동 새로고침은 AC-J5·AC-J7, K3s MCP 사이드카는 AC-J6과 같다.

## Acceptance Criteria

### AC-M1: 워크로드 타입 `codex-task` — claude-code 수준의 도구 표면
- **설명**: `workloadType` 허용값에 `codex-task`가 추가된다(생략·허용값 밖·불변 규칙은 AC-E1). 이미지는 `codex-chat`과 같은 data plane 이미지이고 설정은 `DATA_PLANE_CODEX_TASK_IMAGE`다. 이미지가 비었거나 풀 크기(AC-N1, `CODEX_TASK_AUTH_SLOTS`)가 0이면 `GET /config`에 나오지 않고 생성은 pod 없이 실패한다. 파드는 `codex-chat`과 같은 모양(주 컨테이너 + loopback `k3s-mcp-proxy` 사이드카, 도구 정책 `allow-all`)에 슬롯 PVC를 더한다. 설정은 ConfigMap `codex-task-config`(`CODEX_TASK_CONFIG_CONFIGMAP`: `model`·`models`·`k3s-mcp-url`·`plugin-marketplace-url`, 모두 선택)와 Secret `codex-task-credentials`(`CODEX_TASK_CREDENTIALS_SECRET`: 선택 `k3s-mcp-token`)이며, 다른 에이전트 타입과 이름이 겹치면 control plane이 기동을 거부한다(AC-J6과 같은 이유). 플러그인은 세션이 생성 시 고른 `plugins`만 시드되고, 고르지 않으면 플러그인 없이 뜬다(타입 기본값 없음, 2026-11-12 사용자 결정 — 이전의 `CODEX_TASK_PLUGINS` 설정은 없앴다; `session-plugins.md` AC-L1).

  도구 표면은 claude-code(AC-E2의 구현 결정: 파드가 실행 경계, 코딩 도구 허용)에 맞춘다 — 스레드 설정 샌드박스 `danger-full-access`, 승인 정책 `never`, 샌드박스 네트워크 켜짐, 웹 검색 `disabled`(*2026-11-13*: 러너가 `codex app-server`를 구동하며 SDK 옵션 이름에서 Codex 설정 이름으로 옮겨 적었다, `codex-workload.md` AC-J2). 관리형 `config.toml`은 셸(`shell_tool`·`unified_exec`)과 파일 편집을 **켜고**, 앱·Codex 자체 플러그인·원격 플러그인·브라우저·컴퓨터 사용·이미지 생성·메모리·훅·도구 제안·스킬 MCP 자동 설치·번들 시스템 스킬은 `codex-chat`과 같이 끈다. 러너의 킬 스위치(AC-J5)는 이 타입에서 돌지 않는다. 셸이 있으므로 플러그인 스킬은 Codex가 `$HOME/.agents/skills`에서 직접 읽는다(스킬 MCP도 그대로 등록한다). 워크로드 파드는 claude-code처럼 data plane SA(`view`)의 토큰을 받는다(`automountServiceAccountToken: true`).
- **달성 가치**: V8, V1
- **구체화 대상**: AC-A1의 "실제 세션 워크로드" — 여섯 갈래 (… AC-J2~J7=`codex-chat`, AC-M2~M4=`codex-task`)
- **검증 방법**: `workloadType=codex-task`로 생성하면 컨테이너 둘의 파드 하나가 슬롯 PVC를 마운트한 채 뜨고, 주 컨테이너에 Secret 참조가 없으며 SA 토큰이 마운트됨을 확인한다. 관리형 설정으로 렌더한 모델 도구 목록에 셸 도구가 있고 번들 스킬이 없음을 확인한다(이미지 빌드 검사). 러너가 이 모드에서 `command_execution` 아이템으로 턴을 끊지 않음을 확인한다.

### AC-M2: 로그인은 슬롯의 `auth.json`, 빈 슬롯은 첫 프롬프트에서 device code
- **설명**: 이 타입은 풀 `codex-task`(AC-N1~N5)를 쓴다. 슬롯 파일은 `auth.json`이고 HOME 쪽 자리는 `$HOME/.codex/auth.json`이다(AC-N4가 동기화). 로그인 설정은 AC-J3과 같다 — `forced_login_method = "chatgpt"`, `cli_auth_credentials_store = "file"`, 주 컨테이너의 Codex·OpenAI 자격 증명 환경 변수는 기동 거부. 슬롯이 비어 있으면 첫 프롬프트에서 러너가 AC-J3의 device code 로그인을 돌리고 같은 턴을 이어서 실행한다. 턴 끝의 동기화가 그 로그인을 슬롯에 남기므로, 같은 슬롯을 받는 다음 세션은 로그인하지 않는다.
- **달성 가치**: V3
- **구체화 대상**: AC-J3(로그인)을 「세션 HOME」에서 「풀 슬롯」으로 옮긴 것, AC-N4
- **검증 방법**: 빈 슬롯을 받은 세션의 첫 write 출력에 확인 주소와 코드가 안내 줄로 나타남을 확인한다. 슬롯에 `auth.json`이 있으면 첫 write가 로그인 안내 없이 실행됨을 확인한다(에이전트 단위 테스트 — 실 로그인은 e2e 밖).

### AC-M3: 파일시스템 아카이브, 로그인 제외, 전용 게이트
- **설명**: 동결·복원은 AC-J4와 같은 파일시스템 아카이브다 — 작업 디렉터리·HOME(스레드 기록)·누적 출력. 단 `$HOME/.codex/auth.json`은 아카이브에 넣지 않는다(AC-N4). 외부 스토리지 전송 게이트는 `CODEX_TASK_ARCHIVE_ENABLED`(기본 off)다. 이 타입의 아카이브에는 로그인이 없지만, 셸이 만든 작업 디렉터리 파일이 들어가므로 claude-code와 같이 따로 켠다. 동결이 커밋되면 슬롯을 반납하고(AC-N3), 복원은 새로 임대한 슬롯으로 뜬다.
- **달성 가치**: V2, V3
- **구체화 대상**: AC-B1·B2·B3, AC-N3
- **검증 방법**: 게이트 on에서 동결→복원 후 대화·출력·커서가 보존되고 아카이브에 `auth.json`이 없음을 확인한다. 게이트 off에서 동결이 거부되고 파드·슬롯이 남음을 확인한다.

### AC-M4: 모델과 추론 수준을 세션마다 정한다
- **설명**: 생성 요청의 `model`은 AC-E6의 계약을 따른다(생략은 `platform-default` → ConfigMap `codex-task-config`의 선택 `model` → 플랜 기본). 생성 요청의 선택 필드 `reasoningEffort`는 이 타입에서 `minimal`·`low`·`medium`·`high`·`xhigh` 중 하나다. 생략하면 세션에 저장되지 않고 Codex 설정·모델의 기본값을 쓴다. 값은 모델처럼 **생성 시 고정**이고 동결·복원을 건너 유지된다. 파드는 `CODEX_REASONING_EFFORT`로 받고 러너는 `--effort <값>`으로 스레드 옵션 `modelReasoningEffort`를 준다(재개 턴도 같은 값). 허용값 밖·빈 문자열·비문자열은 pod 생성 전 400이다. `GET /config`의 타입 항목은 `reasoningEfforts`(허용값 배열, 이 타입은 위 다섯)를 내놓는다. 계약의 공통 부분(필드 이름·저장·불변·`/config` 모양·다른 타입의 거부)은 AC-E8이다.
- **달성 가치**: V8
- **구체화 대상**: AC-E8(세션 추론 수준)의 `codex-task` 값
- **검증 방법**: `reasoningEffort=high`로 생성한 세션의 조회 응답과 파드 env에 그 값이 있고 러너 argv에 `--effort high`가 있음을 확인한다. `max`·`""`는 400이고 파드가 없음을 확인한다. `GET /config`의 `codex-task` 항목이 다섯 값을 내놓음을 확인한다.

## 결정 사항 (2026-11-07, 사용자)
- **이름**: 채팅용은 `codex-chat`, 작업용은 `codex-task`. 옛 이름 `codex`는 `codex-chat`의 별칭으로 계속 받는다(`codex-workload.md` AC-J1).
- **권한 수준**: claude-code와 비슷하게 — 셸·파일·네트워크. 웹 검색은 claude-code의 허용 도구에 없으므로 끈다.
- **로그인**: 세션이 아니라 풀 슬롯(PVC)에. 슬롯 파일은 세션 컨테이너가 읽을 수 있다(AC-N5의 대가).

## 남은 일
- 세션 MCP(파일·git push·drop)는 주지 않는다. 셸로 git을 쓰지만 push 자격 증명은 없다 — 필요하면 claude-code의 repo-access 배선을 옮기는 후속.
