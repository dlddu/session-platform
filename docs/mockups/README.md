# Session Pod Platform — Mockup 인덱스

> 이 문서는 `가치 → 사용자 여정 → mockup ↔ 디자인 시스템` 사슬에서
> **mockup ↔ (여정 단계 · 가치 · 디자인 시스템)** 연결의 **단일 진실 원천**입니다.
> 상위 상태 추적은 [`../doc-structure-state.md`](../doc-structure-state.md),
> 여정 정의는 [`../user-journeys/`](../user-journeys/), 가치 정의는 [`../values.md`](../values.md)(V1~V6) 참고.

> ⚠️ **파일명 주의**: `index.html`은 이 인덱스 문서가 아니라 **세션 목록(대시보드) mockup**입니다.
> mockup 매핑의 단일 소스는 이 `README.md`입니다.

---

## Mockup 목록과 매핑

| 파일 | 화면(`<title>`) | 시각화하는 여정 단계 | 달성 가치 | 디자인 시스템 |
|------|------|----------------------|-----------|----------------|
| [`index.html`](./index.html) | Sessions — control plane (세션 목록 대시보드) | **J3-S1** | V4 (보조 V1·V5) | 미연결 — 인라인 임의 토큰 |
| [`new-session.html`](./new-session.html) | New session (새 세션 생성) | **J1-S1, J1-S2** | V1, V5 | 미연결 — 인라인 임의 토큰 |
| [`workspace.html`](./workspace.html) | Session workspace (활성 세션 작업) | **J1-S3, J5-S1·J5-S2·J5-S3** | V1, V3, V6 | 미연결 — 인라인 임의 토큰 |

> 참고: `workspace.html`의 session shell 콘솔은 2026-07-01 쉘 명령 기준으로 갱신됨(옛 key/value write 예시 → `$ ls` / `$ npm run build` 등 쉘 입력·출력, 복원 시 env·cwd 보존 시연). V6·J5와 정합.
| [`restore.html`](./restore.html) | Resume from checkpoint (CRIU 복원) | **J2-S4** | V3, V2 | 미연결 — 인라인 임의 토큰 |

> 4개 mockup 모두 디자인 시스템 없이 인라인 CSS 변수를 사용 → 전부 **임의 스타일 mockup(🟢)**.
> 디자인 시스템 셋업 후 토큰/컴포넌트 단위로 재매핑이 필요합니다.

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
| J2-S4 복원 후 재개 | ✅ | `restore.html` (CRIU 복원, in-memory 상태 보존) |
| J3-S1 세션 목록 확인 | ✅ | `index.html` (active/idle/snapshot 상태별 목록) |
| J3-S2 세션 B로 전환 | ⚠️ | `index.html` ↔ `workspace.html` 네비게이션으로 암시 |
| J3-S3 상태에 따른 활성화 | ⚠️ | `workspace.html`이 snapshot 세션이면 `restore.html`로 전환 |
| J3-S4 다시 A로 복귀 | ❌ | 전용 화면 없음 |
| J4-S1 동시 요청 발생 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| J4-S2 atomic 전이로 단일화 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| J4-S3 일관된 결과 | ⚪ | 백엔드 동시성, UI 비대상 (아래 메모) |
| J5-S1 쉘 연결 | ✅ | `workspace.html` session shell — 쉘 attach·프롬프트 렌더 |
| J5-S2 명령 입력 | ✅ | `workspace.html` input-row `$` 프롬프트 + 콘솔 쉘 명령(`$ ls`, `$ npm run build`) |
| J5-S3 출력 확인 | ✅ | `workspace.html` term — 쉘 stdout 렌더(빌드 로그·상태 보존 문구) |
| J5-S4 쉘 상태 축적 | ❌ | 전용 화면 없음 (상태 축적 자체를 그리는 화면 부재) |

요약: ✅ 8 · ⚠️ 4 · ❌ 3 · ⚪ 3 (총 18단계)

---

## 미시각화 단계 메모

- **🟡 시각화 누락(전용 화면 필요 가능)**: `J2-S3`(재접근), `J3-S4`(A로 복귀), `J5-S4`(쉘 상태 축적). 의도적 제외인지, 화면이 필요한지 결정 필요.
- **🟡 부분 시각화**: `J2-S1`, `J2-S2`, `J3-S2`, `J3-S3` — 상태 표시·네비게이션·전환 동작으로 암시되나 단계 전용 화면은 없음.
- **✅ 해결됨 (2026-07-01) — J5-S1~S3 mockup 내용**: `workspace.html`의 session shell 콘솔이 옛 key/value write 예시에서 **쉘 명령 입력·stdout 출력**(`$ ls`, `$ npm run build`, 복원 시 env·cwd 보존)으로 갱신되어 V6·J5와 정합. 이전 ⚠️(부분/내용 갱신 필요) → ✅.
- **⚪ J4-S1~S3 (의도적 비시각화)**: J4는 동시 접근을 ConfigMap(resourceVersion CAS) + Lease 기반 atomic 전이로 푸는 **백엔드 동시성 여정**으로, UI에 그릴 화면이 아니다.
  이전에 `restore.html`·`workspace.html`에 들어 있던 **"Attached clients(operator + automation) + 동시 atomic 전이" 패널**은
  UI 구현 대상이 아니라는 판단으로 **2026-06-27 제거**됨. 따라서 J4는 시각화 대상이 아니며, 이는 *누락이 아니라 의도된 상태*다.
  (페르소나 P1+P2의 동시 접근 시나리오는 단계 시각화의 행위자였을 뿐, 그 자체가 시각화 대상이 아님 — 가치/단계만이 시각화 대상.)

---

## 마지막 갱신

- **2026-07-01 (2)** — `workspace.html` session shell 콘솔을 쉘 명령 기준으로 갱신(key/value write 예시 제거 → `$` 프롬프트·쉘 stdout·복원 시 env/cwd 보존 시연). J5-S1~S3 ⚠️→✅, 요약 ✅8/⚠️4/❌3/⚪3. 디자인/레이아웃·임의 스타일 상태는 변화 없음.
- **2026-07-01** — V6(인터랙티브 쉘) 확정에 따른 J5 신설 반영. `workspace.html`을 J5-S1~S3·V6에 매핑(당시 콘솔 내용 갱신 필요=⚠️), J5-S4는 전용 화면 없음(❌). 단계 총계 14→18.
- **2026-06-27** — mockup 4종(index/new-session/restore/workspace) 매핑 최초 기록(인덱스 신설). restore/workspace의 J4 동시 접근 패널 제거 반영.
