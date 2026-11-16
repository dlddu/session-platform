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

## 남은 최소 실험 — 명시적 deny 보존 (제품 연결의 전제)

제품(`toolApprovals` opt-in)에 PreToolUse `ask` 를 잇기 전에 **반드시** 답해야 하는 질문:
**설정의 `permissions.deny` 가 막는 호출을 훅의 `ask` 가 되살려 사람 앞에 세울 수 있는가?**
그럴 수 있다면 승인 경로가 명시적 deny 를 우회하는 길이 되므로, 그 전에는 잇지 않는다.

SDK 계약에도 CLI 문자열에도 **훅 결정과 deny 규칙의 우선순위는 적혀 있지 않다**
(`decision_reason_type` 이 `'rule' | 'hook' | 'classifier' | …` 로 **병렬 열거**될 뿐이다).
그래서 실측해야 한다.

비운영·무해하게 재는 법(로그인된 셸에서, 이 하네스를 조금 고쳐서):

1. 격리 cwd 에 `.claude/settings.json` 을 쓴다 —
   `{"permissions":{"deny":["Read(./probe-input.txt)"]}}` (그 파일 하나만 막는다).
2. `settingSources: ["project"]` 로 바꾼다. 나머지는 그대로.
3. 두 케이스를 돌린다:
   - **`deny-rule-baseline`** (훅 없음): 호출이 거부되는지, `permission_denied` 의
     `decision_reason_type` 이 `"rule"` 인지 확인한다. 이 선행이 성립해야 2단계가 의미를 갖는다.
   - **`deny-rule-vs-ask`** (훅이 `ask`): `canUseTool` 이 **불리는지**를 본다.
4. 판정:
   - `canUseTool` **0회** → deny 규칙이 훅보다 세다. **명시적 deny 보존 확인** → 제품 연결 가능.
   - `canUseTool` **1회** → 훅의 `ask` 가 deny 규칙을 뚫고 사람에게 묻는다.
     **제품 연결 불가** — 승인 경로가 명시적 deny 의 우회로가 된다.
   - 1 단계가 거부를 재현하지 못하면(규칙 문법 불일치 등) **아무 결론도 내리지 않는다**.

`.claude/settings.json` 은 임시 cwd 안에만 쓰고, 끝나면 cwd 째로 지운다. 사용자·운영 설정은
건드리지 않는다.

## 이 하네스가 답하지 못하는 것

- 분류기 vs 훅의 **선점 관계**(위 실측에서 분류기 거절이 재현되지 않았다).
- **명시적 deny 규칙 vs 훅의 `ask`** — 위 「남은 최소 실험」이 답한다. 그 전까지 제품 연결은
  막혀 있다.
- 운영 러너에서의 동작. 여기서는 `settingSources: []` 이고 플러그인·K3s MCP 가 없다.
- 승인 만료·중복 요청 재전달 같은 데스크 쪽 성질 — 그것은 이미
  `data-plane/cmd/agent/claude_approvals_test.go` 가 덮는다.
