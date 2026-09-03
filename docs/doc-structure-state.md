# Session Pod Platform 디자인 문서 구조 상태 추적

> 이 문서는 **프론트엔드/디자인 측 사슬**(`가치 → 사용자 여정 → mockup ↔ 디자인 시스템`)의 상태를 추적합니다.
> 백엔드 측 사슬(`가치 → PRD → AC → 테스트`)은 별도 문서 `doc-tracker.md`에서 추적합니다.
> 두 문서가 공유하는 단일 진실 원천은 가치 문서(`values.md`, V1~V5·V8)입니다.

> 📌 **식별자 전환 (2026-08-30)**: 여정·단계 식별자가 순번(`J1`, `J1-S1`)에서 슬러그(`JRN-…`, `STP-…`)로 바뀌었습니다.
> 구↔신 매핑표는 [`user-journeys/README.md`](./user-journeys/README.md#구-식별자-매핑-2026-08-30-전환)에 있습니다.
> 아래 "변경 이력"의 과거 항목은 당시 기록이므로 옛 식별자를 그대로 둡니다.

- **마지막 검증 시점**: 2026-09-03 (10) (여정 mockup 층위 신설 — `docs/journeys/` 파일럿 2개 + 집행 하네스)
- **검증 범위**: 상류(product-doc 사슬)에서 세 번째 워크로드 타입(AC-F1~F6)이 확정되어, 여정
  `JRN-approval-gated-work`(4단계)와 mockup `gated-workspace.html`을 신설하고 `new-session.html`·`index.html`의
  타입 분기를 3종으로 확장했다. 여정 8→9, 단계 29→33, mockup 5→6. **가치는 변하지 않았다**(V8 상속).
  늘어난 ❌ 1개는 승인 결정 화면이 **이 레포 밖(외부 승인 게이트웨이)** 이라는 사실이 드러난 결과다.
  이어서 (8)판에서 상류의 프록시 배치 확정(사이드카 → 워크로드 파드 밖)을, (9)판에서 보조 파드를 헬퍼 파드
  하나로 합친 결정을 `gated-workspace.html`에 반영했다 — 두 판 모두 **수치는 변하지 않고 내용만 갱신**되었다.
  잔여: 미시각화 7단계, mockup↔시스템 미연결(토큰 7중복), 페르소나 미검증

## 현재 상태 요약

- 정의된 가치: **6개** (V1~V5·V8, `values.md` — 참조 전용)
- 사용자 여정: **9개** — 전부 가치에 연결됨 (고아 여정 0). `JRN-session-deletion`은 **연결이 부분적**(아래 위험 참고)
- 여정 단계: **33개** (session-creation 3 · idle-resume 4 · multi-session-switch 4 · concurrent-access 3 ·
  shell-interaction 4 · agent-prompt-loop 5 · manual-freeze 3 · session-deletion 3 · approval-gated-work 4)
- Mockup: **6개** (`mockups/`: index, new-session, workspace, agent-workspace, gated-workspace, restore) — 매핑 단일 소스는 `mockups/README.md`
- 여정 mockup 페이지: **2개 / 판정 대상 8개** (`journeys/`: `JRN-session-creation`, `JRN-shell-interaction`) — 여정 하나 = 페이지 하나 층위. 화면 단위 mockup 6개와 **다른 지표**이며 매핑은 같은 `mockups/README.md`의 "여정 → 여정 mockup 페이지 매핑" 표. 마크업 규약 `journeys/README.md`, 집행은 CI `docs-journey-mockup`
- 디자인 시스템: **정의됨 — 정본은 코드** (`web/src/design/tokens.css` 토큰·프리미티브, `web/src/app/shell.css` 컴포넌트·패턴 약 45종, 색인 `web/src/design/README.md`). mockup 6개는 아직 **미연결**
- 주입형 스킬(`.claude/skills/`): **설치됨** — `ui-with-design-system`, `screen-with-mockup-and-design-system`
- **건강 상태**: 🟡 **가치 측 위험 0 · 시각화 없는 가치 0**이나, **문서화가 구현보다 뒤처진 영역이 드러남**.
  V1~V5·V8은 전부 여정과 mockup에 연결됩니다. 다만 2026-08-30 재작성에서 세션 삭제·수동 동결이
  **구현에는 있고 여정·mockup에는 없던** 상태였음이 확인되어, 여정은 채웠고 mockup은 결정 대기로 남았습니다.

## 연결 매트릭스

### 가치 ↔ 여정
| 가치 | 여정 | 상태 |
|------|------|------|
| V1 세션 격리 | `JRN-session-creation` | ✅ |
| V2 유휴 자원 회수 | `JRN-idle-resume`, `JRN-manual-freeze`, `JRN-session-deletion`(부분) | ✅ |
| V3 끊김 없는 세션 연속성 | `JRN-idle-resume`, `JRN-multi-session-switch`, `JRN-concurrent-access`, `JRN-shell-interaction`, `JRN-agent-prompt-loop`, `JRN-manual-freeze`, `JRN-approval-gated-work` | ✅ |
| V4 자유로운 멀티세션 전환 | `JRN-multi-session-switch` | ✅ |
| V5 일관된 세션 상태 | `JRN-session-creation`, `JRN-concurrent-access` | ✅ |
| V8 목적에 맞는 작업 환경 선택 | `JRN-agent-prompt-loop`, `JRN-approval-gated-work` | ✅ |

### 여정 단계 ↔ mockup
표기: ✅ 전용 화면 · ⚠️ 부분/암시 · ❌ 없음 · ⚪ 의도적 비시각화

| 여정 | 단계 수 | ✅ | ⚠️ | ❌ | ⚪ | mockup |
|------|:---:|:--:|:--:|:--:|:--:|------|
| `JRN-session-creation` | 3 | 3 | 0 | 0 | 0 | new-session, workspace |
| `JRN-idle-resume` | 4 | 1 | 2 | 1 | 0 | restore, agent-workspace; `STP-reaccess` 없음 |
| `JRN-multi-session-switch` | 4 | 1 | 2 | 1 | 0 | index; `STP-switch-back` 없음 |
| `JRN-concurrent-access` | 3 | 0 | 0 | 0 | 3 | 없음 — 의도적 비시각화 |
| `JRN-shell-interaction` | 4 | 4 | 0 | 0 | 0 | workspace(쉘 콘솔 + Shell state 패널) |
| `JRN-agent-prompt-loop` | 5 | 5 | 0 | 0 | 0 | new-session(타입 선택), agent-workspace |
| `JRN-manual-freeze` | 3 | 1 | 1 | 1 | 0 | workspace·agent-workspace의 Freeze/Archive now |
| `JRN-session-deletion` | 3 | 0 | 0 | 3 | 0 | 없음 — SPA에만 구현됨 |
| `JRN-approval-gated-work` | 4 | 3 | 0 | 1 | 0 | gated-workspace, new-session(타입 카드); `STP-approval-decide`는 레포 밖 화면 |
| **합계** | **33** | **18** | **5** | **7** | **3** | — |

### 여정 ↔ 여정 mockup 페이지 (`journeys/`)
표기: ✅ 전용 페이지 있음 · ⏳ 예정(잔여 격차) · ⚪ 예외 등재(그리지 않음)

| 여정 | 상태 | 페이지 |
|------|:--:|------|
| `JRN-session-creation` | ✅ | `journeys/JRN-session-creation/` |
| `JRN-shell-interaction` | ✅ | `journeys/JRN-shell-interaction/` |
| `JRN-concurrent-access` | ⚪ | 없음 — 수용된 위험 등재 |
| 나머지 6개 여정 | ⏳ | 없음 — `mockups/README.md` 매핑 표에 `⏳ 예정`으로 선언 |

> 이 표의 ✅ 는 **그 여정 전용 페이지가 있다**는 뜻이다. 아래 "여정 단계 ↔ mockup" 표의 ✅ 18 은
> **어떤 화면 단위 mockup이 그 단계를 담고 있다**는 뜻이라 서로 다른 지표다 — 합산하지 않는다.

### 가치 ↔ mockup
| 가치 | 시각화 mockup | 상태 |
|------|---------------|------|
| V1 세션 격리 | new-session, workspace, agent-workspace, gated-workspace(Egress 패널) | ✅ |
| V2 유휴 자원 회수 | restore, agent-workspace, gated-workspace, workspace(Freeze now) | ✅ (부분) |
| V3 끊김 없는 연속성 | restore, workspace, agent-workspace, gated-workspace | ✅ (부분; 동시성 몫은 비시각화) |
| V4 멀티세션 전환 | index | ✅ (부분) |
| V5 일관된 상태 | new-session | ✅ (생성 몫; 동시성 몫은 비시각화) |
| V8 작업 환경 선택 | new-session(타입 카드 3종·모델 선택), agent-workspace, gated-workspace | ✅ |

> **시각화 없는 가치 0.**

### mockup ↔ 디자인 시스템
- **디자인 시스템 정본(2026-08-08 (4) 신설)**: `web/src/design/` — 토큰·프리미티브 `tokens.css`, 컴포넌트·패턴 `web/src/app/shell.css`, 색인 `web/src/design/README.md`. 사용자 결정에 따라 **코드가 정본이고 문서는 얇은 색인**이다. 발견 가능성은 루트 `README.md`가 이 위치를 가리키는 것으로 확보(주입형 스킬이 "README가 가리키는 위치"로 정본을 찾는다).
- **연결 상태는 여전히 미연결**: index/new-session/workspace/agent-workspace/gated-workspace/restore **6개 전부 임의 스타일 mockup(🟢) 유지**. 각 파일이 자기 `:root`에 같은 값을 인라인으로 갖고 있을 뿐 정본을 참조하지 않는다. `gated-workspace`는 `agent-workspace`의 인라인 토큰을 복사해 만들어 중복이 한 겹 더 늘었다.
- **방향에 주의**: 역사적으로 mockup이 먼저였고 `tokens.css`가 거기서 1:1 이식됐다(파일 주석에 명시). 그러나 이제 정본은 코드이므로 **코드를 먼저 고치고 mockup이 따라온다**로 방향이 뒤집혔다.
- **중복 비용**: 토큰 1개 변경 시 `tokens.css` + mockup 6개 = **7곳**. 이것이 남은 🟢 위험의 실체이며, 디자인 시스템 부재가 아니라 *동기화 부재*로 성격이 바뀌었다.

## 위험 진단

### 🔴 가치 측 위험
- **고아 여정**: 없음 (9개 여정 모두 유효한 가치 참조)
- **고아 mockup**: 없음 — 인덱스(`mockups/README.md`)로 6개 전부 여정/가치에 매핑됨
- **시각화 없는 가치**: 없음
- **페르소나 미검증**: 여정 9개·단계 33개가 실사용자로 검증된 적 없는 P1 위에 서 있다
  (`user-journeys/README.md` 미해결 항목). 이 사슬에서 가장 오래된 미해결 항목이다.

### 🟠 문서화가 구현보다 뒤처진 영역 (2026-08-30 신규 인지)
- **세션 삭제**: `DELETE /api/v1/sessions/{id}` · `web/src/app/DeleteSessionDialog.tsx` ·
  `web/e2e/session-deletion.spec.ts`가 있는데 여정·mockup·PRD AC가 없었다.
  → 여정 `JRN-session-deletion` 신설로 문서 공백은 메웠고, **mockup 3단계와 PRD 전용 AC는 여전히 없음**.
- **수동 동결**: `POST /api/v1/sessions/{id}/snapshot` · Workspace의 Freeze/Archive now ·
  `web/e2e/manual-archive.spec.ts`가 있는데 여정이 없었다.
  → 여정 `JRN-manual-freeze` 신설. **수동 트리거 전용 AC 없음**(AC-B1/A3에 얹혀 있음).
- **원인**: 커버리지 점검이 `가치 → 여정 → mockup` **한 방향**이라 "구현에는 있는데 문서에 없는" 흐름이 잡히지 않았다.
  다음 검증부터는 API 라우트·SPA 화면 목록에서 여정으로 거슬러 올라가는 **역방향 점검**을 함께 수행할 것.

### 🟡 시각화 누락
- **시각화 누락 단계 (7)**: `STP-reaccess`, `STP-switch-back`, `STP-freeze-decision`,
  `STP-delete-intent`, `STP-delete-confirm`, `STP-delete-settled`, `STP-approval-decide` — 전용 화면 없음. **결정 필요**.
- **🟠 다른 제품에 걸친 단계 (2026-09-03 신규)**: `STP-approval-decide`는 결정이 **외부 승인 게이트웨이 화면**에서
  일어나므로 이 레포가 그릴 대상이 아닐 수 있다. `JRN-concurrent-access`처럼 ⚪(의도적 비시각화)로 수용할지,
  세션 쪽에 요약 화면을 두어 ✅로 만들지 **결정 대기**. 현재는 보수적으로 ❌로 센다.
- **부분 시각화 단계 (5)**: `STP-step-away`, `STP-auto-freeze`, `STP-switch-away`, `STP-target-activation`,
  `STP-freeze-confirm` — 상태 표시·네비게이션으로 암시되나 단계 전용 화면 없음.
- **✅ 해결됨 (2026-08-08)**: `STP-shell-state-carry` — `workspace.html`에 Shell state 패널 추가로 ❌→✅.
- **✅ 해결됨 (2026-08-08)**: `restore.html` 콘솔 미갱신 — 쉘 명령·출력으로 교체하여 mockup suite 전체가 쉘 기준으로 정합.
- **✅ 해결됨 (2026-08-08)**: `claude-code` 워크로드 타입 미시각화 — 생성 화면의 타입 선택과 프롬프트·응답 콘솔 2종 확보.
- **✅ 해결됨 (2026-09-03)**: `approval-gated` 워크로드 타입 미시각화 — `gated-workspace.html` 신설(승인 대기 배지·
  승인/거절 verdict·Approvals·Egress 패널)과 `new-session.html`의 세 번째 타입 카드로 4단계 중 3단계 확보.
  상류 AC 확정과 **같은 판에서** 하류를 채웠으므로, 2026-08-08의 V8처럼 "시각화 없는 가치" 구간이 생기지 않았다.

### 🟢 디자인 시스템 측 위험
- **임의 스타일 mockup**: **6개 전부 유지**(2026-09-03 `gated-workspace` 추가). 정본이 생겼으므로 원인이 "시스템 부재"에서 **"정본 미참조(값 7중복)"**로 바뀌었다. 마이그레이션은 미수행 — mockup을 정본에 물릴지, 아니면 "mockup은 스케치이므로 복제를 수용"할지 **결정 대기**.
- **✅ 해결됨 (2026-08-08 (4)) — 디자인 시스템 부재**: `web/src/design/`를 정본으로 확정.
- **✅ 해결됨 (2026-08-08 (4)) — 주입형 스킬 미설치**: `.claude/skills/` 2종 설치 완료.
- **⚠️ SPA 자체는 검증 대상 밖**: `web/src/screens/`의 실제 화면이 정본을 얼마나 지키는지는 아직 확인하지 않았다.
  다음 검증 때 `shell.css` 밖 인라인 스타일·하드코딩 hex 유무를 점검할 필요가 있다.

### 🟠 인접 사슬에서 인지된 항목 (이 스킬 관할 밖, 참고용)
- **고아 가치(소유자 미지정)**: V1~V5·V8 전부 소유자 미지정 — `doc-tracker.md`/`values.md` 참고.
- **세션 삭제·수동 동결의 가치 연결**: V2의 서술이 "유휴 세션의 자동 회수"에 한정되어 사용자 주도 종료·동결을
  담지 못한다. V2 서술 확장 또는 새 가치 신설 여부는 product-doc-engineer 판단.
- **mockup의 명시 model 예시**: `new-session.html`의 `model-a`/`model-b`는 soft catalog 상태의 자리표시자다.
- **승인 게이트의 파드 구성**: `gated-workspace.html`이 그리는 **워크로드 파드 + 헬퍼 파드(MCP·credential-proxy 컨테이너)**
  구성, 컨테이너 단위 자격 증명 분리, "허용 목록에 외부 origin이 하나도 없다"는 표기는 mockup의 표현이 아니라
  **AC-F2/F4/F6의 계약**이다.
  (2026-09-03 이전 판의 ⚠️ 경고는 프록시를 보조 파드로 옮기면서 해소되었다.)
  남은 전제 — 워크로드↔프록시 홉이 평문이라는 점, 참조 구현(`dlddu/pure-agent`)이 클러스터에서 검증된 적
  없다는 점 — 은 `doc-tracker.md`에서 추적한다.
- **에이전트 세션의 페르소나**: `JRN-agent-prompt-loop`는 P1으로 작성됐으나 전용 페르소나 필요 여부 미확인.

## 수용된 위험
- **`JRN-concurrent-access` 시각화 제외 (2026-06-27 수용)**: 이 여정이 지키는 것은 화면이 아니라 결과의 일관성이므로
  **UI 시각화 대상이 아님**. 따라서 3단계의 "mockup 없음"은 누락이 아니라 의도된 상태.
  이전에 `mockups/restore.html`·`workspace.html`에 있던 동시 접근(operator+automation) 패널은 UI 비대상 판단으로 제거.
  **재검토 시점 (2026-09-03 추가)**: 2027-06-27(수용 1년) 또는 그 전이라도 이 여정에 사용자 표면이 생기는 상류 변경이
  확정되는 시점 — 충돌·재시도가 화면에 드러나야 하는 AC가 생기거나, `STP-collision`이 UI 상태로 노출되면 즉시 재검토한다.
  등재는 여정 단위이며, 이 여정은 `docs/journeys/`에 페이지를 두지 않는다(하네스가 강제한다).

## 권장 다음 단계 (우선순위순)

| 우선순위 | 항목 | 권장 대응 |
|---------|------|-----------|
| 🔴 | 페르소나 미검증 | 실사용자 1~2명 인터뷰로 P1 확정 또는 폐기 (product-doc-engineer) |
| 🟠 | 세션 삭제 mockup 3단계 부재 | mockup 추가 또는 "SPA 구현으로 갈음" 결정(수용 시 본 문서에 기록) |
| 🟠 | 여정 mockup 잔여 6개 (2026-09-03) | `journeys/` 페이지 신설. 만들 때 다른 페이지의 `data-branch-pending` 링크 승급이 CI로 강제된다 |
| 🟠 | 세션 삭제·수동 동결의 PRD AC 부재 | 전용 AC 신설 여부 결정 (product-doc-engineer) |
| 🟡 | 누락 단계 `STP-reaccess`·`STP-switch-back`·`STP-freeze-decision` | mockup 추가 또는 의도적 제외 결정 |
| 🟠 | `STP-approval-decide`의 소유권 (2026-09-03) | 승인 결정 화면이 레포 밖 — ⚪(비대상) 수용 또는 세션 쪽 요약 화면 신설 결정 |
| 🟡 | 부분 시각화 5단계 | 단계 전용 화면 필요 여부 검토 (web-artifacts-builder) |
| 🟢 | mockup ↔ 정본 미연결 (토큰 값 7중복) | mockup을 `tokens.css`에 물릴지, 복제 수용으로 갈지 **결정 필요** |
| 🟢 | SPA의 정본 준수 여부 미검증 | 다음 검증 시 `web/src/screens/` 인라인 스타일·하드코딩 hex 점검 |
| 🔵 | 여정 지표 목표치 전부 TBD | 관측 파이프라인 연결 시 목표 수치 확정 |

> 위험은 자동으로 고치지 않습니다. 사용자가 인지한 상태에서 대응 방식을 결정합니다.

## 변경 이력

| 시점 | 변경 내용 | 이전 상태 | 이후 상태 |
|------|-----------|-----------|-----------|
| 2026-06-18 | 사용자 여정 4개(J1~J4) 신규 작성, 가치 V1~V5 전부 연결 | 여정 0개 | 여정 4개 (mockup 0, 디자인 시스템 0) |
| 2026-06-18 | 여정 문서를 여정별 파일로 분리 (`user-journeys.md` → `user-journeys/` 폴더: README + J1~J4) | 단일 파일 | 폴더 + 5개 파일 (내용 동일, 연결 변화 없음) |
| 2026-06-27 | mockup 4종(index/new-session/restore/workspace) 발견·매핑 기록. mockup 인덱스(`mockups/README.md`) 신설, 여정 단계별 mockup 마커 갱신. restore/workspace의 J4 동시 접근 패널 제거(UI 비대상). | mockup 0(기록상), 인덱스 없음 | mockup 4 매핑됨, J4 비시각화 수용, 임의 스타일 4 mockup |
| 2026-07-01 | 상류(product-doc 사슬)에서 **세션 정체 = 인터랙티브 쉘** 확정(가치 V6 · `prd/shell-workload.md`). 이 사슬은 구조 변화 없음 — 여정 README의 "세션 정체 미확정" 항목 해소만 반영. | 세션 정체 미확정(일반화) | 세션 정체 확정(인터랙티브 쉘), 여정/mockup 정합 |
| 2026-07-01 | **J5(쉘에서 명령 실행·상태 이어감) 신설** — V6를 여정으로 연결. `workspace.html`을 J5-S1~S3·V6에 매핑, J5-S4는 전용 화면 없음(❌). 여정 4→5, 단계 14→18. | 여정 4, 단계 14, V6 여정 없음 | 여정 5, 단계 18, V6↔J5↔workspace 연결(부분) |
| 2026-07-01 (2) | `workspace.html` session shell 콘솔을 쉘 명령 기준으로 갱신. J5-S1~S3 ⚠️→✅, 단계 커버리지 ✅5→8·⚠️7→4. | J5-S1~S3 ⚠️(내용 갱신 필요) | J5-S1~S3 ✅ |
| 2026-07-03 | (참고: 구현 사슬) web SPA의 Workspace 콘솔이 `workspace.html` mockup과 정합하는 쉘 커맨드 루프로 구현됨(J5-S2·S3). 이 사슬의 구조는 변화 없음. | J5-S1~S3 mockup만 존재(SPA는 stub 콘솔) | SPA 콘솔 = mockup 정합(J5-S1~S3 구현) |
| 2026-08-08 | 상류에서 워크로드 타입을 가치로 올렸던 **V6·V7 삭제** — 가치 6→5. 이 사슬은 **참조 재연결만** 수행: J5를 V6→**V3**로, `workspace.html` 가치 매핑을 `V1, V3, V6`→`V1, V3`로 변경. 신규 위험으로 `claude-code` 타입 미시각화 등록. | 가치 6개, J5→V6 | 가치 5개, J5→V3 |
| 2026-08-08 (2) | 상류에서 **V8(목적에 맞는 작업 환경 선택) 신설**. 대응 여정·mockup이 없어 **시각화 없는 가치 1개** 발생. `claude-code` 미시각화 위험을 🟡→🔴 승격. | 가치 5개, 시각화 없는 가치 0 | 가치 6개, 시각화 없는 가치 1(V8) |
| 2026-08-08 (3) | **J6(작업 환경 선택 + 에이전트 프롬프트 루프) 신설**하여 V8 연결, mockup 확충으로 시각화 확보: `agent-workspace.html` 신규(J6-S2~S5), `new-session.html`에 타입·모델 선택 추가(J6-S1), `workspace.html`에 Shell state 패널 추가(J5-S4 ❌→✅), `index.html` 타입 태그·타입별 링크 분기, `restore.html` 콘솔 쉘 기준 갱신. 여정 5→6, 단계 18→23, mockup 4→5, ✅8→14·❌3→2. **🔴 위험 전부 해소**, 임의 스타일 mockup 4→5로 증가. | 가치 6, 여정 5, 단계 18, mockup 4, 시각화 없는 가치 1 | 가치 6, 여정 6, 단계 23, mockup 5, 시각화 없는 가치 0 |
| 2026-08-08 (4) | **디자인 시스템 셋업(진입점 5)**. 정본 위치를 **`web/src/design/`(코드)**으로 확정 — 사용자 결정: 코드가 정본, 문서는 얇게. 색인 `web/src/design/README.md` 신설, 루트 `README.md`에 정본 포인터 추가(발견 가능성 확보). `.claude/skills/`에 주입형 스킬 2종 설치. **가치/여정/mockup 매트릭스는 변화 없음** — 이번 판은 시각 언어의 소유권을 정한 작업이다. | 디자인 시스템 미정의, 주입형 스킬 미설치, 임의 스타일 mockup 5 | 디자인 시스템 정의됨(코드 정본), 주입형 스킬 2종 설치, 임의 스타일 mockup 5 (원인이 '시스템 부재'→'정본 미참조'로 변경) |
| 2026-08-09 (5) | **J6 live output UX 갱신** — 여정·mockup을 passive workspace SSE 자동 append, UTF-8 경계 cursor 재개, stale-cursor reset 전체 replay 계약에 맞추고 snapshot 시 Restore 전환과 연결 상태 전용 표시를 명시했다. | bounded polling/수동 Refresh로 응답 확인 | 자동 live output·무손실 재연결·reset 복구; 가치/여정/mockup 매트릭스 변화 없음 |
| 2026-08-30 (6) | **사용자 여정 문서 전면 재작성**. 단계 식별자를 순번(`J1-S1`)에서 슬러그(`STP-…`)로 전환하고 구↔신 매핑표를 여정 README에 남김. 8개 문서에 문서 정보·여정 정의(완료 기준)·터치포인트·생각·감정·페인포인트·분기·예외·측정 지표·변경 이력 섹션을 채우고, PRD 사양 중복 서술은 AC 참조로 위임. 구현에는 있으나 여정에 없던 **`JRN-manual-freeze`·`JRN-session-deletion` 신설**. 여정 6→8, 단계 23→29, 요약 ✅14→15 / ⚠️4→5 / ❌2→6 / ⚪3. mockup 파일·디자인 시스템은 변경 없음. | 여정 6, 단계 23, 순번 식별자, 섹션 3종 | 여정 8, 단계 29, 슬러그 식별자, 섹션 7종 완비 |
| 2026-09-03 (7) | **`approval-gated` 타입 하류 반영**. 상류에서 세 번째 워크로드 타입(AC-F1~F6)이 확정되어 여정 `JRN-approval-gated-work`(4단계)와 mockup `gated-workspace.html`을 신설하고, `new-session.html`에 세 번째 타입 카드를, `index.html`에 타입 태그·링크 분기를 더했다. `JRN-agent-prompt-loop`의 `STP-workload-choice`는 그대로 두고 선택지만 셋으로 넓혔다. **가치는 변하지 않았다** — V8 상속. 여정 8→9, 단계 29→33, mockup 5→6, ✅15→18 / ❌6→7. 늘어난 ❌ 1건은 승인 결정 화면이 레포 밖이라는 사실이 드러난 결과다. | 가치 6, 여정 8, 단계 29, mockup 5, 타입 2종 | 가치 6(변화 없음), 여정 9, 단계 33, mockup 6, 타입 3종, 시각화 없는 가치 0 유지 |
| 2026-09-03 (8) | **공급자 프록시의 보조 파드 이관 반영**. 상류에서 AC-F2의 열린 결정이 ①로 확정되어 `gated-workspace.html`의 Egress 패널·Workload 패널·파드 표기를 갱신했다. 여정·단계·mockup 수와 커버리지 합계는 **변하지 않는다** — 같은 화면의 내용 갱신이다. | 여정 9, 단계 33, mockup 6 / 프록시=사이드카 | 여정 9, 단계 33, mockup 6 / 프록시=보조 파드, Egress ⚠️ 해소 |
| 2026-09-03 (9) | **보조 파드를 헬퍼 파드 하나로 통합 반영**. 상류에서 MCP와 credential-proxy를 별개 파드가 아니라 한 헬퍼 파드의 컨테이너 둘로 확정하여 `gated-workspace.html`의 Egress·Workload 패널과 파드 표기를 갱신했다. 여정·단계·mockup 수와 커버리지 합계는 **변하지 않는다**. | 여정 9, 단계 33, mockup 6 / 세션당 파드 3 | 여정 9, 단계 33, mockup 6 / 세션당 파드 2 |
| 2026-09-03 (10) | **여정 mockup 층위 신설**. 여정 하나 = 페이지 하나 층위(`docs/journeys/`)를 열고 파일럿 2개(`JRN-session-creation` 3단계 · `JRN-shell-interaction` 4단계)를 클릭되는 프로토타입으로 신설했다. 마크업 규약(`journeys/README.md`), 여정→페이지 매핑 표(`mockups/README.md`), 집행 하네스(`tools/journey-prototype.test.mjs`)와 CI 게이트(`docs-journey-mockup`)를 함께 세웠다. `JRN-concurrent-access` 예외 등재에 재검토 시점을 채웠다. **화면 단위 mockup 6개와 그 커버리지 집계는 변하지 않는다** — 다른 층위다. | 여정 페이지 0, 규약·하네스 없음, 예외 등재에 재검토 시점 없음 | 여정 페이지 2/8, 규약·하네스·CI 게이트 있음, 예외 등재 규칙 8 충족 |
