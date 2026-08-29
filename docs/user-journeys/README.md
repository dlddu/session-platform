# Session Pod Platform 사용자 여정

이 폴더는 가치(V1~V5·V8)를 사용자 흐름으로 푸는 **사용자 여정 문서**입니다. 여정별로 파일이 나뉘어 있고,
이 문서(README)는 여러 여정이 공유하는 **페르소나·가치 커버리지·미해결 항목**을 담습니다.

> 📐 **문서 형식 (2026-08-29 정규화)**: J1~J6은 모두 동일한 9개 섹션을 갖습니다 —
> `0. 문서 정보` / `1. 서비스 개요` / `2. 여정 정의` / `3. 단계별 상세` / `4. 분기·예외 흐름` /
> `5. 측정 지표` / `6. 변경 이력` / `7. 인접 여정과의 관계` / `8. 구현 상태`.
> 각 단계는 **사용자 행동 · 터치포인트 · 생각·감정 · 페인포인트/이탈 위험 · 관련 AC · mockup** 6항목을 채웁니다.
> 새 여정을 추가하거나 기존 여정을 고칠 때 이 구조와 섹션 제목을 바꾸지 않습니다.
>
> **식별자는 `J{n}`/`J{n}-S{m}` 순번 방식을 유지합니다.** 표준 형식은 `JRN-`/`STP-` 슬러그이지만,
> `../mockups/README.md`·`../doc-structure-state.md`·mockup 5종이 전부 순번 식별자를 참조하고 있어
> 슬러그로 전환하면 연결이 통째로 끊깁니다. 단계를 추가할 때는 뒤에 붙이고, 삭제해도 번호를 재사용하지 않습니다.

> ⚠️ **확인 필요**: 아래 페르소나와 여정은 가치 문서(`../values.md`)와 PRD에서 역으로 추론한 것입니다.
> 실제 사용자/시나리오에 맞게 수정이 필요합니다.
>
> ✅ **확정됨 (2026-08-08 갱신)**: 세션은 전용 pod에서 사용자가 선택한 workload를 실행합니다.
> 기본 `shell`은 PTY 인터랙티브 쉘(`../prd/shell-workload.md`), `claude-code`는 serial one-shot
> agent loop(`../prd/claude-code-workload.md`)이며 두 타입 모두 같은 격리·동결·복원 보장을 받습니다.

> 📎 **연결 관계**: 이 문서는 `가치 → 사용자 여정 → mockup ↔ 디자인 시스템` 중 **사용자 여정**입니다.
> 각 여정 단계는 mockup으로 시각화됩니다. 현재 mockup 5종(`../mockups/`)이 작성되어 **J1·J5·J6은 완전·J2/J3는 부분** 시각화,
> **J4는 백엔드 동시성 여정이라 의도적 비시각화**입니다. 디자인 시스템 정본은 `web/src/design/`입니다.
> (단계별 매핑: `../mockups/README.md` · 상세 상태: `../doc-structure-state.md`).

---

## 여정 목록

| ID | 여정 | 페르소나 | 달성 가치 | 파일 |
|----|------|----------|-----------|------|
| J1 | 첫 세션 생성과 격리된 작업 | P1 | V1, V5 | [j1-session-creation.md](./j1-session-creation.md) |
| J2 | 자리 비움과 끊김 없는 재개 | P1 | V2, V3 | [j2-idle-resume.md](./j2-idle-resume.md) |
| J3 | 여러 세션 사이 자유 전환 | P1 | V4, V3 | [j3-multi-session-switch.md](./j3-multi-session-switch.md) |
| J4 | 동시 접근에서도 깨지지 않는 상태 | P1 + P2 | V5 | [j4-concurrent-access.md](./j4-concurrent-access.md) |
| J5 | 쉘에서 명령을 실행하고 상태를 이어간다 | P1 | V3 | [j5-shell-interaction.md](./j5-shell-interaction.md) |
| J6 | 목적에 맞는 작업 환경을 골라 에이전트에게 일을 시킨다 | P1 | V8, V3 | [j6-agent-prompt-loop.md](./j6-agent-prompt-loop.md) |

> **J5 ↔ J6은 같은 축의 타입별 쌍입니다.** J5는 `shell` 타입의 사용 루프(직접 명령), J6는 `claude-code` 타입의 사용 루프(프롬프트)이며, J6-S1이 그 **타입을 고르는 순간**을 담아 V8을 달성합니다.

---

## 페르소나

### P1: 멀티세션 작업자 (이하 "작업자")
- **누구**: 여러 개의 장시간 상태 유지 세션을 동시에 보유하고, 그 사이를 오가며 작업하는 사람.
- **상황**: 한 세션에서 작업하다 잠시 자리를 비우거나 다른 세션으로 맥락을 바꾼다. 세션이 멈췄다 재개되어도 작업 맥락이 끊기지 않기를 기대한다.
- **목표**: 세션을 신경 쓰지 않고(생성/동결/복원은 시스템이 알아서) 작업 그 자체에만 집중하는 것.

### P2: 자동화 클라이언트
- **누구**: read/write API로 세션을 프로그래밍적으로 다루는 프로그램(배치 작업, 오케스트레이터, 에이전트 컨트롤러 등).
- **상황**: 사람의 개입 없이 세션에 read/write를 보내며, 때로 작업자(P1)와 **같은 세션에 동시에** 접근한다.
- **목표**: 어떤 타이밍에 요청을 보내도 세션 상태가 깨지지 않고 일관된 결과를 받는 것.

---

## 가치 ↔ 여정 커버리지

| 가치 | 달성하는 여정 | 상태 |
|------|--------------|------|
| V1 세션 격리 | J1 | ✅ 연결됨 |
| V2 유휴 자원 회수 | J2 | ✅ 연결됨 |
| V3 끊김 없는 세션 연속성 | J2, J3, J4, J5, J6 | ✅ 연결됨 |
| V4 자유로운 멀티세션 전환 | J3 | ✅ 연결됨 |
| V5 일관된 세션 상태 | J1, J4 | ✅ 연결됨 |
| V8 목적에 맞는 작업 환경 선택 | J6 | ✅ 연결됨 (2026-08-08 J6 신설로 해소) |

> **고아 여정(가치 없는 여정) 없음 · 여정 없는 가치 없음.** 2026-08-08 신설된 V8(작업 환경 선택)은 같은 날 **J6 신설로 연결**되었습니다(이전: 여정 없음). J5는 V6 삭제에 따라 V3(연속성)으로 재연결됨.
> mockup 매핑 결과 **V1~V5·V8 전부 1개 이상 mockup에 연결**됩니다. 단계 단위로는 J1·J5·J6 완전·J2/J3 부분·J4 의도적 비시각화이며, `J2-S3`·`J3-S4`가 미시각화로 남아 있습니다. J5 단계(S1~S4)는 `workspace.html`의 쉘 콘솔·session state 패널에, J6 단계(S1~S5)는 `new-session.html`의 타입 선택과 `agent-workspace.html`의 프롬프트 콘솔에 시각화됩니다. (단계별 매핑: `../mockups/README.md` · 상세: `../doc-structure-state.md`)

---

## 미해결 항목 (여정 작성 중 발견)

- **세션 목록 조회 흐름 (J3-S1)**: 사용자가 보유 세션과 상태를 확인하는 경로가 PRD에 없음. PRD 보강 또는 의도적 제외 결정 필요.
- ~~**유휴 측정 기준 (J2-S1)**~~ → **2026-07-01 확정**: 마지막 클라이언트 read/write부터 60분을 잰다. 별도의 operational `active→idle` producer는 없고 reaper가 `active`/`idle` record를 직접 검사한다. grace period·busy-shell 등 세부 정책만 열린 항목이다.
- ~~**idle/snapshot 상태의 read/write 정책 (J1-S3, J2-S4)**~~ → **2026-06-27 확정**: 비-active 접근은 통일 "active 보장 후 처리"(idle 승격 / snapshot 복원 후 read·write). J1-S3(격리된 작업)·J2-S4(복원 후 재개) 단계 경험이 이 규칙으로 정의됨 (AC-C2/AC-C3 · `../doc-tracker.md` 참고).
- ~~**"세션"의 정체**~~ → **2026-07-01 확정**: 세션 = 전용 pod의 인터랙티브 쉘(`../prd/shell-workload.md`). **2026-08-08 개정**: 워크로드 타입이 `shell`·`claude-code` 둘로 늘어남(`../prd/claude-code-workload.md`). ~~`claude-code` 여정 신설 여부 검토 필요~~ → **2026-08-08 해소**: J6 신설로 `claude-code` 타입의 사용 루프와 타입 선택 순간을 모두 다룸. 세션의 정체는 이제 "전용 pod에서 도는 **선택된 타입의** 워크로드"이며, J5(쉘)·J6(에이전트)가 타입별 루프를 나눠 담는다. **제품명·소유자**는 여전히 임시값(`../doc-tracker.md` 참고).
- ~~**에이전트 세션의 재개 방식·기본 모델 (J6-S4, J6-S1)**~~ → **2026-08-08 확정, 2026-08-09 개정**: 첫 성공 실행 뒤 `--continue`, 세션별 고정 HOME/workdir, immutable model, 특정 공급자 버전을 API에 고정하지 않는 `platform-default` 별칭을 사용한다. 별칭은 optional Secret `model`을 우선하고 missing/empty면 CLI 기본값으로 fallback하며, 구체 세션 model은 literal로 우선한다. optional `model`의 concrete 기본값은 UI에서 `<model> (platform default)`로 표시하되 create request에서는 생략하고, `models` JSON catalog가 있으면 나머지 ordered picker, missing/empty/`[]`이면 free-text 입력을 사용한다. catalog는 API allowlist가 아니며 mockup의 `model-a`/`model-b`는 catalog가 있는 상태의 예시 이름이다.
- **에이전트 세션의 전용 페르소나 (J6)**: J6는 P1(멀티세션 작업자)로 작성됐으나, "프롬프트로 일을 맡기는 사람"이 P1과 같은 페르소나인지 별도 페르소나인지 확인이 필요하다. P2(자동화 클라이언트)가 `claude-code` 세션을 쓰는 시나리오도 아직 다뤄지지 않음.

> 가치 문서·PRD의 수정이 필요한 항목은 product-doc-engineer 영역입니다. 이 문서는 그 결정을 반영만 합니다.
