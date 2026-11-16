# pretooluse-probe — AC-E2 가 아직 증명하지 못한 순서를 실측하는 격리 하네스

「턴 중 도구 승인」(AC-E2)은 러너의 `canUseTool` 하나에 걸려 있다. 그 경로에는 **증명되지 않은
전제**가 둘 있다.

1. `permissionMode: "auto"` 의 **분류기가 스스로 거절한 호출은 `canUseTool` 에 오지 않는다** —
   그래서 그 호출은 소유자에게도 가지 않는다. (현 구현의 제한)
2. SDK 계약은 PreToolUse 훅이 `permissionDecision: "ask"` 를 돌려주면 **「ask 경로가
   `can_use_tool` 제어요청으로 나타난다」** 고 적고, **「PreToolUse 의 deny 는 `canUseTool` 을
   건너뛴다」** 고 적는다(`@anthropic-ai/claude-agent-sdk/sdk.d.ts` 의 `SDKPermissionDeniedMessage`
   주석). 그러나 **분류기와 훅의 전후**는 어디에도 적혀 있지 않다.

순서 판정은 SDK 가 아니라 CLI 안에 있어(SDK 는 훅을 `initialize` 에 등록만 하고 CLI 가
`hook_callback` 으로 되부른다) 정적 분석으로는 답이 나오지 않는다. 이 하네스는 그 순서를
**실제 실행으로** 재어 기록한다.

이 디렉터리는 제품 코드가 아니다 — 러너도 제품 권한 모드도 바꾸지 않는다.

## 무엇을 돌리는가

격리된 임시 cwd(`mkdtemp`)에 무해한 파일 하나를 두고, 모델에게 **`Read` 한 번**을 시킨다.
네 케이스를 **한 번에 하나씩 순차로** 돈다.

| 케이스 | 훅 | 하네스의 결정 | 묻는 것 |
|---|---|---|---|
| `baseline-auto` | 없음 | allow | 지금 제품과 같은 배선에서 `canUseTool` 이 오는가, 분류기 거절(`permission_denied` / `decision_reason_type="classifier"`)이 보이는가 |
| `hook-ask-allow` | `ask` | allow | `ask` 가 `canUseTool` 에 닿는가, 훅이 `canUseTool` 보다 **먼저**인가 |
| `hook-ask-deny` | `ask` | deny | 같은 경로에서 거절도 턴에 전달되는가 |
| `hook-deny` | `deny` | (무관) | **hard-deny 보존** — 훅의 거절이 `canUseTool` 을 건너뛰는가(0회) |

「하네스의 결정」은 **격리된 가짜 작업에 대한 이 스크립트 자신의 결정**이다. 사람의 승인을
대신하거나 위조하지 않으며, 실제 사용자 승인 경로를 흉내 내지도 않는다.

## 기록하는 것 / 하지 않는 것

기록: 이벤트 종류 · 시작 기준 상대 시각(ms) · 횟수, 그리고 화이트리스트한 칸
(`subtype`·`tool`·`decisionReasonType`·`hookEvent`·`decision`·`behavior`·`isError`)뿐이다.

기록하지 않음: 모델이 쓴 문장, 도구 인자, 도구 결과 본문, 환경 변수, 자격 증명. 원문 스트림은
어디에도 저장하지 않는다. (`recorder-selftest.mjs` 가 합성 비밀 문자열로 이 성질을 검사한다.)

## 전제: 정식 인증

이 하네스는 **정식 로그인/지원되는 환경 인증만** 쓴다. 다른 프로세스의 환경(`/proc/*/environ`)을
읽지 않고, 토큰 파일을 만들지 않는다. 인증이 없으면 아무것도 실행하지 않고 **코드 3** 으로 끝난다.

받아들이는 인증(앞의 것이 우선):

- `CLAUDE_CODE_OAUTH_TOKEN` — 구독 토큰. `claude setup-token` 으로 발급한다(구독 필요).
- `ANTHROPIC_API_KEY` — API 키.
- `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` — 게이트웨이 배치.
- `PROBE_ASSUME_CLI_LOGIN=true` — `claude auth login` 으로 **이미 로그인된** 환경에서, 환경
  변수 없이 CLI 자신의 자격으로 돌 때만 쓴다(그 선언을 기록에 남긴다).

확인 명령(읽기 전용): `claude auth status` · `claude auth --help` · `claude setup-token --help`

## 실행

```sh
cd scripts/experiments/pretooluse-probe
node recorder-selftest.mjs                  # 기록·판정 로직만 — SDK 미실행, 인증 불필요
node probe.mjs > /tmp/pretooluse-probe.json # 실제 실행 (인증 필요)
```

선택 환경 변수: `PROBE_SDK_PATH`(기본 `/opt/claude-runner/node_modules/...` → 없으면 bare import),
`PROBE_CLAUDE_BIN`(기본 `/usr/local/bin/claude`), `PROBE_CASE_TIMEOUT_MS`(기본 120000).

각 케이스는 `AbortController` 로 상한이 걸리고, `maxTurns: 3`·`tools: ["Read"]` 이며 **외부 MCP
서버를 붙이지 않는다**(`mcpServers: {}`). `settingSources: []` 라 이 레포·사용자 설정을 읽지 않는다.
`maxTurns` 가 1 이면 도구 호출 하나를 끝내지 못하고 `error_max_turns` 로 끝난다(2026-11-16 실측).
`canUseTool` 은 인자를 그대로 돌려주고(`updatedInput: input`), 대상이 그 격리 파일인지 확인하며,
케이스당 한 번만 결정한다.

출력은 JSON 하나다: `versions`(SDK·CLI·node·인증 출처)와 케이스별 `events`·`counts`·`verdict`.

## 판정 기준

- **`hook-deny` 의 `hardDenyPreserved === true`** — 훅이 불렸는데 `canUseTool` 이 **0회**.
  거짓이면 명시적 deny 보존이 훅 경로에서 깨진다는 뜻이므로, 그 자체로 **채택 불가** 신호다.
- **`hook-ask-allow` / `hook-ask-deny` 의 `askReachedCanUseTool === true`**, 그리고
  **`hookBeforeCanUseTool === true`** — `ask` 가 실제로 `canUseTool` 로 오고 훅이 앞선다.
  이 둘이 참이어야 「사용자 결정 경로로 연결 가능」이 **실측으로** 성립한다.
- **`baseline-auto` 의 `preemptionProvable`** — 무해한 `Read` 호출로는 분류기가 거절할 이유가
  없으므로 이 값은 **`false`** 다(2026-11-16 실측이 그렇게 나왔다). 그때 결과 JSON 의 `note` 가
  그대로 말한다:
  *「BASELINE DID NOT REPRODUCE A CLASSIFIER DENY — preemption over the classifier is NOT
  provable from this run」*. 즉 **「PreToolUse ask 가 분류기의 자동 거절을 선점한다」는 이 실험만으로
  증명되지 않는다.** 그 명제를 보려면 분류기가 실제로 거절하는 호출이 필요한데, 그런 호출을
  만드는 것은 이 하네스의 무해성 제약 밖이다 — 별도 승인과 설계가 필요하다.

## 2026-11-16 실측 (SDK 0.3.220 / CLI 2.1.220, 로그인된 별도 셸)

| 케이스 | hook | canUseTool(callback) | 결과 |
|---|---:|---:|---|
| `baseline-auto` | — | **0** | Read 성공 1 · 분류기 거절 **0** |
| `hook-ask-allow` | 1 | **1** | Read 성공 1 |
| `hook-ask-deny` | 1 | **1** | tool_result error 1 |
| `hook-deny` | 1 | **0** | tool_result error 1 |

네 케이스 모두 `failure: null` · `result: success`. 원본은 셸 쪽에 보존돼 있다
(`result.json`, 그리고 `maxTurns: 1` 로 `error_max_turns` 가 났던 첫 시도의 `result-maxturns1.json`).

읽히는 것 셋:

1. **`ask` 는 실제로 `canUseTool` 로 온다.** baseline 에서 0 이던 콜백이 `ask` 를 돌려준
   케이스에서 1 이 됐다 — 같은 무해한 호출인데 훅이 경로를 바꾼 것이다.
2. **`deny` 는 `canUseTool` 을 건너뛴다.** 훅 1 / 콜백 **0** 이고 호출은 거부됐다.
   계약(sdk.d.ts)의 「PreToolUse hook denies bypass canUseTool」이 실측과 같다.
3. **분류기 거절은 재현되지 않았다**(baseline 의 `classifierDenies` 0). 그래서
   **「PreToolUse ask 가 auto 분류기의 자동 거절을 선점한다」는 여전히 미증명**이다.
   이 한계는 지우지 말 것 — 선점은 이 실험이 보여 준 적이 없다.

> ⚠️ 이 실측은 **설정 원천이 없는 환경**(`settingSources: []`)의 것이다. 명시적 deny 규칙이
> 있는 환경에서 훅의 `ask` 가 어떻게 되는지는 **아직 재지 않았다** — 아래 「남은 최소 실험」.

## 2026-11-16 (2) 실측 — 명시적 deny 보존 (같은 SDK/CLI, 로그인된 셸)

제품에 훅을 잇기 전에 답해야 했던 질문: **설정의 `permissions.deny` 가 막는 호출을 훅의 `ask` 가
되살려 사람 앞에 세울 수 있는가?** 네 변형을 돌렸다.

| 변형 | 규칙 | 결과 | 채택 |
|---|---|---|---|
| (A) | `options.settings` 의 `deny: ["Read"]` (도구 전체) | `tool_use` 0 · 훅 0 · 콜백 0 | ❌ 도구 자체가 사라진 것이라 **우선순위의 증거가 아니다** |
| (B) | `Read(/tmp/… )` 단일 슬래시 절대경로형 | baseline 이 **성공** | ❌ 규칙이 매칭되지 않았다 — 무효 |
| (C) | `options.settings` 의 `deny: ["Read(./probe-input.txt)"]` | baseline·ask 모두 Read 요청 1 → `tool_result` error 1, **훅 0 · 콜백 0** | ✅ |
| (D) | 임시 cwd `.claude/settings.json` 같은 규칙 + `settingSources: ["project"]` | 네 케이스 모두 Read 요청 1 → `tool_result` error 1, **훅 0 · 콜백 0**, `result success` | ✅ |

같은 파일에 규칙이 **없는** 대조군은 baseline·ask 모두 Read 성공 1이었다. 그래서 차단은 그 규칙
때문이고, **이 버전의 `Read(파일)` 규칙에서 deny 는 훅보다 앞선다** — 훅이 불리기도 전에 호출이
끝난다. 원본은 셸 쪽 `result-projectdeny.json` · `result-pathdeny-relative.json` 에 있다.

### 이 결과로 말하지 않는 것
- **`decision_reason_type="rule"` 을 직접 관측하지 못했다.** 이 Read 규칙 경로에서는 SDK 의
  `permission_denied` 시스템 메시지가 나오지 않았다. 거부는 `tool_result` error 로만 보였다.
- (A) 의 도구 전체 금지는 **우선순위가 아니라 도구 제거**다.
- 관리형(managed) 설정·다른 도구·`auto` 분류기 자동 거절로 **일반화하지 않는다**.
- 하네스의 `hook-deny` 판정값 `hardDenyPreserved` 는 「훅이 불렸고 콜백이 0」을 보는 것이라,
  **훅조차 불리지 않는 이 deny 규칙 경로에는 적용되지 않는다**. 그 경로는 `rule-before-hook`
  이라는 **별도 판정**으로 읽어야 하고, `hardDenyPreserved === false` 를 위반으로 읽으면 안 된다.

### 그래서 제품에 무엇이 들어갔나
`toolApprovals` 를 고른 턴에만 `PreToolUse` 훅 하나가 붙고, 그 훅은 **`ask` 외에는 말하지 않는다**
(`lib.mjs` 의 `approvalHookDecision`). 정책이 허용하는 호출만 `ask` 로 끌어오고, 정책이 거부하는
것·정책이 스스로 정하는 것(`AskUserQuestion`·플랜 모드)은 `null` 이라 훅이 아무 판정도 싣지 않는다 —
거부는 전과 같이 `canUseTool` 에서 일어나고 출력에 한 줄을 남긴다. 승인을 고르지 않은 턴의 SDK
옵션에는 `hooks` 키 자체가 없다.

## 2026-11-16 (3) 실측 — 제품 러너 통합 (비운영 loopback 승인 데스크)

위 둘이 SDK 의 성질을 잰 것이라면, 이것은 **제품 러너 자체**를 돌린 것이다. 원격
`d269365` 를 clone 해 `data-plane/claude-runner/runner.mjs` 를 **고치지 않고**(설치된 SDK
`node_modules` 심링크만 걸었다) 실행하고, `--approvals-url` 이 가리키는 자리에 테스트 대역
승인 데스크를 loopback 으로 띄웠다. 네 케이스 모두 **exit 0 · 최종 `success` · stderr 없음**.

| 케이스 | 데스크가 받은 요청 | 턴의 결과 |
|---|---|---|
| allow | `POST /approvals/ask` 1 · `GET` 1 | `tool_result` `is_error=false` 1 |
| deny | `POST` 1 · `GET` 1 | `tool_result` `is_error=true` 1 |
| timeout (`CLAUDE_RUNNER_APPROVAL_WAIT_MS=1000`) | `POST` 1 · `GET` 2 · **`DELETE` 1** | `tool_result` `is_error=true` 1 |
| noapproval (`--approvals-url` 없음) | **0건** | Read 성공 1 |

읽히는 것: 승인을 고른 턴만 데스크를 부르고(마지막 행이 0건), 올리기·조회·철회가 설계대로
한 번씩 돌며, 결정이 그 턴 안에서 도구 결과가 된다. 러너의 대기 상한이 지나면 러너가 스스로
요청을 **철회**(`DELETE`)하고 거부로 닫는다.

안전 범위: 격리 임시 cwd 의 무해한 파일 하나, 프로젝트 설정 deny 로 `Bash`·`Write`·`Edit`·
`Agent`·`Task`·`WebFetch`·`WebSearch`·`NotebookEdit` 를 막았고, 데스크는 **그 정확한 Read/파일만**
승인하며 최대 1건만 허용했다. JSON 원문과 토큰은 기록하지 않았다. 원본은 셸 쪽
`runner-integration.py` · `runner-integration-result.json` 에 있다.

> ⚠️ **데스크는 테스트 대역이다.** 이 실측은 러너 ↔ 승인 경로의 왕복을 보인 것이고, 운영
> control-plane·세션 MCP·공개 API 를 지나는 전체 e2e 가 아니다. 그쪽으로 넓혀 읽지 말 것.

## 이 하네스가 답하지 못하는 것

- 분류기 vs 훅의 **선점 관계**(위 실측에서 분류기 거절이 재현되지 않았다).
- **관리형 설정·다른 도구에서의 deny 우선순위** — 위 (2) 실측은 `Read(파일)` 규칙 하나의 범위다.
- 운영 전체 경로. (3) 이 제품 러너를 돌렸지만 승인 데스크는 테스트 대역이고, 운영
  control-plane·세션 MCP·공개 API 는 지나지 않았다.
- 승인 만료·중복 요청 재전달 같은 데스크 쪽 성질 — 그것은 이미
  `data-plane/cmd/agent/claude_approvals_test.go` 가 덮는다.
