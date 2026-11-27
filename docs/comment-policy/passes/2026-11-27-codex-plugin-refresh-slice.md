# 판정 패스 — codex 플러그인 새로고침 슬라이스 (2026-11-27)

범위(L 표면, 5파일 · **47줄 판정, 제거 31 · 유지 16**): PR #206(AC-J7 「수동 플러그인 새로고침」)이 세운
codex 플러그인 새로고침 슬라이스의 신규 L 행 5개. 4개 파일은 주석이 남아 원장 행이 `완료`로 바뀌었고,
`control-plane/internal/service/plugin_refresh_test.go` 는 주석이 전부 사라져 원장 행이 삭제되었다.

모델 정의의 「판정 슬라이스」 예산(400줄)을 다 채우지 않은 사유: 이번 트리거(#204·#205·#206 머지)가 만든
차이가 이 5행 47줄 그 자체이고, 같은 트리거가 재측정한 기존 행 가운데 `codex.go`·`codex_skills.go` 는
열린 PR #272, `minimax_marketplace.go`·`minimax_marketplace_test.go` 는 열린 PR #201 이 손대고 있어
「파일이 실제로 겹치는 열린 PR이 있을 때만 그 덩어리를 뺀다」에 따라 빠졌다. 남은 L 미판정은 누적분이라
정의 「판정 슬라이스」의 다음 덩어리로 감지 단계가 잇는다.

판정은 필요성 시험으로 한다 — 각 주석에 「지우면 이 코드를 고치러 온 사람이 틀린 판단을 하거나 확인에 큰 비용을
치르는가」를 묻는다. 코드는 바꾸지 않았다(주석 삭제뿐).

## 제거 — 31줄

좌표는 「이 내용을 이미 말하는 곳」이다. AC-J7 의 정본은 `docs/prd/codex-workload.md` 다.

| 파일 | 제거 | 줄 | 좌표 |
| --- | --- | ---: | --- |
| `data-plane/cmd/agent/codex_plugin_refresh.go` | 파일 머리 AC-J7 블록, `pluginRefresh` doc, `pluginRefresher` doc | 10 | `docs/prd/codex-workload.md` AC-J7, 타입·인터페이스 이름과 바로 아래 코드 |
| `data-plane/cmd/agent/codex_plugin_refresh_test.go` | 파일 머리 AC-J7 블록 3건, `refreshableCodexCLI` doc, 시나리오 표제 3건 | 14 | 각 `Test*` 이름과 단언, `docs/prd/codex-workload.md` AC-J7 |
| `control-plane/internal/api/plugin_refresh_test.go` | 테스트 머리 AC-J7 블록 | 3 | 테스트 이름과 단언, `docs/prd/codex-workload.md` AC-J7 |
| `control-plane/internal/service/plugin_refresh_test.go` | 테스트 머리 AC-J7 블록 | 3 | 테스트 이름과 단언, `docs/prd/codex-workload.md` AC-J7 (**주석이 전부 사라져 원장 행 삭제**) |

## 유지 — 16줄 (사유)

| 대상 | 줄 | 필요 사유 |
| --- | ---: | --- |
| `pluginRefreshPath`·`pluginRefreshTurnPath` doc (`data-plane/cmd/agent/codex_plugin_refresh.go`) | 3 | 두 경로 상수는 경로 문자열만 보면 어느 호출자의 것인지 구분이 안 된다 — 하나는 control plane 이 부르고 하나는 턴 안 skills MCP 가 loopback 으로 부르므로, 구분하지 못하면 새로고침이 턴 도중 끼어든다 |
| `codexPluginRefreshTimeout` doc (`data-plane/cmd/agent/codex_plugin_refresh.go`) | 1 | 새로고침은 부팅이 아니라 마켓플레이스 클론 1회라 대기 시간이 클론 제한에 30초 여유를 더한 값이다 — 숫자 30초의 근거가 코드에는 없다 |
| 새로고침이 새 시드를 못 읽어도 스킬을 비우지 않는 규칙 (`data-plane/cmd/agent/codex_plugin_refresh.go`) | 2 | 새 시드를 못 읽으면 부팅과 달리 지금 있는 스킬을 그대로 둔다 — 지우면 실패를 「스킬 초기화」로 오해해 스킬을 지우는 수정이 근거를 잃는다 |
| `pluginRefreshNotice` doc (`data-plane/cmd/agent/codex_plugin_refresh.go`) | 1 | 이 함수는 쓰지 않고 반환만 하며 그 반환 문자열이 대화 기록에 붙는다 — 이름에서 드러나지 않아 대화로 나가는 경로를 놓칠 수 있다 |
| 플랫폼이 대화에 아무것도 쓰지 않는다는 계약과 턴 중 새로고침이 거절된다는 규칙 (`data-plane/cmd/agent/codex_plugin_refresh_test.go`) | 3 | 다음 턴 프롬프트에 플랫폼이 말을 섞지 않고 새로고침은 턴이 돌거나 대기 중일 때 거절된다는 두 불변식은 테스트 이름만으로는 복원되지 않는다 |
| 플러그인이 없는 워크로드에 새로고침 라우트가 없다는 계약 (`data-plane/cmd/agent/codex_plugin_refresh_test.go`) | 1 | 라우트 자체가 없어 404 가 정상 응답이다 — 테스트 이름이 「미지원」만 말해 404 의 의미가 드러나지 않는다 |
| 턴 안 새로고침 경로는 파드 안 프로세스만 닿는다는 제한 (`data-plane/cmd/agent/codex_plugin_refresh_test.go`) | 1 | 이 경로는 파드 안 프로세스만 닿을 수 있는 loopback 경로다 — 이 제한이 없으면 턴 도중 외부에서 새로고침을 끼어 넣을 수 있다 |
| 운영자는 모든 테넌트의 세션을 새로고침할 수 있다는 계약 (`control-plane/internal/api/plugin_refresh_test.go`) | 1 | 테넌트를 붙이지 않은 호출이 운영자라 모든 테넌트를 본다 — 200 기대값의 근거가 코드에는 없다 |
| kind SUT 의 codex 사이드카에는 마켓플레이스 토큰이 없다는 환경 제약 (`web/e2e/codex-workload-7-plugin-refresh.spec.ts`) | 3 | 토큰이 없어 새로고침 결과가 「새로 받을 마켓플레이스가 없다」가 된다 — 이 기대값이 환경 제약인지 결함인지 구분하는 단서다 |

## 판단이 갈린 것

- `pluginRefreshTurnPath` doc 끝의 `(AC-J7)` 표기는 남겼다. AC 번호는 `docs/prd/codex-workload.md` 의
  정본을 가리키는 짧은 참조라서, 이 주석을 지울 근거가 된다면 AC 블록과 함께 지워질 대상이지 따로
  걸러낼 대상이 아니다.
- 테스트 파일의 시나리오 표제 중 「플러그인이 없는 워크로드」와 「턴 안 경로는 파드 안 프로세스만」 두 건은
  테스트 이름과 단언으로는 복원되지 않는 계약이라 남겼다. 나머지 표제(성공·실패 케이스)는 단언이 이미
  말하는 내용이라 걷었다.
