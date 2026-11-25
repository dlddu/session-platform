# 판정 패스 — codex 워크로드·멀티테넌시 축 (2026-11-25)

범위(L 표면, 18행 · **398줄 판정, 제거 118 · 유지 280**): PR #203이 세운 codex 워크로드(AC-J1~J8·
AC-M1~M4)와 멀티테넌시(AC-K1~K5) 축 중 **control-plane 13행 187줄 + data-plane cmd/agent 5행 211줄**.
모델 정의의 「판정 슬라이스」(예산 400줄)를 한 덩어리로 가져간 것이다 — 누적 398줄에서 다음 덩어리
`data-plane/cmd/agent/codex_test.go`(36줄)를 더하면 434줄이 되어 예산을 넘으므로 여기서 멈췄다.

열린 PR #213(`codex-task-web-search`)이 점유한 파일 — `data-plane/cmd/agent/codex_config.go`·
`data-plane/codex-runner/lib.mjs`·`runner.test.mjs`·`surface-check.mjs`·`surface.mjs` — 은
「파일이 실제로 겹치는 열린 PR이 있을 때만 그 덩어리를 뺀다」에 따라 범위 밖이다. PR #201
(`reconcile/tbm_session-platform-comment-necessity-rct_20261104-0003-minimax-k3s-proxy-slice`, 미머지)의
20파일과는 0겹이다.

판정은 필요성 시험으로 한다 — 각 주석에 「지우면 이 코드를 고치러 온 사람이 틀린 판단을 하거나 확인에 큰 비용을
치르는가」를 묻는다. 코드를 바꾸지 않았다(주석 삭제와 doc 주석 문장 축소뿐).

## 제거 — 118줄

좌표는 「이 내용을 이미 말하는 곳」이다. AC 번호는 `docs/prd/codex-workload.md`·`docs/prd/codex-task-workload.md`·
`docs/prd/multitenancy.md`·`docs/prd/agent-auth-pool.md`의 AC, 그 밖의 AC는 해당 PRD.

| 파일 | 제거 | 줄 | 좌표 |
| --- | --- | ---: | --- |
| `control-plane/cmd/control-plane/codex.go` | `codexConfig` doc, `image` 필드 doc, `offered` doc, `codexArchiveOption` doc | 8 | 타입·필드·함수 이름과 바로 아래 코드, AC-J1·AC-J4~J6 |
| `control-plane/cmd/control-plane/codex_test.go` | `TestOfferedWorkloadTypesIncludesCodexOnlyWithImageAndSlot` 머리 | 1 | 테스트 이름·단언, AC-J1·AC-N6 |
| `control-plane/cmd/control-plane/tenancy.go` | `tenancyConfig` doc | 2 | 타입·필드 이름, AC-K1·AC-K3 (**주석이 전부 사라져 원장 행 삭제**) |
| `control-plane/internal/adapter/k8s/codex.go` | `CodexSchedulerMCPURL`·`K3SMCPProxyLoopbackURL` doc, `WithCodexCredentialsSecret`·`WithCodexConfigConfigMap`·`WithCodexTaskCredentialsSecret`·`WithCodexTaskConfigConfigMap` doc | 9 | 상수·함수 이름과 시그니처, AC-J1·AC-J6·AC-M1 |
| `control-plane/internal/api/tenancy_test.go` | 테스트 머리 7건 | 11 | 각 `Test*` 이름·단언, AC-K1~AC-K4·AC-J1 (`docs/prd/multitenancy.md`) |
| `control-plane/internal/session/codex.go` | `SteerStatusApplied` doc, `Applied` doc | 2 | 상수·메서드 이름과 본문, AC-J9 |
| `control-plane/internal/tenancy/resolver.go` | `Resolver` doc, `TokenReviewResolver` doc, `NewTokenReviewResolver` doc, `StaticResolver` doc, `Resolve` doc | 7 | 타입·함수 이름과 시그니처, AC-K1·AC-K4 |
| `control-plane/internal/tenancy/tenancy.go` | `Tenant`·`Allows`·`Config`·`accountPattern`·`Tenants`·`Lookup`·`ForServiceAccount`·`Caller`·`OperatorCaller`·`Sees`·`ErrNoTenant` doc | 13 | 각 식별자 이름과 바로 아래 코드, AC-K2~AC-K4 |
| `control-plane/internal/tenancy/tenancy_test.go` | `TestParseRejects` 머리 | 1 | 테스트 이름과 표의 각 케이스, AC-J1 |
| `control-plane/test/codex_orchestrator_test.go` | 파일 머리, `TestCodexChat_RefusesWithoutSlot` 머리, `optionalConfigMapRef` doc | 6 | 테스트 이름·단언·헬퍼 본문, AC-J1·AC-J3·AC-J6·AC-N6 |
| `control-plane/test/e2e_codex_2_pod_shape_test.go` | 슬라이스 머리 | 2 | 테스트 이름과 단언, AC-N6 (**주석이 전부 사라져 원장 행 삭제**) |
| `control-plane/test/harness_tenancy_test.go` | `tenantAlpha`·`tenantBeta` 등 상수 그룹 머리 | 1 | 상수 이름과 `deploy/overlays/e2e/e2e-tenancy.yaml` |
| `data-plane/cmd/agent/codex.go` | 파일 머리 15줄, `codexSkillsDirEnv`·`codexAgentURLEnv`·`codexSessionDBServerName`·`codexSchedulerServerName`·`codexAuthFile`·`codexAuthHomeRel`·`codexReasoningEfforts` doc, `task`·`effort`·`k3sProxyURL`·`agentBinary`·`sessionDBURL`·`schedulerURL`·`refreshSidecar` 필드 doc, effort 검증 주석, `runAgentSubcommand` doc, `loopbackAgentURL` doc | 39 | 상수·필드·함수 이름과 바로 아래 코드, AC-J1~J9·AC-M1~M4·AC-N6·AC-P1 |
| `data-plane/cmd/agent/codex_skills.go` | `codexSidecar` doc, `configured`·`revision` 필드 doc, `codexSkillMatches` doc | 5 | 타입·필드·함수 이름과 본문, AC-J5·AC-J7 |
| `data-plane/cmd/agent/codex_skills_mcp.go` | `maxCodexSkillsMCPLine` doc, `client` 필드 doc, `refreshSkills` doc | 4 | 상수·필드·함수 이름과 본문, AC-J5·AC-J7 |
| `data-plane/cmd/agent/codex_skills_mcp_test.go` | 테스트 머리 2건 | 4 | 테스트 이름·단언, AC-J5·AC-J7 (**주석이 전부 사라져 원장 행 삭제**) |
| `data-plane/cmd/agent/codex_stream.go` | `discarding`·`steer` 필드 doc, `jsonString` doc | 3 | 필드·함수 이름과 본문, AC-J2·AC-J9 |

## 유지 — 280줄 (사유)

| 대상 | 줄 | 필요 사유 |
| --- | ---: | --- |
| `Keep in sync with control-plane/internal/adapter/k8s (…)` 5건 (`control-plane/internal/adapter/k8s/codex.go` 4 · `data-plane/cmd/agent/codex.go` 1) | 9 | 두 모듈이 같은 env 이름·값을 따로 선언하므로 한쪽만 바꾸면 파드가 조용히 어긋난다 — 어느 파일이 정본인지 코드만으로는 드러나지 않는다 |
| 보안 불변식 — `validateCodexConfig`·`codexProfile`·`loopbackK3SMCPProxy`·`validateCodexClientEnv`·`validateLoopbackSidecarEndpoint`·`codexCredentialEnv`·`sessionDBMCPURL`·`codexK3SMCPProxy` doc | 30 | 지우면 다른 타입의 K3s 토큰·모델·자격증명이 워크로드 파드로 새는 방향의 수정을 막는 근거가 사라진다 — 토큰 보유자가 파드 밖으로 옮겨질 위험은 코드 형태에서 드러나지 않는다 |
| 테넌시 경계 — `DefaultAudience`·`Resolve`·`Parse`·`AnyWorkloadType`·`Grants are stored normalized…` doc과 `Grants are stored normalized`·`An omitted type is still an error` 주석 | 18 | 「ambient ServiceAccount 토큰이 테넌트 자격증명으로 두 번 쓰이면 안 된다」·「비어 있는 grant는 `shell`로 접힌다」·「망가진 grant는 조용히 넓어지지 않고 기동을 멈춘다」는 세 규칙은 코드만 읽으면 반대로 읽힌다 |
| 외부 시스템의 문서화되지 않은 동작 — `codexSkillNameSeparator` 머리의 스킬 경로 규칙(`$HOME/.agents/skills/<name>/SKILL.md`, `<plugin>--<skill>`), `codexPluginSkills`·`codexSkillsFromSeed`·`fetchCodexSidecar` doc | 24 | Codex CLI가 스킬을 어디서 찾고 어떤 이름으로 부르는지, marketplace 시드가 어떤 형식인지 — 레포 안의 다른 곳이 말하지 않는다 |
| 파일·심볼 경계 규칙 — `readCodexSkillFile`·`openRoot`·`listCodexSkillFiles`·`resolveCodexSkillName`·`skillDescription`·`serveCodexSkillsMCP` doc, `maxCodexSkillsMCPLine`·`codexSkillsRefreshTimeout` doc | 27 | `os.Root`가 이미 막는 것과 이 코드가 추가로 막는 것의 경계, MCP stdio 전송 규약(프레임당 한 줄), 타임아웃 산정 근거 — 지우면 심볼릭 링크로 다른 스킬 파일을 읽는 수정의 근거가 없어진다 |
| 스킬 동기화의 소유권 — `syncCodexSkills`·`installCodexSkill`·`codexSkillChanges`·`codexSkill` doc과 마커·HOME 보관 규칙 | 20 | 「태그 없는 디렉터리는 건드리지 않는다」·「마커를 먼저 써서 동기화가 끊겨도 다음 sync가 그 디렉터리를 소유한다」는 규칙은 코드만으로는 보이지 않는다 — 손으로 넣은 스킬이 지워지는 수정의 근거가 사라진다 |
| 러너·프로젝터 계약 — `argv`·`validateState`·`ensureHome`·`codexMaxRecordBytes`·`codexMaxDetailBytes`·`oneLine`·`errorMessage`·`withSteerDesk`·`Close` doc과 `discarding`·`steer` 외의 스트림 규칙 | 32 | 「프롬프트는 `--` 뒤에 와서 플랫폼 제어 플래그를 바꿀 수 없다」·「죽은 러너가 남긴 arg0 링크는 아카이브가 거부하는 절대 링크」·「잘린 JSON 레코드는 레코드가 아니다」는 세 규칙은 지우면 실패 모드로 읽힌다 |
| 레거시 워크로드 타입 호환 — `legacyWorkloadTypeCodexChat`·`legacyWorkloadTypeCodex`·`legacyWorkloadCodexChat` doc, `WorkloadTypeCodexManager`·`WorkloadTypeCodexTask` doc, tenancy 테스트의 `sample` grant 설명 | 20 | 「구 이름이 모든 입력 경로에서 아직 받아들여진다」는 사실은 배포된 테넌트 설정과 기존 세션 레코드에서만 온다 — 지우면 이전 이름으로 쓰인 세션이 조용히 사라지는 수정의 근거가 없다 |
| 스테어·도구 표면 — `Steered`·`SteerableWorkload`·`IsCodex` doc, `codexCLI`·`codexAgentURLEnv`·`agentURL` doc, `codexSidecar`·`probeSidecar`·`refreshSidecar` 관련 유지 문장 | 15 | 「applied는 '턴이 받았다'와 '턴에 못 미쳤다'를 가른다」·「빈 `agentURL`은 갱신 도구를 아예 내지 않는다」는 구분은 반환값 이름에서 드러나지 않는다 |
| e2e 하네스 — `execInAgent` doc, `TestCodex4_ModelSeesNoShellFileOrImageTool` doc, `tenantToken`·`doAs` doc, `TestCodexChat_SlotSplitsLoginAndDatabase` doc, `headerResolver` doc, `alpha's grant` 설명, `"*" is never echoed` 주석 | 20 | 「사이드카가 있는 파드라 API 서버가 기본 컨테이너를 고르지 않는다」·「빈 토큰은 Authorization 헤더를 보내지 않아 운영자가 된다」·「와일드카드는 양쪽에 펼친 목록으로 보인다」는 하네스 규칙은 테스트 이름만으로는 복원되지 않는다 |
| 패키지 주석 — `control-plane/internal/tenancy/tenancy.go` 의 패키지 doc | 4 | 패키지 doc은 Go 관례상 유지한다 — 「토큰 없는 요청은 운영자」의 기본 규칙을 패키지 수준에서 한 문장으로 남긴다 |

## 판단이 갈린 것

- 테스트 파일의 머리 주석은 AC 번호만 남기는 한 줄을 남기지 않았다. `docs/prd/multitenancy.md`·
  `docs/prd/codex-workload.md`가 이미 말하는 내용의 사본이며, 테스트-AC 매핑은 `// 검증 AC:`·
  `// 검증 시나리오:` 지시자가 맡는다(두 지시자 모두 `DIRECTIVE` 목록에 있어 지문·판정에서 제외된다).
- `control-plane/internal/tenancy/tenancy.go` 의 패키지 doc과 `control-plane/internal/adapter/k8s/codex.go` 의
  파일 머리(코드x 런타임 구성의 개요)는 남겼다 — 둘 다 패키지·축 수준에서 무엇을 보장하는지 말하는 문장이라
  지우면 이 축이 무엇을 하는 축인지 코드에서 복원되지 않는다.
- `data-plane/cmd/agent/codex.go` 의 파일 머리 15줄은 지웠다. 파일 머리는 「이 파일이 무엇을 하는 파일인지」
  를 다시 말하는 자리이고, 그 내용은 각 함수 doc이 이미 말한다 — 반면 `control-plane/internal/adapter/k8s/codex.go`
  의 파일 머리는 「파드 안에서 토큰이 어디에 있는지」를 말해 남겼다.

## 원장 반영

- 이 패스로 주석이 전부 사라진 파일 3개의 원장 행은 지웠다(빈 범위는 등재가 아니다 — R1):
  `control-plane/cmd/control-plane/tenancy.go` · `control-plane/test/e2e_codex_2_pod_shape_test.go` ·
  `data-plane/cmd/agent/codex_skills_mcp_test.go`.
- 남은 15개 행의 판정 칸을 `완료`로 바꾸고 줄 수·지문을 판정 후 실측값으로 갱신했다.
  게이트 `scripts/check_comment_policy.py` 는 rc=0 — R1~R6 위반 0.
