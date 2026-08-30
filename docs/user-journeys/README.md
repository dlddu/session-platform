# Session Pod Platform 사용자 여정

이 폴더는 제품 가치(`../values.md`, V1~V5·V8)를 **사용자가 겪는 흐름**으로 푸는 여정 문서입니다.
여정 하나가 파일 하나이고, 이 README는 여러 여정이 공유하는 **식별자 규칙 · 페르소나 · 가치 커버리지 · 미해결 항목**을 담습니다.

> 📎 **연결 관계**: `가치 → 사용자 여정 → mockup ↔ 디자인 시스템` 사슬에서 이 폴더는 **사용자 여정**입니다.
> 여정 단계 ↔ mockup 매핑의 단일 진실 원천은 [`../mockups/README.md`](../mockups/README.md)이고,
> 사슬 전체의 상태는 [`../doc-structure-state.md`](../doc-structure-state.md)에서 추적합니다.
> 백엔드 사슬(`가치 → PRD → AC → 테스트`)은 [`../doc-tracker.md`](../doc-tracker.md)입니다.

---

## 문서 규칙

**1. 식별자는 슬러그를 쓴다 (순번 금지).**
여정은 `JRN-<슬러그>`, 단계는 `STP-<슬러그>`. 단계는 중간에 추가·삭제·재배치되므로 순번(`S1`, `S2`)을 식별자로 쓰면
순서가 바뀌는 순간 mockup·테스트·코드 주석 쪽 연결이 **조용히** 어긋납니다. 단계 슬러그는 이 폴더 전체에서 유일합니다.
문서를 고칠 때 기존 식별자는 바꾸지 않고, 단계를 폐기하면 식별자를 재사용하지 말고 변경 이력에 남깁니다.

**2. 단계는 사용자 행동 기준으로 나눈다.**
화면·API는 단계 이름이 아니라 **터치포인트** 항목에 적습니다. 화면은 리뉴얼마다 바뀌지만 행동은 오래 남습니다.

**3. 계약의 정본은 PRD다.**
플래그·상한·상태 코드·전이 규칙 같은 명세는 여정에 옮겨 적지 않고 **AC 식별자로 위임**합니다.
여정에는 "사용자가 무엇을 하고 무엇을 보고 무엇을 느끼는가"만 적습니다. 이 규칙이 없으면 여정 문서가 PRD 요약본이 되어
PRD가 바뀔 때마다 조용히 낡습니다.

**4. 구조는 8개 문서가 동일하다.**
섹션 순서·제목을 바꾸지 않습니다. 해당 없는 항목은 지우지 말고 "해당 없음" 또는 `TBD`로 남깁니다.

### 구 식별자 매핑 (2026-08-30 전환)

2026-08-30 이전에는 `J1`~`J6` / `J1-S1` 같은 순번 식별자를 썼습니다. 옛 커밋·변경 이력을 읽을 때 참고하세요.
**과거 변경 이력에 적힌 옛 식별자는 그대로 둡니다**(당시 기록이므로 고쳐 쓰지 않습니다).

| 구 여정 | 신 여정 | 구 단계 → 신 단계 |
|---|---|---|
| J1 | `JRN-session-creation` | S1 `STP-create-request` · S2 `STP-workspace-entry` · S3 `STP-isolated-work` |
| J2 | `JRN-idle-resume` | S1 `STP-step-away` · S2 `STP-auto-freeze` · S3 `STP-reaccess` · S4 `STP-restore-resume` |
| J3 | `JRN-multi-session-switch` | S1 `STP-session-list` · S2 `STP-switch-away` · S3 `STP-target-activation` · S4 `STP-switch-back` |
| J4 | `JRN-concurrent-access` | S1 `STP-parallel-clients` · S2 `STP-collision` · S3 `STP-consistent-result` |
| J5 | `JRN-shell-interaction` | S1 `STP-shell-attach` · S2 `STP-command-input` · S3 `STP-output-read` · S4 `STP-shell-state-carry` |
| J6 | `JRN-agent-prompt-loop` | S1 `STP-workload-choice` · S2 `STP-prompt-submit` · S3 `STP-response-watch` · S4 `STP-conversation-carry` · S5 `STP-agent-freeze-resume` |
| — | `JRN-manual-freeze` | 신설 (2026-08-30) |
| — | `JRN-session-deletion` | 신설 (2026-08-30) |

---

## 여정 목록

| 여정 식별자 | 여정명 | 페르소나 | 달성 가치 | 단계 | 문서 |
|---|---|---|---|:--:|---|
| `JRN-session-creation` | 첫 세션 생성과 격리된 작업 | P1 | V1, V5 | 3 | [문서](./JRN-session-creation.md) |
| `JRN-idle-resume` | 자리 비움과 끊김 없는 재개 | P1 | V2, V3 | 4 | [문서](./JRN-idle-resume.md) |
| `JRN-multi-session-switch` | 여러 세션 사이 자유 전환 | P1 | V4, V3 | 4 | [문서](./JRN-multi-session-switch.md) |
| `JRN-concurrent-access` | 동시에 건드려도 깨지지 않는 세션 | P1 + P2 | V5, V3 | 3 | [문서](./JRN-concurrent-access.md) |
| `JRN-shell-interaction` | 쉘에서 명령을 실행하고 상태를 이어간다 | P1 | V3 | 4 | [문서](./JRN-shell-interaction.md) |
| `JRN-agent-prompt-loop` | 작업 환경을 골라 에이전트에게 일을 시킨다 | P1 | V8, V3 | 5 | [문서](./JRN-agent-prompt-loop.md) |
| `JRN-manual-freeze` | 다 쓴 세션을 직접 접어두기 | P1 | V2, V3 | 3 | [문서](./JRN-manual-freeze.md) |
| `JRN-session-deletion` | 끝난 세션을 정리하기 | P1 | V2 (부분) | 3 | [문서](./JRN-session-deletion.md) |

**총 8개 여정 · 29개 단계.**

여정 사이 관계는 두 축으로 읽습니다.

- **타입 축**: `JRN-shell-interaction`(`shell`)과 `JRN-agent-prompt-loop`(`claude-code`)는 같은 자리의 타입별 쌍입니다.
  타입을 고르는 순간은 `STP-workload-choice`가 담고, 그것이 V8입니다.
- **수명 축**: 생성(`JRN-session-creation`) → 사용(위 두 여정) → 접기(`JRN-idle-resume` 자동 · `JRN-manual-freeze` 수동)
  → 되살리기(`STP-restore-resume`) → 정리(`JRN-session-deletion`).

---

## 페르소나

> ⚠️ **검증 필요 (미해결)**: 아래 두 페르소나는 실제 사용자 인터뷰가 아니라 가치 문서·PRD에서 **역으로 추론**한 것입니다.
> 2026-06-18 최초 작성 이후 실사용자로 검증된 적이 없고, 그 사이 여정이 8개까지 늘었습니다.
> 구체 서술에는 `(가정)`을 붙였습니다 — 팀이 먼저 확인해야 할 지점입니다.

### P1: 멀티세션 작업자

- **누구**: 박세진(가명), 32세 플랫폼 엔지니어 (가정). 상태가 길게 쌓이는 작업 세션 3~5개를 동시에 굴린다 (가정).
- **상황**: 한 세션에 빌드를 걸어두고 다른 세션으로 맥락을 옮긴다. 회의·퇴근으로 몇 시간씩 자리를 비우고,
  돌아와서 "아까 그 상태 그대로"를 기대한다. 터미널 멀티플렉서를 직접 관리하던 습관이 있어
  **세션이 소리 없이 사라지는 것**에 특히 민감하다 (가정).
- **목표**: 세션 관리(생성·동결·복원)를 신경 쓰지 않고 작업 자체에만 집중하는 것.
- **성공의 정의**: "내가 세션을 관리하고 있다는 자각이 없다."

### P2: 자동화 클라이언트

- **누구**: 사람이 아니라 프로그램. 야간 배치, 오케스트레이터, 에이전트 컨트롤러 등 (가정).
- **상황**: read/write API로 세션을 프로그래밍적으로 다루며, 때로 P1과 **같은 세션에 동시에** 접근한다.
- **목표**: 어떤 타이밍에 요청을 보내도 일관된 결과를 받는 것. 재시도가 중복 부작용을 만들지 않는 것.
- **주의**: P2는 감정이 없으므로, P2가 주인공인 단계에서는 "생각·감정" 자리에 **호출자가 관측하는 결과**를 적습니다.
  P2는 현재 `JRN-concurrent-access`에만 등장합니다 — 아래 미해결 항목 참고.

---

## 가치 ↔ 여정 커버리지

| 가치 | 달성하는 여정 | 상태 |
|---|---|---|
| V1 세션 격리 | `JRN-session-creation` | ✅ |
| V2 유휴 자원 회수 | `JRN-idle-resume`, `JRN-manual-freeze`, `JRN-session-deletion`(부분) | ✅ |
| V3 끊김 없는 세션 연속성 | `JRN-idle-resume`, `JRN-multi-session-switch`, `JRN-concurrent-access`, `JRN-shell-interaction`, `JRN-agent-prompt-loop`, `JRN-manual-freeze` | ✅ |
| V4 자유로운 멀티세션 전환 | `JRN-multi-session-switch` | ✅ |
| V5 일관된 세션 상태 | `JRN-session-creation`, `JRN-concurrent-access` | ✅ |
| V8 목적에 맞는 작업 환경 선택 | `JRN-agent-prompt-loop` | ✅ |

**고아 여정 없음 · 여정 없는 가치 없음.** 단, `JRN-session-deletion`의 가치 연결은 부분적입니다(아래 미해결 항목).

단계별 mockup 시각화 상태는 여기서 중복 관리하지 않습니다 — [`../mockups/README.md`](../mockups/README.md)가 단일 소스입니다.

---

## 미해결 항목

여정을 쓰면서 발견한, **여정 문서 밖에서 결정되어야 하는** 항목입니다.
가치·PRD 수정이 필요한 것은 product-doc-engineer 영역이고, 이 폴더는 결정을 반영만 합니다.

### 🔴 오래 밀려 있는 항목

- **페르소나 미검증**: 위 경고 참고. 실사용자 1~2명 인터뷰로 P1을 확정하거나 폐기해야 합니다.
  현재 여정 8개·단계 29개가 전부 검증되지 않은 P1 위에 서 있습니다.
- **제품명·소유자 미지정**: "Session Pod Platform"은 임시 작업명이고 소유자가 없어 가치 전부가 고아 상태입니다
  (`../values.md` · `../doc-tracker.md`).

### 🟡 열린 결정

- **세션 종료의 가치 연결 (`JRN-session-deletion`)**: 삭제는 AC-A3(자원 회수)를 거쳐 V2에 닿지만,
  V2의 서술은 "유휴 세션의 자동 회수"라서 **사용자가 의도적으로 수명을 끝내는 행위**를 담지 못합니다.
  V2 서술을 넓히거나 별도 가치를 세우는 결정이 필요합니다.
- **수동 동결의 위치 (`JRN-manual-freeze`)**: 같은 이유로 V2의 "60분 유휴" 서술과 어긋납니다.
  수동 동결은 AC-B1/AC-A3의 사용자 주도 트리거로 이해하고 있으나 **수동 트리거 전용 AC가 PRD에 없습니다**.
- **P2의 사용 범위**: P2는 `JRN-concurrent-access`에만 등장합니다. P2가 `claude-code` 세션을 쓰는 시나리오,
  P2 단독의 배치 여정이 필요한지 미확인.
- **에이전트 세션의 전용 페르소나**: `JRN-agent-prompt-loop`는 P1으로 썼지만
  "프롬프트로 일을 맡기는 사람"이 P1과 같은 사람인지 확인 필요.
- **`claude-code`의 `idle` 의미**: 상주 프로세스가 없는 타입에서 `active`와 `idle`의 차이는 자원 점유뿐입니다.
  별도 정책(더 짧은 유휴 한계 등)이 필요한지 미결 (`../doc-tracker.md`).
- **동결 트리거 세부 정책**: 60분 한계는 확정(AC-B1·AC-D5)이나 grace period·per-session override,
  장시간 포그라운드 작업 중인 쉘의 동결 여부는 미결 (`../doc-tracker.md`).
- **지표 목표치 전부 `TBD`**: 8개 문서의 지표는 정의(분자/분모)만 확정했고 목표 수치가 없습니다.
  관측 파이프라인이 붙는 시점에 함께 정해야 합니다.

### ✅ 해소된 항목 (기록 보존)

- ~~**"세션"의 정체**~~ → 2026-07-01 확정(전용 pod의 인터랙티브 쉘), 2026-08-08 개정(타입 `shell`·`claude-code`).
- ~~**유휴 측정 기준**~~ → 2026-07-01 확정: 마지막 클라이언트 read/write 기준 (AC-B1·AC-D5).
- ~~**idle/snapshot 상태의 read/write 정책**~~ → 2026-06-27 확정: 비-active 접근은 "active 보장 후 처리" (AC-C2/C3).
- ~~**에이전트 재개 방식·기본 모델**~~ → 2026-08-08 확정, 2026-08-09 개정 (AC-E4/E6).
- ~~**세션 목록 조회 흐름이 PRD에 없음**~~ → 2026-08-30 해소: `GET /api/v1/sessions`와 Sessions 화면이 구현되어
  `STP-session-list`가 실제 구현을 가리킵니다. (전용 AC를 세울지는 여전히 product-doc-engineer 판단.)
