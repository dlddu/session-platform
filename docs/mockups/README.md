# Session Pod Platform — Mockup 인덱스

브라우저 미리보기: <https://dlddu.github.io/session-platform/mockups/>
(문서 포털 진입점은 <https://dlddu.github.io/session-platform/>)

> 이 문서는 `가치 → 사용자 여정 → mockup ↔ 디자인 시스템` 사슬에서
> **mockup ↔ (여정 단계 · 가치 · 디자인 시스템)** 연결의 **단일 진실 원천**입니다.
> 상위 상태 추적은 [`../doc-structure-state.md`](../doc-structure-state.md),
> 여정 정의는 [`../user-journeys/`](../user-journeys/), 가치 정의는 [`../values.md`](../values.md)(V1~V5·V8) 참고.

> ⚠️ **파일명 주의**: `index.html`은 이 인덱스 문서가 아니라 **세션 목록(대시보드) mockup**입니다.
> mockup 매핑의 단일 소스는 이 `README.md`입니다.

---

## Mockup 목록과 매핑

| 파일 | 화면(`<title>`) | 시각화하는 여정 단계 | 달성 가치 | 디자인 시스템 |
|------|------|----------------------|-----------|----------------|
| [`index.html`](./index.html) | Sessions — control plane (세션 목록 대시보드) | **J3-S1** | V4 (보조 V1·V5·V8) | 미연결 — 인라인 임의 토큰 |
| [`new-session.html`](./new-session.html) | New session (새 세션 생성) | **J1-S1, J1-S2, J6-S1** | V1, V5, **V8** | 미연결 — 인라인 임의 토큰 |
| [`workspace.html`](./workspace.html) | Session workspace (활성 `shell` 세션 작업) | **J1-S3, J5-S1·S2·S3·S4** | V1, V3 | 미연결 — 인라인 임의 토큰 |
| [`agent-workspace.html`](./agent-workspace.html) | Agent session workspace (활성 `claude-code` 세션 작업) | **J6-S2·S3·S4·S5** | V8, V3, V2 (보조 V1) | 미연결 — 인라인 임의 토큰 |
| [`restore.html`](./restore.html) | Resume from checkpoint (`shell` CRIU 복원) | **J2-S4 (`shell`)** | V3, V2 | 미연결 — 인라인 임의 토큰 |

> **타입별 작업 화면이 둘로 갈립니다 (2026-08-08)**: `workspace.html`은 `shell` 타입, `agent-workspace.html`은 `claude-code` 타입 전용입니다.
> `index.html`의 세션 카드는 workloadType 태그를 달고 타입에 따라 두 화면 중 하나로 링크합니다 — 이것이 여정 J5(쉘)와 J6(에이전트)가 갈리는 지점입니다.
>
> 참고: `workspace.html`의 session shell 콘솔은 2026-07-01 쉘 명령 기준으로 갱신됨(옛 key/value write 예시 → `$ ls` / `$ npm run build` 등 쉘 입력·출력, 복원 시 env·cwd 보존 시연). J5와 정합.
> `restore.html` 콘솔도 2026-08-08 같은 기준으로 갱신됨(옛 key/value read/write 예시 제거 — 이전 판의 🟡 위험 해소).

> 5개 mockup 모두 인라인 CSS 변수를 사용 → 전부 **임의 스타일 mockup(🟢)**.
> `agent-workspace.html`은 `workspace.html`의 인라인 토큰을 복사해 만들었으므로 겉보기 일관성은 있으나, **시스템에 연결된 것은 아닙니다.**
>
> **2026-08-08 (4) 갱신 — 디자인 시스템 정본이 생겼습니다**: [`web/src/design/`](../../web/src/design/README.md)
> (토큰·프리미티브 `tokens.css`, 컴포넌트·패턴 `web/src/app/shell.css`). 코드가 정본이고 문서는 색인입니다.
> 다만 mockup 5개는 여전히 그 정본을 참조하지 않고 같은 값을 각자 인라인으로 갖고 있습니다 —
> 값은 일치하지만 **연결은 아니므로 "미연결" 상태는 유지**됩니다.
> 토큰 하나를 바꾸려면 `tokens.css` + mockup 5개 = **6곳**을 고쳐야 하며, 이 중복이 남은 🟢 위험의 실체입니다.
> 방향 규칙: **코드를 먼저 고치고 mockup이 따라옵니다.**

---

## 여정 단계별 시각화 커버리지

표기: ✅ 전용 화면 있음 · ⚠️ 부분/암시(전용 화면 없음) · ❌ 없음 · ⚪ 의도적 비시각화

| 단계 | 시각화 | mockup · 근거 |
|------|:---:|------|
| J1-S1 세션 생성 요청 | ✅ | `new-session.html` (생성 플로우) |
| J1-S2 전용 pod 기동 | ✅ | `new-session.html` ("Schedule dedicated pod", 1:1 격리) |
| J1-S3 격리된 작업 | ✅ | `workspace.html` (active 세션 read/write, 전용 pod 격리) |
| J2-S1 이탈 → idle | ⚠️ | `index.html` 목록의 idle 상태 · `workspace.html` lifecycle (전용 화면 없음) |
| J2-S2 60분 동결 → snapshot | ⚠️ | `restore.html` lifecycle "auto-freeze 60min" · "Freeze now" (동결 진행 전용 화면 없음) |
| J2-S3 재접근 | ❌ | 전용 화면 없음 (복원의 트리거) |
| J2-S4 복원 후 재개 | ✅ | `restore.html` (`shell` CRIU) + `agent-workspace.html` (`claude-code` archive) |
| J3-S1 세션 목록 확인 | ✅ | `index.html` (active/idle/snapshot 상태별 목록 + workloadType 태그) |
| J3-S2 세션 B로 전환 | ⚠️ | `index.html` ↔ `workspace.html`/`agent-workspace.html` 네비게이션으로 암시 |
| J3-S3 상태에 따른 활성화 | ⚠️ | `workspace.html`이 snapshot 세션이면 `restore.html`로 전환 |
| J3-S4 다시 A로 복귀 | ❌ | 전용 화면 없음 |
| J4-S1 동시 요청 발생 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| J4-S2 atomic 전이로 단일화 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| J4-S3 일관된 결과 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| J5-S1 쉘 연결 | ✅ | `workspace.html` session shell — 쉘 attach·프롬프트 렌더 |
| J5-S2 명령 입력 | ✅ | `workspace.html` input-row `$` 프롬프트 + 콘솔 쉘 명령(`$ ls`, `$ npm run build`) |
| J5-S3 출력 확인 | ✅ | `workspace.html` term — 쉘 stdout 렌더(빌드 로그·상태 보존 문구) |
| J5-S4 쉘 상태 축적 | ✅ | `workspace.html` **Shell state 패널** — cwd·env·shell var·background job과 "이것이 체크포인트 대상"임을 렌더 (2026-08-08 신규) |
| J6-S1 작업 환경 선택 | ✅ | `new-session.html` **workload type 카드**(shell/claude-code) + model 선택 + 타입·모델 불변성 안내 |
| J6-S2 프롬프트 전송 | ✅ | `agent-workspace.html` input-row `▸` 프롬프트 + passive output 연결 표시(서버 running/queued 추정 아님) |
| J6-S3 응답 확인 | ✅ | `agent-workspace.html` term — 실행 중 에이전트 응답 자동 append + 재연결 가능한 `nextOffset` 커서 표기 |
| J6-S4 대화가 이어짐 | ✅ | `agent-workspace.html` **Conversation 패널** — 턴 이력·작업 디렉터리·생성 파일, "턴 N이 1~N-1을 본다" |
| J6-S5 동결·복원 건너 이어감 | ✅ | `agent-workspace.html` snapshot 상태 콘솔 — CRIU 아님·아카이브 기반, 복원 후 옛 문맥으로 응답 |

요약: ✅ 14 · ⚠️ 4 · ❌ 2 · ⚪ 3 (총 23단계)

---

## 미시각화 단계 메모

- **🟡 시각화 누락(전용 화면 필요 가능)**: `J2-S3`(재접근), `J3-S4`(A로 복귀). 의도적 제외인지, 화면이 필요한지 **결정 대기 중**.
- **🟡 부분 시각화**: `J2-S1`, `J2-S2`, `J3-S2`, `J3-S3` — 상태 표시·네비게이션·전환 동작으로 암시되나 단계 전용 화면은 없음.
- **✅ 해결됨 (2026-08-08) — J5-S4**: `workspace.html`에 Shell state 패널이 추가되어 "축적된 쉘 상태가 다음 명령으로 이어지고 그대로 동결된다"를 렌더. 이전 ❌ → ✅.
- **✅ 해결됨 (2026-08-08) — J6 전 단계**: `claude-code` 워크로드 타입(AC-E1~E6)과 가치 V8이 상류에 확정된 뒤 대응 화면이 없던 상태(🔴 위험)가 `new-session.html` 타입 선택 + `agent-workspace.html` 신설로 해소.
- **✅ 해결됨 (2026-07-01) — J5-S1~S3 mockup 내용**: `workspace.html`의 session shell 콘솔이 옛 key/value write 예시에서 **쉘 명령 입력·stdout 출력**으로 갱신되어 J5와 정합.
- **⚪ J4-S1~S3 (의도적 비시각화)**: J4는 동시 접근을 ConfigMap(resourceVersion CAS) + Lease 기반 atomic 전이로 푸는 **백엔드 동시성 여정**으로, UI에 그릴 화면이 아니다.
  이전에 `restore.html`·`workspace.html`에 들어 있던 **"Attached clients(operator + automation) + 동시 atomic 전이" 패널**은
  UI 구현 대상이 아니라는 판단으로 **2026-06-27 제거**됨. 따라서 J4는 시각화 대상이 아니며, 이는 *누락이 아니라 의도된 상태*다.

---

## mockup 내용의 잠정성

- **명시 model 예시 (AC-E6)**: `new-session.html`의 `model-a`/`model-b`는 optional
  `models` soft catalog가 있는 상태를 그린 자리표시자다. 실제 UI는 no-store config API의 concrete default를 `<model> (platform default)` 한 항목으로 합치고 ordered catalog를 사용하며, missing/empty/`[]`이면 default 이름을 안내하는 free-text 입력으로 돌아간다. catalog는
  API allowlist가 아니다. `platform-default`는 특정 공급자 버전을 API에 고정하지 않는
  별칭이며, 새/복원 pod 또는 container restart 때 optional Secret `model`을 우선하고
  missing/empty면 CLI 기본 선택에 위임한다. 실행 중 컨테이너는 Secret 변경으로 즉시
  바뀌지 않는다.
- **대화 재개 (AC-E4)**: 실제 계약은 첫 성공 실행 뒤 `--continue`와 세션별 고정 HOME/workdir이다.
  mockup은 CLI flag 자체보다 사용자가 보는 연속 대화 결과를 표현한다.

---

## 마지막 갱신

- **2026-08-09 (5)** — **J6 live output UX 갱신**: `agent-workspace.html`이 passive SSE 연결 상태와 실행 중 자동 append, `nextOffset` cursor 재개, snapshot Restore 전환을 표현하도록 갱신했다. 서버 run/queue 수는 추정하지 않으며 mockup 수·커버리지 합계는 변하지 않았다.
- **2026-08-08 (3)** — **J6 신설에 따른 mockup 확충**: `agent-workspace.html` 신규 작성(J6-S2~S5), `new-session.html`에 workload type·model 선택 추가(J6-S1 · V8), `workspace.html`에 Shell state 패널 추가(J5-S4 ❌→✅), `index.html` 카드에 workloadType 태그·타입별 링크 분기 추가, `restore.html` 콘솔을 쉘 기준으로 갱신(옛 key/value 예시 제거). mockup 4→5, 단계 총계 18→23, 요약 ✅8→14 / ❌3→2. **V8 시각화 부재(🔴) 해소.**
- **2026-07-01 (2)** — `workspace.html` session shell 콘솔을 쉘 명령 기준으로 갱신(key/value write 예시 제거 → `$` 프롬프트·쉘 stdout·복원 시 env/cwd 보존 시연). J5-S1~S3 ⚠️→✅, 요약 ✅8/⚠️4/❌3/⚪3. 디자인/레이아웃·임의 스타일 상태는 변화 없음.
- **2026-07-01** — 인터랙티브 쉘 확정(당시 가치 V6, 2026-08-08 삭제)에 따른 J5 신설 반영. `workspace.html`을 J5-S1~S3에 매핑(당시 콘솔 내용 갱신 필요=⚠️), J5-S4는 전용 화면 없음(❌). 단계 총계 14→18.
- **2026-06-27** — mockup 4종(index/new-session/restore/workspace) 매핑 최초 기록(인덱스 신설). restore/workspace의 J4 동시 접근 패널 제거 반영.
