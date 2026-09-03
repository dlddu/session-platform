# Session Pod Platform — Mockup 인덱스

브라우저 미리보기: <https://dlddu.github.io/session-platform/mockups/>
(문서 포털 진입점은 <https://dlddu.github.io/session-platform/>)

> 이 문서는 `가치 → 사용자 여정 → mockup ↔ 디자인 시스템` 사슬에서
> **mockup ↔ (여정 단계 · 가치 · 디자인 시스템)** 연결의 **단일 진실 원천**입니다.
> 상위 상태 추적은 [`../doc-structure-state.md`](../doc-structure-state.md),
> 여정 정의는 [`../user-journeys/`](../user-journeys/), 가치 정의는 [`../values.md`](../values.md)(V1~V5·V8) 참고.

> ⚠️ **파일명 주의**: `index.html`은 이 인덱스 문서가 아니라 **세션 목록(대시보드) mockup**입니다.
> mockup 매핑의 단일 소스는 이 `README.md`입니다.

> 📌 **식별자 전환 (2026-08-30)**: 여정·단계 식별자가 순번(`J1`, `J1-S1`)에서 슬러그(`JRN-…`, `STP-…`)로 바뀌었습니다.
> 구↔신 매핑표는 [`../user-journeys/README.md`](../user-journeys/README.md#구-식별자-매핑-2026-08-30-전환)에 있습니다.
> 아래 "마지막 갱신"의 과거 항목은 당시 기록이므로 옛 식별자를 그대로 둡니다.

---

## Mockup 목록과 매핑

| 파일 | 화면(`<title>`) | 시각화하는 여정 단계 | 달성 가치 | 디자인 시스템 |
|------|------|----------------------|-----------|----------------|
| [`index.html`](./index.html) | Sessions — control plane (세션 목록 대시보드) | **`STP-session-list`** (보조: `STP-step-away`·`STP-switch-away`·`STP-freeze-confirm`) | V4 (보조 V1·V5·V8) | 미연결 — 인라인 임의 토큰 |
| [`new-session.html`](./new-session.html) | New session (새 세션 생성) | **`STP-create-request`, `STP-workspace-entry`, `STP-workload-choice`**(타입 카드 3종) | V1, V5, **V8** | 미연결 — 인라인 임의 토큰 |
| [`workspace.html`](./workspace.html) | Session workspace (활성 `shell` 세션 작업) | **`STP-isolated-work`, `STP-shell-attach`, `STP-command-input`, `STP-output-read`, `STP-shell-state-carry`, `STP-freeze-now`** | V1, V3, V2 | 미연결 — 인라인 임의 토큰 |
| [`agent-workspace.html`](./agent-workspace.html) | Agent session workspace (활성 `claude-code` 세션 작업) | **`STP-prompt-submit`, `STP-response-watch`, `STP-conversation-carry`, `STP-agent-freeze-resume`, `STP-freeze-now`** | V8, V3, V2 (보조 V1) | 미연결 — 인라인 임의 토큰 |
| [`gated-workspace.html`](./gated-workspace.html) | Gated agent session workspace (활성 `approval-gated` 세션 작업 · 워크로드 파드 + 헬퍼 파드) | **`STP-gated-prompt-submit`, `STP-approval-wait`, `STP-gated-result`, `STP-freeze-now`** | V8, V3, V2 (보조 V1) | 미연결 — 인라인 임의 토큰 |
| [`restore.html`](./restore.html) | Resume from checkpoint (`shell` CRIU 복원) | **`STP-restore-resume`(`shell`)** (보조: `STP-auto-freeze`) | V3, V2 | 미연결 — 인라인 임의 토큰 |

> **타입별 작업 화면이 셋으로 갈립니다 (2026-08-08 → 2026-09-03)**: `workspace.html`은 `shell` 타입,
> `agent-workspace.html`은 `claude-code` 타입, `gated-workspace.html`은 `approval-gated` 타입 전용입니다.
> `index.html`의 세션 카드는 workloadType 태그를 달고 타입에 따라 세 화면 중 하나로 링크합니다 — 이것이
> `JRN-shell-interaction`(쉘) · `JRN-agent-prompt-loop`(에이전트) · `JRN-approval-gated-work`(승인 게이트)가
> 갈리는 지점입니다. 고르는 순간은 셋 다 `new-session.html`의 같은 카드 묶음에서 일어납니다.
>
> 참고: `workspace.html`의 session shell 콘솔은 2026-07-01 쉘 명령 기준으로 갱신됨(옛 key/value write 예시 → `$ ls` / `$ npm run build` 등 쉘 입력·출력, 복원 시 env·cwd 보존 시연). `JRN-shell-interaction`과 정합.
> `restore.html` 콘솔도 2026-08-08 같은 기준으로 갱신됨(옛 key/value read/write 예시 제거 — 이전 판의 🟡 위험 해소).

> 6개 mockup 모두 인라인 CSS 변수를 사용 → 전부 **임의 스타일 mockup(🟢)**.
> `agent-workspace.html`은 `workspace.html`의, `gated-workspace.html`은 `agent-workspace.html`의 인라인 토큰을
> 복사해 만들었으므로 겉보기 일관성은 있으나, **시스템에 연결된 것은 아닙니다.**
>
> **2026-08-08 (4) 갱신 — 디자인 시스템 정본이 생겼습니다**: [`web/src/design/`](../../web/src/design/README.md)
> (토큰·프리미티브 `tokens.css`, 컴포넌트·패턴 `web/src/app/shell.css`). 코드가 정본이고 문서는 색인입니다.
> 다만 mockup 6개는 여전히 그 정본을 참조하지 않고 같은 값을 각자 인라인으로 갖고 있습니다 —
> 값은 일치하지만 **연결은 아니므로 "미연결" 상태는 유지**됩니다.
> 토큰 하나를 바꾸려면 `tokens.css` + mockup 6개 = **7곳**을 고쳐야 하며, 이 중복이 남은 🟢 위험의 실체입니다.
> `gated-workspace.html` 신설로 중복이 한 곳 더 늘었습니다.
> 방향 규칙: **코드를 먼저 고치고 mockup이 따라옵니다.**

---

## 화면 ↔ mockup 매핑 (SPA 구현 대조)

> 위의 표가 **mockup ↔ 여정 단계**를 잇는다면, 이 표는 **mockup ↔ SPA 구현 화면**을 잇습니다.
> 정합성 모델 `tbm_session-platform-mockup-render`가 "구현된 화면이 대응 목업과 시각적으로
> 일치하는가"를 판정하는 **기계 판독 가능한 매핑의 단일 소스**이며,
> `scripts/check-render-fidelity.py`(CI `render-fidelity` 잡)가 아래 표를 코드와
> **양방향 집합 동등성**으로 강제합니다 — 표에 없는 구현 파일도, 코드에 없는 표 행도 실패합니다.

매핑된 구현 파일 = **11**

| 구현 파일 | 종류 | 대응 mockup | 근거 | 상태 |
|-----------|------|-------------|------|------|
| `web/src/screens/Sessions.tsx` | screen | `docs/mockups/index.html` | 세션 목록 대시보드. 상태별 카드 + workloadType 태그 | ✅ 매핑 |
| `web/src/screens/NewSession.tsx` | screen | `docs/mockups/new-session.html` | 생성 모달 · 워크로드 타입 카드 · 프로비저닝 단계 뷰 | ✅ 매핑 |
| `web/src/screens/Workspace.tsx` | screen | `docs/mockups/workspace.html`, `docs/mockups/agent-workspace.html` | 한 화면이 `workloadType`에 따라 둘로 갈린다(`shell` / `claude-code`). `approval-gated`는 구현이 없어 아래 표 참조 | ✅ 매핑 (1:N) |
| `web/src/screens/Restore.tsx` | screen | `docs/mockups/restore.html` | 체크포인트 복원 화면 | ✅ 매핑 |
| `web/src/app/AppShell.tsx` | component | `docs/mockups/index.html` | 뷰포트 + 64px 하단 레일. 6개 mockup이 공유하는 셸이라 정본 출처를 `index.html`로 고정 | ✅ 매핑 |
| `web/src/app/SessionCard.tsx` | component | `docs/mockups/index.html` | 세션 카드(vitals · freeze ring · snapshot body) | ✅ 매핑 |
| `web/src/app/StateBadge.tsx` | component | `docs/mockups/index.html` | `session.State` 배지 | ✅ 매핑 |
| `web/src/app/Toast.tsx` | component | `docs/mockups/index.html` | `toast-wrap` / `toast`. 6개 mockup 공통이라 `index.html`로 고정 | ✅ 매핑 |
| `web/src/app/icons.tsx` | component | `docs/mockups/index.html` | 인라인 SVG 아이콘 1:1 이식(이 파일의 기존 주석이 이 규약의 선례) | ✅ 매핑 |
| `web/src/app/shell.css` | style | `docs/mockups/index.html` | 셸·공용 컴포넌트 CSS. `tokens.css`·`icons.tsx`와 같은 규약으로 `index.html`을 정본 출처로 고정하고, 화면별 패널 대조는 아래 「패널 구조 차집합」이 담당 | ✅ 매핑 |
| `web/src/app/DeleteSessionDialog.tsx` | component | — | 삭제 확인 대화상자. **SPA에만 있고 대응 mockup이 없다**(위 「미시각화 단계 메모」의 `STP-delete-confirm` ❌와 같은 사실) | ❌ 대응 mockup 없음 |

각 구현 파일은 헤더에 `mockup: <경로>` 선언 주석을 갖습니다(대응이 없으면 `mockup: none — <사유>`).
표와 선언이 어긋나면 게이트가 실패하므로, **둘 중 하나만 고치는 것은 불가능**합니다.

> 스캔 스코프는 정합성 모델의 as-is와 같게 `web/src/screens` + `web/src/app`이고, 대조 단위가 되는
> `.tsx`·`.css`만 등재합니다. `web/src/app/sessionRoutes.ts`는 경로 계산 헬퍼로 렌더 산출물이 없어
> 제외합니다. `web/src/design/`은 이탈의 **기준점(정본)**이라 대조 대상이 아닙니다.

### 구현이 없는 mockup

아직 대응 화면이 없는 mockup은 이 모델의 drift가 **아닙니다**(모델 정의: "아직 구현되지 않은
화면의 부재는 drift가 아니다"). 다만 구현이 착지하는 순간 매핑이 낡으므로, **구현 신호**를 함께
등재해 게이트가 그 시점을 잡아냅니다 — 신호가 `web/src/screens`·`web/src/app`에서 1건이라도
발견되면 게이트는 "매핑 표를 갱신하라"며 실패합니다.

| mockup | 구현 신호 (`git grep -F`) | 비고 |
|--------|---------------------------|------|
| `docs/mockups/gated-workspace.html` | `approval-gated` | AC-F1~F6 구현이 `tbm_session-platform-docs-impl`에 걸려 있다. 착지하면 `Workspace.tsx`의 1:N 대응에 이 파일이 합류한다 |

---

## 패널 구조 차집합 (구현 대조)

mockup의 `<div class="panel">` 안 `<h4>` 제목과, 구현의 `className="panel…"` 직후 `<h4>` 텍스트를
뽑아 **쌍별로** 비교한 실측 원장입니다. 게이트가 매번 다시 계산해 이 표와 대조하므로 손으로
맞출 수 없습니다.

패널 차집합 상한 = **10**

| 구현 파일 | mockup | 이 쌍의 구현 패널 | mockup-only (구현에 없음) | impl-only (mockup에 없음) |
|-----------|--------|-------------------|---------------------------|---------------------------|
| `web/src/screens/Workspace.tsx` | `docs/mockups/workspace.html` | Actions | Vitals · Shell state · Lifecycle | Actions |
| `web/src/screens/Workspace.tsx` | `docs/mockups/agent-workspace.html` | Workload · Actions | Vitals · Conversation · Lifecycle | Actions |
| `web/src/screens/Restore.tsx` | `docs/mockups/restore.html` | — | Vitals · Lifecycle | — |

`Workspace.tsx`는 한 파일이 두 mockup을 그리므로 **쌍마다 그 변형이 실제로 그리는 패널만** 선언합니다
(`Workload` 패널은 `isAgent`일 때만 렌더). 게이트는 쌍별 선언의 **합집합이 파일에서 추출한 패널
전체와 같은지**까지 확인하므로, 한 쌍에서 빼는 방식으로 차집합을 줄일 수 없습니다.

**읽는 법**: `Actions`가 두 쌍 모두에서 impl-only인 것은 같은 패널 하나가 두 대조에 나타난 것이지
패널이 둘이라는 뜻이 아닙니다. 상한 10은 (3+1)+(3+1)+(2+0)의 합입니다.

> **이 표가 세는 것은 패널 층위까지입니다.** 패널 **내부**의 카피·수치(예: `Idle timer` ·
> `Auto-freeze at` · `Pod uptime` · `process tree` · `Resuming from checkpoint` · `Session workspace`,
> 역방향으로 `Delete session` · `Switch (AC-C4)`)는 아직 대조하지 않습니다 — 후속 작업이며,
> `Vitals`(CPU/메모리)·`Pod uptime`처럼 **실데이터 소스가 API에 있는지**부터 확인해야 방향
> (구현을 늘릴지 · mockup을 줄일지)이 정해지는 항목이 섞여 있습니다.

---

## 여정 단계별 시각화 커버리지

표기: ✅ 전용 화면 있음 · ⚠️ 부분/암시(전용 화면 없음) · ❌ 없음 · ⚪ 의도적 비시각화

### `JRN-session-creation`

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-create-request` 세션 만들기 | ✅ | `new-session.html` (생성 플로우) |
| `STP-workspace-entry` 작업 공간 진입 | ✅ | `new-session.html` ("Schedule dedicated pod", 1:1 격리) |
| `STP-isolated-work` 격리된 작업 | ✅ | `workspace.html` (active 세션 read/write, 전용 pod 격리) |

### `JRN-idle-resume`

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-step-away` 자리 비움 | ⚠️ | `index.html` 목록의 idle 상태 · `workspace.html` lifecycle (전용 화면 없음) |
| `STP-auto-freeze` 자동 동결 | ⚠️ | `restore.html` lifecycle "auto-freeze 60min" (동결 진행 전용 화면 없음) |
| `STP-reaccess` 재접근 | ❌ | 전용 화면 없음 (복원의 트리거) |
| `STP-restore-resume` 복원 후 재개 | ✅ | `restore.html`(`shell` CRIU) + `agent-workspace.html`(`claude-code` archive) |

### `JRN-multi-session-switch`

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-session-list` 세션 목록 확인 | ✅ | `index.html` (상태별 목록 + workloadType 태그) |
| `STP-switch-away` 다른 세션으로 이동 | ⚠️ | `index.html` ↔ `workspace.html`/`agent-workspace.html` 네비게이션으로 암시 |
| `STP-target-activation` 대상 세션 활성화 | ⚠️ | `workspace.html`이 snapshot 세션이면 `restore.html`로 전환 |
| `STP-switch-back` 원래 세션으로 복귀 | ❌ | 전용 화면 없음 |

### `JRN-concurrent-access`

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-parallel-clients` 다중 클라이언트 접속 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| `STP-collision` 요청 충돌 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| `STP-consistent-result` 일관된 결과 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |

### `JRN-shell-interaction`

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-shell-attach` 쉘 연결 | ✅ | `workspace.html` session shell — 쉘 attach·프롬프트 렌더 |
| `STP-command-input` 명령 입력 | ✅ | `workspace.html` input-row `$` 프롬프트 + 콘솔 쉘 명령(`$ ls`, `$ npm run build`) |
| `STP-output-read` 출력 확인 | ✅ | `workspace.html` term — 쉘 stdout 렌더(빌드 로그·상태 보존 문구) |
| `STP-shell-state-carry` 쉘 상태 축적 | ✅ | `workspace.html` **Shell state 패널** — cwd·env·shell var·background job (2026-08-08 신규) |

### `JRN-agent-prompt-loop`

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-workload-choice` 작업 환경 선택 | ✅ | `new-session.html` **workload type 카드**(shell/claude-code) + model 선택 + 불변성 안내 |
| `STP-prompt-submit` 프롬프트 전송 | ✅ | `agent-workspace.html` input-row `▸` 프롬프트 + passive output 연결 표시 |
| `STP-response-watch` 응답 확인 | ✅ | `agent-workspace.html` term — 실행 중 자동 append + 재연결 가능한 커서 표기 |
| `STP-conversation-carry` 대화 이어짐 | ✅ | `agent-workspace.html` **Conversation 패널** — 턴 이력·작업 디렉터리·생성 파일 |
| `STP-agent-freeze-resume` 동결·복원 건너 이어감 | ✅ | `agent-workspace.html` snapshot 상태 콘솔 — 아카이브 기반, 복원 후 옛 문맥으로 응답 |

### `JRN-approval-gated-work` (2026-09-03 신설)

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-gated-prompt-submit` 게이트가 걸린 채 맡김 | ✅ | `gated-workspace.html` input-row + "outbound calls need your approval" 표시 · Egress 패널 |
| `STP-approval-wait` 승인 대기가 보임 | ✅ | `gated-workspace.html` 콘솔의 **awaiting approval 배지** + Approvals 패널(대기 항목·요청 식별자) |
| `STP-approval-decide` 승인·거절 결정 | ❌ | **전용 화면 없음 — 결정이 일어나는 승인 게이트웨이 화면이 이 레포 밖**이다. 세션 쪽에는 요청 식별자만 남아 두 화면을 눈으로 잇는다. ⚪(의도적 비시각화)로 볼지 ❌로 볼지 결정 대기 (`../user-journeys/README.md` 열린 결정) |
| `STP-gated-result` 승인 결과 위에서 이어감 | ✅ | `gated-workspace.html` 콘솔의 approved/rejected verdict + Conversation 패널의 shared volume 행 |

### `JRN-manual-freeze` (2026-08-30 신설)

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-freeze-decision` 접어두기 결정 | ❌ | 전용 화면 없음 — 접기/지우기를 한자리에서 비교하는 지점이 mockup에 없다 |
| `STP-freeze-now` 지금 접기 | ✅ | `workspace.html` "Freeze now" · `agent-workspace.html`·`gated-workspace.html` "Archive now" |
| `STP-freeze-confirm` 접힘 확인 | ⚠️ | `index.html`의 `snapshot` 배지로 암시 (완료 알림·되살릴 수 있음 문구 없음) |

### `JRN-session-deletion` (2026-08-30 신설)

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| `STP-delete-intent` 정리 대상 선택 | ❌ | `index.html` 카드에 삭제 진입점이 없다 (SPA에는 있음) |
| `STP-delete-confirm` 삭제 확인 | ❌ | 삭제 확인 대화상자 mockup 없음 (SPA `DeleteSessionDialog`만 존재) |
| `STP-delete-settled` 삭제 완료 확인 | ❌ | 전용 화면 없음 |

요약: ✅ 18 · ⚠️ 5 · ❌ 7 · ⚪ 3 (총 33단계 / 9여정)

---

## 미시각화 단계 메모

- **🟠 신설 여정 미시각화 (2026-08-30)**: `JRN-session-deletion` 3단계 전부와 `STP-freeze-decision`이 ❌입니다.
  두 흐름은 **SPA에는 구현돼 있으나 mockup이 없습니다**(`web/src/app/DeleteSessionDialog.tsx`,
  `web/src/screens/Workspace.tsx`의 Freeze/Archive now). mockup을 추가할지, "구현이 앞선 흐름은 mockup 생략"으로
  수용할지 **결정 대기 중**입니다.
- **🟡 시각화 누락**: `STP-reaccess`(재접근), `STP-switch-back`(원래 세션 복귀). 의도적 제외인지 화면이 필요한지 **결정 대기 중**.
- **🟠 다른 제품에 걸친 단계 (2026-09-03)**: `STP-approval-decide`는 **외부 승인 게이트웨이의 화면**에서 일어납니다.
  이 레포가 그릴 대상이 아니므로 `JRN-concurrent-access`처럼 ⚪(의도적 비시각화)로 수용할지, 아니면
  세션 쪽에 승인 요청을 요약해 보여주는 최소 화면을 두어 ✅로 만들지 **결정 대기 중**입니다.
  현재는 보수적으로 ❌로 셉니다.
- **🟡 부분 시각화**: `STP-step-away`, `STP-auto-freeze`, `STP-switch-away`, `STP-target-activation`, `STP-freeze-confirm`
  — 상태 표시·네비게이션·전환 동작으로 암시되나 단계 전용 화면은 없음.
- **✅ 해결됨 (2026-08-08) — `STP-shell-state-carry`**: `workspace.html`에 Shell state 패널이 추가되어
  "축적된 쉘 상태가 다음 명령으로 이어지고 그대로 동결된다"를 렌더. 이전 ❌ → ✅.
- **✅ 해결됨 (2026-09-03) — `JRN-approval-gated-work` 4단계 중 3단계**: `approval-gated` 타입(AC-F1~F6)이
  상류에 확정되면서 `gated-workspace.html`을 신설하고 `new-session.html`에 세 번째 타입 카드를 더해
  제출·대기·결과가 화면을 얻었습니다. 남은 ❌ 1건은 위의 "다른 제품에 걸친 단계"입니다.
- **✅ 해결됨 (2026-08-08) — `JRN-agent-prompt-loop` 전 단계**: `claude-code` 워크로드 타입(AC-E1~E6)과 가치 V8이
  상류에 확정된 뒤 대응 화면이 없던 상태(🔴 위험)가 `new-session.html` 타입 선택 + `agent-workspace.html` 신설로 해소.
- **✅ 해결됨 (2026-07-01) — 쉘 루프 mockup 내용**: `workspace.html`의 session shell 콘솔이 옛 key/value write 예시에서
  **쉘 명령 입력·stdout 출력**으로 갱신되어 `JRN-shell-interaction`과 정합.
- **⚪ `JRN-concurrent-access` 전 단계 (의도적 비시각화)**: 이 여정이 지키는 것은 화면이 아니라 결과의 일관성이라
  UI에 그릴 대상이 아니다. 이전에 `restore.html`·`workspace.html`에 있던 **"Attached clients(operator + automation) +
  동시 atomic 전이" 패널**은 UI 구현 대상이 아니라는 판단으로 **2026-06-27 제거**됨.
  따라서 이는 *누락이 아니라 의도된 상태*다.

---

## mockup 내용의 잠정성

- **명시 model 예시 (AC-E6)**: `new-session.html`의 `model-a`/`model-b`는 optional
  `models` soft catalog가 있는 상태를 그린 자리표시자다. 실제 UI는 no-store config API의 concrete default를
  `<model> (platform default)` 한 항목으로 합치고 ordered catalog를 사용하며, missing/empty/`[]`이면 default 이름을
  안내하는 free-text 입력으로 돌아간다. catalog는 API allowlist가 아니다. `platform-default`는 특정 공급자 버전을
  API에 고정하지 않는 별칭이며, 새/복원 pod 또는 container restart 때 optional Secret `model`을 우선하고
  missing/empty면 CLI 기본 선택에 위임한다. 실행 중 컨테이너는 Secret 변경으로 즉시 바뀌지 않는다.
- **대화 재개 (AC-E4)**: 실제 계약은 첫 성공 실행 뒤 `--continue`와 세션별 고정 HOME/workdir이다.
  mockup은 CLI flag 자체보다 사용자가 보는 연속 대화 결과를 표현한다.
- **승인 요청 예시 (AC-F3)**: `gated-workspace.html`의 `req-2c81`·`req-4f2a`, `rates.vendor.example`,
  `/shared` 경로는 전부 자리표시자다. 실제 외부 식별자는 `{세션ID}:{요청ID}` 규칙만 계약이고, 도구 이름·
  마운트 경로·대기 시간 표기는 구현 시 확정된다. 결정 UI는 이 레포 밖(승인 게이트웨이)이다.
- **보조 파드 표기 (AC-F4)**: mockup의 `helper/…` 파드 이름과 Egress 허용 목록 표기는 예시다.
  다만 **워크로드 파드와 헬퍼 파드(MCP·credential-proxy 컨테이너)가 함께 뜨고 함께 회수된다**는 사실,
  **자격 증명이 컨테이너 단위로 갈린다**는 사실, Egress 패널이 말하는 **"허용 목록에 외부 origin이 하나도
  없다"**는 AC-F2/F4/F6의 실제 계약이다.
  (2026-09-03 이전 판은 이 자리에 "공급자 HTTPS는 아직 허용 목록에 있다"는 경고를 달고 있었다 —
  공급자 프록시를 워크로드 파드 밖으로 내면서 해소되었다.)

---

## 마지막 갱신

- **2026-09-03 (9)** — **보조 파드를 헬퍼 파드 하나로 통합**: 상류에서 MCP와 credential-proxy를 별개 파드가 아니라 **한 헬퍼 파드의 컨테이너 둘**로 확정하여(AC-F4) Egress 패널을 헬퍼 파드 한 항목 + 컨테이너 두 줄로 접고, 자격 증명 행을 "mcp container / proxy container"로, 파드 표기를 둘로 줄였다. **mockup 수·커버리지·요약은 변하지 않는다.** 새 토큰 없이 기존 인라인 클래스만 재사용했다.
- **2026-09-03 (8)** — **공급자 프록시를 보조 파드로 이관**: AC-F2의 열린 결정이 ①(프록시를 사이드카에서 보조 파드로 분리)으로 확정되어 `gated-workspace.html`의 Egress 패널에서 경고(⚠️ 공급자 HTTPS가 허용 목록에 있음)를 걷어내고 허용 항목에 프록시 파드를, 차단 항목에 "공급자 API 직접"을 넣었다. Workload 패널의 자격 증명 행을 파드별 배치(게이트웨이 키=MCP · 공급자 토큰=프록시 · 워크로드 파드=없음)로 바꾸고, 파드 표기를 셋으로 확장했다. **mockup 수·단계 커버리지·요약은 변하지 않는다** — 같은 화면의 내용 갱신이다. 새 토큰·컴포넌트 없이 기존 인라인 클래스(`.eg`·`.ctx-note`·`.kv`)만 재사용했다.
- **2026-09-03 (7)** — **`approval-gated` 타입 신설에 따른 mockup 확충**: `gated-workspace.html` 신규 작성(승인 대기 배지·승인/거절 verdict·Approvals 패널·Egress 허용 목록 패널·공유 볼륨 행), `new-session.html`에 세 번째 타입 카드 추가(`STP-workload-choice`), `index.html` 카드의 타입 태그·링크 분기를 3종으로 확장. 여정 8→9, 단계 29→33, mockup 5→6, 요약 ✅15→18 / ❌6→7. 신규 여정의 ❌ 1건(`STP-approval-decide`)은 결정 화면이 이 레포 밖이라는 사실이 드러난 결과다. 임의 스타일 mockup 5→6(토큰 중복 6곳→7곳).
- **2026-08-30 (6)** — **여정 문서 전면 재작성에 따른 매핑 갱신**: 단계 식별자가 순번에서 슬러그로 전환되어 이 문서의 매핑을 여정별 표로 재구성했다. 여정 6→8(`JRN-manual-freeze`·`JRN-session-deletion` 신설), 단계 23→29, 요약 ✅14→15 / ⚠️4→5 / ❌2→6 / ⚪3. **mockup 파일 자체는 변경 없음** — 늘어난 ❌는 새로 문서화된 흐름(수동 동결 결정·세션 삭제)이 mockup에 없다는 사실이 드러난 결과다.
- **2026-08-09 (5)** — **J6 live output UX 갱신**: `agent-workspace.html`이 passive SSE 연결 상태와 실행 중 자동 append, `nextOffset` cursor 재개, snapshot Restore 전환을 표현하도록 갱신했다. 서버 run/queue 수는 추정하지 않으며 mockup 수·커버리지 합계는 변하지 않았다.
- **2026-08-08 (3)** — **J6 신설에 따른 mockup 확충**: `agent-workspace.html` 신규 작성(J6-S2~S5), `new-session.html`에 workload type·model 선택 추가(J6-S1 · V8), `workspace.html`에 Shell state 패널 추가(J5-S4 ❌→✅), `index.html` 카드에 workloadType 태그·타입별 링크 분기 추가, `restore.html` 콘솔을 쉘 기준으로 갱신(옛 key/value 예시 제거). mockup 4→5, 단계 총계 18→23, 요약 ✅8→14 / ❌3→2. **V8 시각화 부재(🔴) 해소.**
- **2026-07-01 (2)** — `workspace.html` session shell 콘솔을 쉘 명령 기준으로 갱신(key/value write 예시 제거 → `$` 프롬프트·쉘 stdout·복원 시 env/cwd 보존 시연). J5-S1~S3 ⚠️→✅, 요약 ✅8/⚠️4/❌3/⚪3. 디자인/레이아웃·임의 스타일 상태는 변화 없음.
- **2026-07-01** — 인터랙티브 쉘 확정(당시 가치 V6, 2026-08-08 삭제)에 따른 J5 신설 반영. `workspace.html`을 J5-S1~S3에 매핑(당시 콘솔 내용 갱신 필요=⚠️), J5-S4는 전용 화면 없음(❌). 단계 총계 14→18.
- **2026-06-27** — mockup 4종(index/new-session/restore/workspace) 매핑 최초 기록(인덱스 신설). restore/workspace의 J4 동시 접근 패널 제거 반영.
