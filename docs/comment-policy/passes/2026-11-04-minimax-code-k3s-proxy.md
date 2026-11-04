# 판정 패스 — minimax-code 파일군 + K3s MCP 프록시 (2026-11-04)

범위(L 표면, 20파일 · **398줄 판정, 제거 143 · 유지 255**): minimax-code 워크로드의 컨트롤 플레인 쪽 8파일과
데이터 플레인 `minimax_*` 10파일, 그리고 같은 기능이 쓰는 `k3s_mcp_proxy.go`·`_test.go`. 모델 정의의 판정
슬라이스(예산 400줄)를 한 덩어리로 가져간 것이다. 열린 PR #192(`k8s/minimax_code.go` 등)와 파일이 겹치는
`control-plane/internal/adapter/k8s/minimax_code.go`(58줄)는 뺐다.

판정은 필요성 시험으로 한다 — 각 주석에 「지우면 이 코드를 고치러 온 사람이 틀린 판단을 하거나 확인에 큰 비용을
치르는가」를 묻는다. 코드를 바꾸지 않았다(주석 삭제와 doc 주석 문장 축소뿐).

## 제거 — 143줄

좌표는 「이 내용을 이미 말하는 곳」이다. AC 번호는 `docs/prd/minimax-code-workload.md`의 AC-G1~G6, 그 밖의 AC는 해당 PRD.

| 파일 | 제거 | 줄 | 좌표 |
| --- | --- | ---: | --- |
| `control-plane/cmd/control-plane/minimax_code.go` | `miniMaxConfig` doc의 「claude-code 필드와 분리」 | 3 | 필드가 `miniMaxConfig` 별도 타입이라는 것 자체, AC-G3 |
| `control-plane/cmd/control-plane/minimax_code_test.go` | 테스트 머리의 AC-G3·AC-G4 재진술 2건 | 4 | `TestValidateMiniMaxSecret`·`TestMiniMaxArchiveOption`의 이름과 단언, AC-G3·G4 |
| `control-plane/internal/api/minimax_code_test.go` | AC-G1 머리 | 2 | 테스트 이름·단언, AC-G1·AC-E6 |
| `control-plane/internal/service/minimax_code_test.go` | `newMiniMaxService` 재진술, 테스트 머리 4건 | 9 | `TestMiniMaxCode*` 이름·단언, AC-G1·G2·G4 |
| `control-plane/internal/service/minimax_idle_test.go` | AC-G6/F3 머리 | 2 | AC-F3·AC-G6 |
| `control-plane/internal/session/minimax_code.go` | 타입 doc의 「claude-code 실행 모델 위에서…」, `HasApprovalGate`의 괄호 AC 나열 | 4 | `HasApprovalGate`·`QueuesPrompts`의 본문과 PRD 포인터(남김) |
| `control-plane/internal/session/minimax_code_test.go` | 테스트 머리 2건 | 3 | 테스트 이름·단언, AC-G1·AC-E6 |
| `control-plane/test/minimax_code_orchestrator_test.go` | 파일 머리 3줄, 테스트 머리 12건 | 31 | 각 `Test*` 이름·단언, AC-G1·G3~G6, 같은 헬퍼의 `workload_type_orchestrator_test.go` |
| `data-plane/cmd/agent/minimax_code.go` | 필드 doc 2건, `resolveMiniMaxModel`, `marketplacePlugins`, `openItem`, `openMessage` | 10 | 필드·함수 이름과 본문(`k3sProxyURL`·`fetchSeed`·`resolveMiniMaxModel` …), AC-G1·AC-E6 |
| `data-plane/cmd/agent/minimax_code_test.go` | 테스트 머리 전부 | 24 | 테스트 이름·단언. `--effort`·16384 토큰 관측은 `miniMaxModelLimits` doc이 말한다(유지) |
| `data-plane/cmd/agent/minimax_marketplace.go` | 타입·필드·헬퍼 doc 6건 | 7 | `miniMaxMarketplaceSeeder`·`bufferedResponse`·`pathList`·`writeMiniMaxSeedPlugin`의 이름·시그니처, 같은 파일의 `insideTree` doc |
| `data-plane/cmd/agent/minimax_marketplace_test.go` | 테스트 머리 3건 | 6 | 테스트 이름·단언, `githubTokenThroughProxy` doc |
| `data-plane/cmd/agent/minimax_mcp_registration.go` | `ensureMiniMaxMCPRegistration` doc | 2 | 파일 머리의 「이름으로 병합·태그」 설명 |
| `data-plane/cmd/agent/minimax_mcp_registration_test.go` | 테스트 머리 2건 | 5 | 테스트 이름·단언, 파일 머리 설명 |
| `data-plane/cmd/agent/minimax_plugins.go` | `miniMaxManagedPluginMarker` 위에 붙은 `ensureHome` 설명 | 5 | **틀린 주석**: `ensureHome`은 이 파일에 없고 `minimax_code.go`의 `ensureHome` doc이 같은 말을 한다. 마커 설명은 남김 |
| `data-plane/cmd/agent/minimax_plugins_test.go` | 테스트 머리 5건, 헬퍼 doc | 13 | 테스트 이름·단언, AC-G5·G6 |
| `data-plane/cmd/agent/minimax_tools.go` | `miniMaxEndpoints` doc, `validateMiniMaxClientEnv` doc | 4 | 타입·함수 본문, AC-G3·G6 |
| `data-plane/cmd/agent/k3s_mcp_proxy.go` | `k3sMCPFixedHeaders`·`k3sMCPPath` doc | 4 | 두 상수의 이름과 값, 같은 파일의 프록시 doc |
| `data-plane/cmd/agent/k3s_mcp_proxy_test.go` | 테스트 머리 2건 | 5 | `newK3SMCPProxy` doc(옵션 키·health 라우트), AC-G5 |

## 유지 — 255줄 (사유)

| 대상 | 줄 | 필요 사유 |
| --- | ---: | --- |
| `Keep in sync with control-plane/internal/adapter/k8s (…)` 4건 (`minimax_code.go`·`minimax_marketplace.go`·`minimax_tools.go`·`k3s_mcp_proxy.go`) | 8 | 두 모듈이 같은 env 이름·포트를 따로 선언하므로 한쪽만 바꾸면 파드가 조용히 어긋난다 |
| `minimax_mcp_registration.go` 머리의 2026-10-24 관측(mcode 0.6.12) | 17 | 플러그인 MCP 서버 탐색이 턴과 경합해 도구가 하나도 안 실리는 현상 — 코드 형태로는 왜 `mcp.json`에 쓰는지 알 수 없다 |
| `miniMaxModelLimits`·`miniMaxModelDefinition`·`miniMaxPermission`·`miniMaxEffort` 등 mcode의 기본값·거동을 말하는 주석 | 다수 | mcode 문서화되지 않은 동작(200k 문맥·16384 토큰 상한, `--effort` 거부, 자동 업데이트)이라 코드에서 복원 불가 |
| `miniMaxCredentialEnv`·`validateMiniMaxSecret`·`gitCloneMarketplace`·`insideTree`·`k3sMCPProxyRequestHeaderAllowed` 등 보안 불변식 주석 | 다수 | 지우면 토큰이 HOME·아카이브·워크로드 파드로 새는 방향의 수정을 막는 근거가 사라진다 |
| `minimax_marketplace.go`·`k3s_mcp_proxy.go`·`minimax_tools.go` 머리의 배치 근거(헬퍼 파드 프록시, 승인 게이트 밖, 120 s 기본값) | 다수 | 왜 그 컨테이너에서 도는지·왜 게이트 밖인지는 코드만으로는 틀린 이동을 부른다 |
| `minimax_stream_contract_test.go` 전부 | 11 | 실측 캡처(mcode 0.6.12, 2026-10-22)의 출처와 각 픽스처가 담은 형태 — 스트림 모양이 바뀌면 여기서 드러나게 하는 근거 |
| `minimax_plugins_test.go` 픽스처·마이그레이션 설명 4건 | 9 | 실제 마켓플레이스 모양(OAuth client id·`${VAR}` 헤더), 서버 이중 로드 방지 사유 |
| `minimax_marketplace_test.go` 헬퍼 파드 픽스처 설명 | 2 | 픽스처가 왜 토큰만 있고 마켓플레이스가 없는지 |
| `test/minimax_code_orchestrator_test.go` 쓰기 가능 scratch 인라인 | 2 | 읽기 전용 루트에서 마켓플레이스 clone이 쓸 emptyDir이 필요한 이유 |
| `session/minimax_code.go` `QueuesPrompts` doc | 4 | 과대 프롬프트가 스냅샷을 복원만 시키고 거부되는 것을 막는 사전 검사의 이유 |

## 판단이 갈린 것

- 컨트롤 플레인 테스트 파일의 머리 주석은 거의 다 지웠다(전체 제거 143줄 중 약 4분의 3이 테스트 머리).
  AC 번호만 남기는 한 줄은 `docs/prd`·`docs/test`가 이미 말하는 내용의 사본이어서다. 테스트-AC 매핑은
  `// 검증 AC:`·`// 검증 시나리오:` 지시자가 맡는다.
- 이 패스로 주석이 전부 사라진 파일 7개의 원장 행은 지웠다(빈 범위는 등재가 아니다 — R1):
  `control-plane/cmd/control-plane/minimax_code_test.go` · `control-plane/internal/api/minimax_code_test.go` ·
  `control-plane/internal/service/minimax_idle_test.go` · `control-plane/internal/session/minimax_code_test.go` ·
  `data-plane/cmd/agent/minimax_code_test.go` · `data-plane/cmd/agent/minimax_mcp_registration_test.go` ·
  `data-plane/cmd/agent/k3s_mcp_proxy_test.go`.

## 범위 밖(후속)

- `control-plane/internal/adapter/k8s/minimax_code.go` — 열린 PR #192와 파일이 겹쳐 뺐다. 그 PR 머지 뒤 다음 슬라이스.
- 나머지 L 미판정 행, D·E 표면 전부.
