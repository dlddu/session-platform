# Session Pod Platform 문서 체계 상태 추적

## 현재 상태 요약
- 정의된 가치: **6개** (V1~V6) — V6에서 "세션 = 인터랙티브 쉘" 확정
- PRD: **4개** (아키텍처/라이프사이클/상태·API/쉘 워크로드)
- Acceptance Criteria: **15개** (가치 연결됨: 15개 / 미연결: 0개)
- 테스트 문서: **4개** (AC 커버됨: 15개 / 미커버: 0개)
- **건강 상태**: ⚠️ **위험 있음** — 모든 가치의 소유자가 미지정(고아 가치 6개). 구조적 연결(가치→PRD→AC→테스트)은 완전.

## 연결 매트릭스

| 가치 | PRD | AC | 테스트 | 상태 |
|------|-----|-----|--------|------|
| V1: 세션 격리 | PRD-아키텍처, PRD-쉘워크로드 | AC-A1, AC-A2, AC-D1 | T-아키텍처, T-쉘워크로드 | ✅ 완전 |
| V2: 유휴 자원 회수 | PRD-아키텍처, PRD-라이프사이클, PRD-쉘워크로드 | AC-A3, AC-B1, AC-D5 | T-아키텍처, T-라이프사이클, T-쉘워크로드 | ✅ 완전 |
| V3: 끊김 없는 연속성 | PRD-라이프사이클, PRD-상태·API, PRD-쉘워크로드 | AC-B2, AC-B3, AC-C2, AC-C3, AC-D4 | T-라이프사이클, T-상태·API, T-쉘워크로드 | ✅ 완전 |
| V4: 자유로운 멀티세션 전환 | PRD-상태·API | AC-C2, AC-C4 | T-상태·API | ✅ 완전 |
| V5: 일관된 세션 상태 | PRD-아키텍처, PRD-상태·API | AC-A1, AC-C1, AC-C3 | T-아키텍처, T-상태·API | ✅ 완전 |
| V6: 인터랙티브 쉘 세션 | PRD-쉘워크로드 | AC-D1, AC-D2, AC-D3, AC-D4, AC-D5 | T-쉘워크로드 | ✅ 완전 |

> 가치 → PRD → AC → 테스트의 모든 연결은 이어져 있음. 끊어진 화살표는 소유자 부재(아래) 하나뿐.
> PRD-쉘워크로드의 AC-D1~D5는 신규 메커니즘이 아니라 기존 AC의 **구체화 연결**을 함께 가진다:
> AC-D1→AC-A1, AC-D2→AC-C3, AC-D3→AC-C2, AC-D4→AC-B3, AC-D5→AC-B1.

## 위험 진단

### 🔴 고아 가치 (소유자 없는 가치)
- **V1~V6 전부** — 제품 소유자가 미지정 상태. 가치의 책임 소재가 불분명함. **소유자 지정 필요.**

### 미정렬 문서 (가치 참조 없는 문서)
- (없음)

### 무가치 PRD (가치를 달성하지 않는 PRD)
- (없음)

### AC 없는 PRD
- (없음)

### 미연결 AC (가치와 연결되지 않은 AC)
- (없음)

### 미검증 AC (테스트 없는 AC)
- (없음)

### 고아 테스트 (AC를 참조하지 않는 테스트)
- (없음)

### 🟡 확인 필요한 설계 결정 (스킬 표준 위험은 아니나 명세 공백)
- **스냅샷 트리거 정책 (AC-B1)**: 60분은 최대 유휴 한계로 확정됐으나, grace period·per-session override 등 정확한 트리거 타이밍은 미확정 (`session.go`의 `TODO(policy)`).
- **바쁜 쉘 동결 여부 (AC-D5 ↔ AC-B1)**: 장시간 포그라운드 작업 실행 중 클라이언트 유휴 60분 도달 시 동결 대상이 되는지 — CRIU상 기술적으로는 가능하나, 트리거 정책과 함께 결정 필요. (AC-D5로 유휴 "정의"는 확정, "정책"은 미확정)
- **read 출력 버퍼 증가 (AC-D3)**: 페이지네이션은 2026-07-03 `offset` 커서 개정으로 해소(반복 read의 `payload`는 델타만큼). 스냅샷 시 버퍼 처리 중 **복원 후 offset 커서 유효성**은 2026-07-04(J5-S4) 확정 — scrollback이 에이전트 메모리 상주라 CRIU 체크포인트에 포함되어 복원 후에도 커서 유효(버퍼-인-체크포인트). 누적 버퍼 자체의 상한/ring buffer는 계속 보류.
- **제품명·소유자**: "Session Pod Platform"은 임시 작업명. 실제 제품명/소유자 확정 필요.

### ✅ 해결된 설계 결정
- **세션 워크로드의 정체 (V6 / PRD-쉘워크로드)** — *2026-07-01 확정*. 이전까지 "세션 워크로드"·"인메모리 상태"·"read/write"가 추상적이고 `data-plane`이 미정의(placeholder alpine)였던 상태를 해소. **세션 = 전용 pod에서 PTY에 연결되어 실행되는 인터랙티브 쉘**(기본 `/bin/bash`)로 확정. write=쉘 stdin 입력(AC-D2), read=쉘 stdout/stderr **전체** 출력 비파괴적 반환(AC-D3), 보존 상태=쉘 프로세스 트리(AC-D4), 유휴 기준=클라이언트 쉘 I/O(AC-D5). 기존 AC-A1/B1/B3/C2/C3에 구체화 상호참조 추가, `data-plane/README.md` 갱신.
- **유휴 시간 측정 기준 (부분 해결, AC-D5)** — *2026-07-01*. "마지막 활동"의 정의를 **마지막 클라이언트 read/write(쉘 I/O)** 로 확정. 단, 트리거 *정책*(위 🟡)은 여전히 열림.
- **idle/snapshot read/write 정책 (AC-C2 / AC-C3)** — *2026-06-27 확정*. 비-active 접근은 통일 "active 보장 후 처리" 규칙: `idle`은 `idle→active` atomic 승격(AC-C1), `snapshot`은 CRIU 복원(AC-B2)으로 active 전이 후 read/write.

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
