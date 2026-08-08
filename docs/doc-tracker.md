# Session Pod Platform 문서 체계 상태 추적

## 현재 상태 요약
- 정의된 가치: **6개** (V1~V5, V8) — 2026-08-08 워크로드 타입 가치(V6·V7) 삭제 후, 타입 선택을 추상 가치 V8로 재정립. V6·V7 식별자는 재사용하지 않음(`values.md`의 삭제된 가치 표)
- PRD: **5개** (아키텍처/라이프사이클/상태·API/쉘 워크로드/클로드코드 워크로드)
- Acceptance Criteria: **21개** (가치 연결됨: 21개 / 미연결: 0개)
- 테스트 문서: **5개** (AC 커버됨: 21개 / 미커버: 0개)
- **건강 상태**: ⚠️ **위험 있음** — 모든 가치의 소유자가 미지정(고아 가치 6개). 구조적 연결(가치→PRD→AC→테스트)은 완전.

## 연결 매트릭스

| 가치 | PRD | AC | 테스트 | 상태 |
|------|-----|-----|--------|------|
| V1: 세션 격리 | PRD-아키텍처, PRD-쉘워크로드, PRD-클로드코드 | AC-A1, AC-A2, AC-D1, AC-E1, AC-E6 | T-아키텍처, T-쉘워크로드, T-클로드코드 | ✅ 완전 |
| V2: 유휴 자원 회수 | PRD-아키텍처, PRD-라이프사이클, PRD-쉘워크로드, PRD-클로드코드 | AC-A3, AC-B1, AC-D5, AC-E5 | T-아키텍처, T-라이프사이클, T-쉘워크로드, T-클로드코드 | ✅ 완전 |
| V3: 끊김 없는 연속성 | PRD-라이프사이클, PRD-상태·API, PRD-쉘워크로드, PRD-클로드코드 | AC-B2, AC-B3, AC-C2, AC-C3, AC-D2, AC-D3, AC-D4, AC-E2, AC-E3, AC-E4, AC-E5 | T-라이프사이클, T-상태·API, T-쉘워크로드, T-클로드코드 | ✅ 완전 |
| V4: 자유로운 멀티세션 전환 | PRD-상태·API, PRD-쉘워크로드, PRD-클로드코드 | AC-C2, AC-C4, AC-D3, AC-E3 | T-상태·API, T-쉘워크로드, T-클로드코드 | ✅ 완전 |
| V5: 일관된 세션 상태 | PRD-아키텍처, PRD-상태·API | AC-A1, AC-C1, AC-C3 | T-아키텍처, T-상태·API | ✅ 완전 |
| V8: 목적에 맞는 작업 환경 선택 | PRD-클로드코드 | AC-E1 | T-클로드코드 | ✅ 완전 |

> **가치 상속 규칙 (2026-08-08)**: 워크로드 타입 가치(V6·V7)가 삭제되면서, 타입별 구체화 AC는
> 자신이 구체화하는 **상위 AC의 가치를 상속**한다:
> `shell` — AC-D1→AC-A1(V1), AC-D2→AC-C3(V3), AC-D3→AC-C2(V3·V4), AC-D4→AC-B3(V3), AC-D5→AC-B1(V2)
> `claude-code` — AC-E2→AC-C3(V3), AC-E3→AC-C2(V3·V4), AC-E4→(V3), AC-E5→AC-B1·B2·B3(V2·V3), AC-E6→AC-A1(V1)
> **AC-E1(워크로드 타입 선택)만 예외** — 구체화가 아니라 새 축이라 상속할 가치가 없어, 이를 뒷받침하는 추상 가치 **V8**을 신설해 직접 연결했다(2026-08-08 3차).

## 위험 진단

### 🔴 고아 가치 (소유자 없는 가치)
- **V1~V5·V8 전부** — 제품 소유자가 미지정 상태. 가치의 책임 소재가 불분명함. **소유자 지정 필요.**

### 미정렬 문서 (가치 참조 없는 문서)
- (없음)

### 무가치 PRD (가치를 달성하지 않는 PRD)
- (없음)

### AC 없는 PRD
- (없음)

### 미연결 AC (가치와 연결되지 않은 AC)
- (없음) — *2026-08-08 해소*. V6·V7 삭제로 잠시 미연결이던 AC-E1은 추상 가치 **V8(목적에 맞는 작업 환경 선택)** 신설로 연결됨(아래 해결된 설계 결정 참고).

### 미검증 AC (테스트 없는 AC)
- (없음)

### 고아 테스트 (AC를 참조하지 않는 테스트)
- (없음)

### 🟡 확인 필요한 설계 결정 (스킬 표준 위험은 아니나 명세 공백)
- **스냅샷 트리거 정책 (AC-B1)**: 60분 최대 유휴 한계에 대한 **운영 트리거는 구현됨** (`service.IdleReaper`가 유휴 ≥ `MaxIdle` 세션을 주기 스캔해 스냅샷 + pod 회수). 다만 grace period·per-session override 등 정확한 트리거 타이밍 정책은 여전히 미확정 (`session.go`의 `TODO(policy)`).
- **바쁜 쉘 동결 여부 (AC-D5 ↔ AC-B1)**: 장시간 포그라운드 작업 실행 중 클라이언트 유휴 60분 도달 시 동결 대상이 되는지 — CRIU상 기술적으로는 가능하나, 트리거 정책과 함께 결정 필요. (AC-D5로 유휴 "정의"는 확정, "정책"은 미확정)
- **read 출력 버퍼 증가 (AC-D3)**: 페이지네이션은 2026-07-03 `offset` 커서 개정으로 해소(반복 read의 `payload`는 델타만큼). 스냅샷 시 버퍼 처리 중 **복원 후 offset 커서 유효성**은 2026-07-04(J5-S4) 확정 — scrollback이 에이전트 메모리 상주라 CRIU 체크포인트에 포함되어 복원 후에도 커서 유효(버퍼-인-체크포인트). 누적 버퍼 자체의 상한/ring buffer는 계속 보류.
- **제품명·소유자**: "Session Pod Platform"은 임시 작업명. 실제 제품명/소유자 확정 필요.
- **🆕 claude-code 대화 재개 방식 (AC-E4)**: 확정된 커맨드 형태(`claude --model … -p <프롬프트>`)에는 재개 옵션이 없다. 첫 실행은 그대로지만 2회차 이후가 AC-E4(대화 이어짐)를 만족하려면 재개 옵션이 필요하다. **어떤 재개 방식을 쓸지, 대화 ID를 세션 메타데이터로 보관할지 미확정.** T-클로드코드 시나리오 5는 확정 전까지 의도적 실패로 둔다.
- **🆕 claude-code 모델 정책 (AC-E6)**: "세션 model 불변"과 "플랫폼 기본 모델 존재"는 타입 불변성(AC-E1)과의 일관성을 위해 채택한 결정으로, 별도 확인 필요. 기본 모델의 구체 값도 미지정.
- **🆕 claude-code의 `idle` 상태 의미 (AC-E5 ↔ AC-C2/C3)**: 상주 프로세스가 없는 타입에서 `active`와 `idle`(pod 보유·미사용)의 실질 차이는 자원 점유뿐이다. 상태 모델은 타입 공통으로 유지했으나, `idle` 구간에 대한 별도 정책(예: 더 짧은 유휴 한계)이 필요한지는 미결.

### ✅ 해결된 설계 결정
- **워크로드 타입의 가치 표현 (V6·V7 → V8)** — *2026-08-08 확정*. 워크로드 타입 자체를 가치로 올린 V6·V7은 추상도가 과해(수단을 가치 자리에 둠) 삭제하고, 타입별 명세는 PRD에 남겼다. 그 결과 미연결 상태가 된 AC-E1(타입 선택)은 **선택 가능하다는 사실만** 담는 추상 가치 V8로 재연결했다. 판단 기준: 새 워크로드 타입이 추가돼도 **가치 문서는 변하지 않아야 한다** — V6·V7 방식은 타입마다 가치가 늘어났고, V8은 늘어나지 않는다.
- **세션 워크로드의 정체 (당시 V6 / PRD-쉘워크로드)** — *2026-07-01 확정, 2026-08-08 가치 V6는 삭제되고 명세는 PRD에 유지*. 이전까지 "세션 워크로드"·"인메모리 상태"·"read/write"가 추상적이고 `data-plane`이 미정의(placeholder alpine)였던 상태를 해소. **세션 = 전용 pod에서 PTY에 연결되어 실행되는 인터랙티브 쉘**(기본 `/bin/bash`)로 확정. write=쉘 stdin 입력(AC-D2), read=쉘 stdout/stderr **전체** 출력 비파괴적 반환(AC-D3), 보존 상태=쉘 프로세스 트리(AC-D4), 유휴 기준=클라이언트 쉘 I/O(AC-D5). 기존 AC-A1/B1/B3/C2/C3에 구체화 상호참조 추가, `data-plane/README.md` 갱신.
- **유휴 시간 측정 기준 (부분 해결, AC-D5)** — *2026-07-01*. "마지막 활동"의 정의를 **마지막 클라이언트 read/write(쉘 I/O)** 로 확정. 단, 트리거 *정책*(위 🟡)은 여전히 열림.
- **idle/snapshot read/write 정책 (AC-C2 / AC-C3)** — *2026-06-27 확정*. 비-active 접근은 통일 "active 보장 후 처리" 규칙: `idle`은 `idle→active` atomic 승격(AC-C1), `snapshot`은 CRIU 복원(AC-B2)으로 active 전이 후 read/write.

### 🟠 인접 사슬에서 인지된 항목 (이 문서 관할 밖, 참고용)
- ~~**디자인 사슬 미갱신 (`claude-code` 타입)**~~ → **2026-08-08 해소**: `claude-code` 타입을 다루는 여정(J6)과 mockup이 신설되었다 — 세션 생성 화면에 워크로드 타입·모델 선택이 추가되고, 프롬프트·응답 콘솔(`mockups/agent-workspace.html`)이 생겼다. 상세는 `doc-structure-state.md`.
- ~~**V8의 시각화 부재 (2026-08-08 신규)**~~ → **2026-08-08 해소**: V8이 **J6(작업 환경 선택 + 에이전트 프롬프트 루프)** 로 연결되고 `new-session.html`의 타입 선택 UI로 시각화되어, 디자인 사슬의 "시각화 없는 가치"는 **0**이 되었다. design-doc-structure-validator 영역.

> ⚠️ **다만 mockup 내용이 이 문서의 열린 항목에 걸려 있다**: `new-session.html`의 모델 선택지(`model-a`/`model-b`, "platform default")는 AC-E6의 기본 모델 미확정 때문에 **자리표시자**이고, `agent-workspace.html`의 대화 재개 표현은 AC-E4의 재개 방식 미확정 상태에서 결과만 그린 것이다. 두 항목이 확정되면 mockup 갱신이 필요하다.
- **V6 삭제에 따른 하류 재연결 (2026-08-08 수행)**: V6를 참조하던 J5 여정·`workspace.html` 매핑·e2e 커버리지 표를 V3으로 재연결했다. 디자인 사슬 문서(`doc-structure-state.md`)는 **참조 재연결만** 반영했고 전체 재검증은 하지 않았다.
- **구현 사슬 부분 반영 (AC-E1)** — *2026-08-08 갱신*: `workloadType`은 이제 `CreateSessionRequest`·`Session`(OpenAPI + Go 도메인 + ConfigMap 레코드)에 있고, 기본값 `shell`·허용값 외 400·수명 중 불변(snapshot→restore 왕복 보존)이 단위/통합 테스트로 검증된다. `ClientOrchestrator`도 타입별 이미지·`DATA_PLANE_WORKLOAD` env·`session-platform.dev/workload-type` 라벨로 pod를 분기한다. **다만 AC-E1은 아직 닫히지 않았다** — 검증 방법이 요구하는 "`claude-code` pod에서 클로드 코드 CLI 실행 가능"의 근거인 **data plane 이미지·에이전트 모드가 없기 때문**이다(`DATA_PLANE_CLAUDE_CODE_IMAGE` 미설정 시 그 타입 생성은 명시적 에러로 거부된다). AC-E2~E6은 여전히 **명세만 확정**이고, `model` 필드는 AC-E6의 기본 모델 미확정(위 🟡)에 걸려 아직 없다. UI의 타입 선택(J6·`new-session.html`)도 미구현.

## 변경 이력

| 시점 | 변경 내용 | 이전 상태 | 이후 상태 |
|------|-----------|-----------|-----------|
| 초기 생성 | V1~V5 가치 정의(요구사항에서 추론) | 가치 0개 | 가치 5개 (소유자 미지정) |
| 초기 생성 | PRD-아키텍처 작성 (AC-A1~A3) | PRD 0개 | PRD 1개, AC 3개 |
| 초기 생성 | PRD-라이프사이클 작성 (AC-B1~B3) | PRD 1개, AC 3개 | PRD 2개, AC 6개 |
| 초기 생성 | PRD-상태·API 작성 (AC-C1~C4) | PRD 2개, AC 6개 | PRD 3개, AC 10개 |
| 초기 생성 | 테스트 문서 3종 작성 (10개 AC 전부 커버) | 테스트 0개 | 테스트 3개, 미검증 AC 0개 |
| 2026-06-27 | AC-C2/AC-C3 idle·snapshot read/write 정책 확정("active 보장 후 처리"). | 명세 공백 2건 | 명세 공백 0건(read/write 정책) |
| 2026-06-28 | AC-C1 atomic 메커니즘을 Redis → ConfigMap(resourceVersion CAS) + Lease로 전환. | 저장소=Redis(스텁) | 저장소=ConfigMap/Lease(실구현) |
| 2026-07-01 | **V6 "인터랙티브 쉘 세션" 추가 + PRD-쉘워크로드(AC-D1~D5) + T-쉘워크로드 신설**. 세션 워크로드의 정체를 인터랙티브 쉘로 확정. AC-A1/B1/B3/C2/C3에 구체화 상호참조, values 경고·`data-plane/README.md` 갱신, 여정 README·doc-structure-state의 "세션 정체 미확정" 항목 해소 표기. | 가치 5개, PRD 3, AC 10, 테스트 3 / 세션 정체 미확정 | 가치 6개, PRD 4, AC 15, 테스트 4 / 세션 정체 확정 |
| 2026-07-01 | AC-D3 read 시맨틱을 "마지막 read 이후 델타" → **"세션 시작 이후 전체 누적 출력, 비파괴적 반환"** 으로 변경. PRD·T-쉘워크로드 시나리오3·AC-C2 상호참조 갱신, 버퍼 무제한 증가를 열린 항목으로 등록. AC 수·연결 변화 없음(시맨틱만 변경). | read=마지막 read 이후 델타(1회 전달) | read=전체 누적 출력(비파괴적) |
| 2026-07-03 | AC-D3 read 시맨틱을 **"서버 발급 `nextOffset` 커서 기반 델타"** 로 개정 — read는 요청 `offset`(기본 0) 이후 출력 + 새 `nextOffset`을 반환, `offset=0`은 전체 이력, `offset`>누적 길이는 빈 payload. 서버가 출력을 버리지 않으므로 비파괴·전체 조회 성질은 `offset=0`으로 유지. T-쉘워크로드 시나리오 2·3, J5-S3, OpenAPI(`ReadRequest.offset`/`ReadResult.nextOffset`) 동기 갱신. 페이지네이션 열린 항목 해소(버퍼 상한·스냅샷 시 버퍼 처리는 계속 보류). AC 수·연결 변화 없음. | read=전체 누적 출력(비파괴적) | read=offset 커서 델타(offset=0이면 전체, 비파괴적) |
| 2026-07-04 | **J5-S4 쉘 상태 축적·이어짐 (CRIU) 실 코드**. `Checkpointer` 포트 뒤 K8s-native 실 어댑터(`criu.ContainerCheckpointer` — kubelet `ContainerCheckpoint` API) 구현, `RestoreInto`를 restore-target pod 스펙(`AnnotationRestoreCheckpoint`)으로 분기(빈 쉘 기동 방지), 복원 후 커서 연속성(버퍼-인-체크포인트) 확정, AC-D4 마커 왕복 in-process 검증(`TestScenario4_CRIUIntegrity`) 채움. `criu-verification.md`의 5개 열린 결정을 구체 선택으로 확정, 게이트 on 실검증은 프로비저닝으로 인계. AC 수·연결 변화 없음(AC-D4/B3 구현). | CRIU=미검증 스텁, 5개 결정 미확정, Scenario4=의도적 실패 | CRIU=실 코드(미검증), 5개 결정 확정, Scenario4=마커 왕복(런타임 시 실검증) |
| 2026-07-04 | 체크포인트 저장소(결정 ③)를 **노드 로컬 1차 → S3(내구)** 로 개정. 노드 인스턴스 프로파일을 베이스로 STS AssumeRole(`CHECKPOINT_S3_ROLE_ARN`)로 접근하는 S3 스토어(`internal/adapter/checkpointstore`, aws-sdk-go-v2) 구현. `ContainerCheckpointer`가 kubelet 노드 로컬 아카이브를 업로드하고 `Ref=s3://…`(+`SizeBytes`) 기록, 버킷 미설정 시 노드 로컬 폴백. `criu-verification.md` 결정 ③·실구현 요약·인계 체크리스트 갱신. | 저장소=노드 로컬(1차), 오브젝트 스토리지=후속 | 저장소=S3(assume-role), 노드 로컬은 중간 산물 |
| 2026-08-08 | **V7 "에이전트 세션 — 클로드 코드 CLI" 추가 + PRD-클로드코드(AC-E1~E6) + T-클로드코드 신설**. 세션 워크로드를 단수(쉘)에서 **복수 타입**(`shell` 기본 / `claude-code`)으로 확장: write=프롬프트 1회 실행(`claude --model … -p`, 직렬 큐잉), read=실행 출력(커서 규약은 쉘과 동일), 대화·작업디렉터리 연속성, **CRIU 비대상 — 파일시스템 아카이브로 동결·복원**, 자격증명=Secret 주입·model=세션별. V2·V3 서술을 타입 중립으로 개정, V6를 "기본 타입"으로 격하 표기, AC-A1/A2·B1/B2/B3·C2/C3/C4에 타입 분기 상호참조 추가, PRD-쉘워크로드에 범위(`workloadType=shell`) 한정 명시. | 가치 6개, PRD 4, AC 15, 테스트 4 / 워크로드 타입 단수 | 가치 7개, PRD 5, AC 21, 테스트 5 / 워크로드 타입 복수 |
| 2026-08-08 (2) | **가치 V6·V7 삭제** — 워크로드 타입(인터랙티브 쉘 / 클로드 코드 CLI)을 가치로 올린 것은 과도하게 구체적이라는 판단. 가치 문서에는 타입 중립 품질 가치 V1~V5만 유지하고, "무엇이 도는가"는 PRD 소관으로 이관. 두 워크로드 PRD의 AC 달성 가치를 **상위 AC로부터 상속**하도록 재배선(AC-D1→V1, D2→V3, D3→V3·V4, D4→V3, D5→V2 / AC-E2→V3, E3→V3·V4, E4→V3, E5→V2·V3, E6→V1). 하류 참조(J5 여정, `workspace.html` 매핑, e2e 표, `data-plane/README.md`)도 재연결. PRD·AC·테스트 수는 변화 없음. | 가치 7개, AC 미연결 0 | 가치 5개, AC 미연결 1(AC-E1) |
| 2026-08-08 (4) | **AC-E1 구현 착수 — 워크로드 타입 축이 control plane에 반영됨**. `session.WorkloadType`(`shell` 기본 / `claude-code`) 도입, `CreateSessionRequest`·`Session`·OpenAPI·ConfigMap 레코드에 `workloadType` 추가(미지정=`shell`, 허용값 외 400, 수명 중 불변), `PodOrchestrator.Start`/`RestoreInto`가 타입을 받아 타입별 이미지·`DATA_PLANE_WORKLOAD` env·`workload-type` 라벨로 pod를 분기, 이미지 미설정 타입은 프로비저닝 전 거부. 문서 수·AC 수 변화 없음(구현만 전진). | AC-E1~E6 = 명세만, 구현 0 | AC-E1의 control plane 절반 구현·검증, data plane 절반과 AC-E2~E6은 후속 |
| 2026-08-08 (3) | **V8 "목적에 맞는 작업 환경 선택" 신설** — V6·V7 삭제로 미연결이 된 AC-E1을 뒷받침. V8은 "선택할 수 있다"는 사실만 담고 어떤 환경을 제공하는지는 PRD 소관으로 남겨, 워크로드 타입이 늘어도 가치 문서가 변하지 않도록 설계. `values.md`에 **삭제된 가치 표**(V6·V7 식별자 재사용 금지) 추가. AC·테스트 수 변화 없음. | 가치 5개, AC 미연결 1(AC-E1) | 가치 6개(V1~V5·V8), AC 미연결 0 |
