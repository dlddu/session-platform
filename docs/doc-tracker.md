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
- **read 출력 버퍼 증가 (AC-D3, shell)**: 페이지네이션은 2026-07-03 `offset` 커서 개정으로 해소(반복 read의 `payload`는 델타만큼). shell scrollback은 agent 메모리에 있지만 agent 자체는 CRIU dump 대상이 아니므로 `/checkpoint`가 **CRIU images와 같은 archive에 별도 직렬화**하고 `/restore`가 preload한다. 이 때문에 복원 전 offset cursor가 그대로 유효하다. shell 누적 버퍼 자체의 상한/ring buffer는 계속 보류한다. `claude-code`는 별도 계약으로 invocation 16 MiB·누적 256 MiB 상한과 terminal marker를 구현했다(AC-E2/E3). Live SSE도 같은 append-only byte cursor를 사용해 반복 전송을 줄이지만 저장 상한 자체를 바꾸지는 않는다.
- **제품명·소유자**: "Session Pod Platform"은 임시 작업명. 실제 제품명/소유자 확정 필요.
- **🆕 claude-code의 `idle` 상태 의미 (AC-E5 ↔ AC-C2/C3)**: 상주 프로세스가 없는 타입에서 `active`와 `idle`(pod 보유·미사용)의 실질 차이는 자원 점유뿐이다. 상태 모델은 타입 공통으로 유지했으나, `idle` 구간에 대한 별도 정책(예: 더 짧은 유휴 한계)이 필요한지는 미결.

### ✅ 해결된 설계 결정
- **claude-code 대화 재개 방식 (AC-E4)** — *2026-08-08 확정*. 첫 성공 실행은 새 대화, 이후 실행은 세션별 고정 HOME/workdir에서 `--continue`; 별도 conversation ID는 control-plane에 저장하지 않고 CLI 기록·resume flag를 filesystem archive에 보존한다.
- **claude-code 모델 정책 (AC-E6)** — *2026-08-08 확정, 2026-08-09 개정*. model은 생성 시 불변이며, 미지정은 특정 공급자 버전을 API에 고정하지 않는 `platform-default` 별칭으로 저장한다. 이 별칭의 주 컨테이너는 시작할 때 optional Secret `model`을 `CLAUDE_CODE_MODEL`로 읽고, missing/empty면 CLI `--model`을 생략한다. 구체 세션 model은 literal로 주입되어 Secret 기본값보다 우선한다. singular Secret 변경은 새/복원 pod나 container restart의 다음 시작부터 적용되며 실행 중인 컨테이너를 즉시 바꾸지 않는다.
- **claude-code 모델 catalog 정책 (AC-E6)** — *2026-08-09 확정*. optional Secret `model`과 `models`는 공개 UI 설정이며 Deployment가 키 단위로 control plane의 `CLAUDE_CODE_DEFAULT_MODEL`과 `CLAUDE_CODE_MODELS`에 투영한다. control plane은 JSON shape·model pattern·중복·reserved `platform-default`를 startup 시 엄격히 검증하고, no-store `GET /api/v1/config`로 concrete default 또는 fallback alias와 ordered catalog를 노출한다. catalog는 UI soft catalog이지 API allowlist가 아니며 missing/empty/`[]`는 기존 free-text UI를 유지한다. UI의 default와 catalog 변경은 control-plane rollout 뒤 반영된다. credentials Secret 이름을 바꾸면 두 env의 `secretKeyRef.name`을 같은 literal 이름으로 patch한다.
- **워크로드 타입의 가치 표현 (V6·V7 → V8)** — *2026-08-08 확정*. 워크로드 타입 자체를 가치로 올린 V6·V7은 추상도가 과해(수단을 가치 자리에 둠) 삭제하고, 타입별 명세는 PRD에 남겼다. 그 결과 미연결 상태가 된 AC-E1(타입 선택)은 **선택 가능하다는 사실만** 담는 추상 가치 V8로 재연결했다. 판단 기준: 새 워크로드 타입이 추가돼도 **가치 문서는 변하지 않아야 한다** — V6·V7 방식은 타입마다 가치가 늘어났고, V8은 늘어나지 않는다.
- **세션 워크로드의 정체 (당시 V6 / PRD-쉘워크로드)** — *2026-07-01 확정, 2026-08-08 가치 V6는 삭제되고 명세는 PRD에 유지*. 이전까지 "세션 워크로드"·"인메모리 상태"·"read/write"가 추상적이고 `data-plane`이 미정의(placeholder alpine)였던 상태를 해소. **세션 = 전용 pod에서 PTY에 연결되어 실행되는 인터랙티브 쉘**(기본 `/bin/bash`)로 확정. write=쉘 stdin 입력(AC-D2), read=쉘 stdout/stderr **전체** 출력 비파괴적 반환(AC-D3), 보존 상태=쉘 프로세스 트리(AC-D4), 유휴 기준=클라이언트 쉘 I/O(AC-D5). 기존 AC-A1/B1/B3/C2/C3에 구체화 상호참조 추가, `data-plane/README.md` 갱신.
- **유휴 시간 측정 기준 (부분 해결, AC-D5)** — *2026-07-01*. "마지막 활동"의 정의를 **마지막 클라이언트 read/write(쉘 I/O)** 로 확정. 단, 트리거 *정책*(위 🟡)은 여전히 열림.
- **idle/snapshot read/write 정책 (AC-C2 / AC-C3)** — *2026-06-27 확정, 2026-08-08 타입 축 반영*. 비-active 접근은 통일 "active 보장 후 처리" 규칙: `idle`은 `idle→active` atomic 승격(AC-C1), `snapshot`은 workload별 복원(`shell`=CRIU, `claude-code`=filesystem archive, AC-B2)으로 active 전이 후 read/write.

### 🟠 인접 사슬에서 인지된 항목 (이 문서 관할 밖, 참고용)
- ~~**디자인 사슬 미갱신 (`claude-code` 타입)**~~ → **2026-08-08 해소**: `claude-code` 타입을 다루는 여정(J6)과 mockup이 신설되었다 — 세션 생성 화면에 워크로드 타입·모델 선택이 추가되고, 프롬프트·응답 콘솔(`mockups/agent-workspace.html`)이 생겼다. 상세는 `doc-structure-state.md`.
- ~~**V8의 시각화 부재 (2026-08-08 신규)**~~ → **2026-08-08 해소**: V8이 **J6(작업 환경 선택 + 에이전트 프롬프트 루프)** 로 연결되고 `new-session.html`의 타입 선택 UI로 시각화되어, 디자인 사슬의 "시각화 없는 가치"는 **0**이 되었다. design-doc-structure-validator 영역.

> ✅ mockup의 `model-a`/`model-b`는 non-empty soft catalog의 예시일 뿐이다. 제품 UI는 config catalog가 있으면 concrete default를 `<model> (platform default)`로 합친 ordered picker, catalog가 missing/empty/`[]`이면 concrete default를 안내하는 free-text 입력을 사용한다. 대화 재개 표현은 첫 성공 실행 이후 `--continue` 결정으로 구체화되었다. 시각 정본은 `web/src/` 코드다.
- **V6 삭제에 따른 하류 재연결 (2026-08-08 수행)**: V6를 참조하던 J5 여정·`workspace.html` 매핑·e2e 커버리지 표를 V3으로 재연결했다. 디자인 사슬 문서(`doc-structure-state.md`)는 **참조 재연결만** 반영했고 전체 재검증은 하지 않았다.
- **구현 사슬 반영 (AC-E1~E6)** — *2026-08-09 갱신*: OpenAPI/도메인/model/workload pod 분기, **HTTPS-only** credential-proxy sidecar에 한정된 필수 base URL/token SecretKeyRef, `platform-default` 주 컨테이너의 optional `model` SecretKeyRef와 구체 model literal 우선, strict `CLAUDE_CODE_DEFAULT_MODEL`/`CLAUDE_CODE_MODELS` parser, no-store config API와 soft catalog/free-text UI, multi-workload data-plane 이미지, serial Claude runner, 첫 성공 이후 `--continue`와 고정 HOME/workdir, prompt 1 MiB/413, invocation 16 MiB·누적 256 MiB output marker/507, cursor와 output-full 상태를 보존하는 CRIU 비대상 archive restore, 타입별 SPA route/workspace가 구현되었다. 세션 pod는 전용 `data-plane` ServiceAccount/token을 사용하며 클러스터 범위의 기본 `view` 역할로 일반 리소스를 읽되 Secret 읽기는 거부된다. Claude runner는 `--permission-mode auto --effort xhigh -p --output-format stream-json --verbose --include-partial-messages -- PROMPT`로 실행하며 stdout JSONL 자체가 아니라 `text_delta`와 진단 stderr를 증분 redaction·UTF-8-safe projection으로 저장한다. Credential proxy도 raw upstream 64 MiB 상한 안에서 provider SSE response를 EOF 전 증분 전달하고 response-read 경계의 secret suffix를 tail-safe redaction한다. SPA는 passive workspace SSE의 UTF-8 경계 `output` 이벤트(`id=nextOffset`, `{offset,payloadBase64,nextOffset}`)를 자동 append하고 `Last-Event-ID` 우선 cursor로 재연결하며, past-end cursor의 `reset`은 decoder 폐기·read(0) 전체 교체·새 cursor 재개로 복구한다. 기존 JSON `POST /read`는 reconcile/호환 경로다. SSE 연결·output/reset·keepalive 자체는 `lastAccess`를 갱신하거나 snapshot을 복원하지 않지만 reset reconciliation의 `POST /read`는 일반 Read API대로 idle 승격·`lastAccess` 갱신 의미를 유지한다. snapshot 확인 시 자동 read 없이 Restore 화면으로 전환한다. UI는 연결 상태만 표시하고 서버 run/queue 개수를 추정하지 않는다. archive 외부 전송은 `CLAUDE_CODE_ARCHIVE_ENABLED` opt-in(기본 off)이다. singular `model` 변경은 새/복원 pod 또는 container restart의 다음 시작부터, plural `models`와 UI에 표시되는 singular `model` 변경은 control-plane rollout 뒤 반영된다. custom credentials Secret 이름은 두 공개 설정 env의 `secretKeyRef.name`에도 동일하게 patch한다. fake runner·단위/통합·Playwright route fixture로 결정적 계약을 검증하며, 실제 외부 Claude API 배포 smoke는 아직 별도 opt-in 범위다.
- ~~**AC-C1 Lease/metadata 안전 부채**~~ → **2026-08-08 해소**: Snapshot·Restore·recovery가 기본 15초 Lease를 5초 heartbeat + per-Renew deadline으로 갱신하고, holder loss는 operation context를 취소한다. `Touch`는 latest-only monotonic partial update, lifecycle은 state + private snapshot transaction을 한 번에 owner-fenced aggregate CAS하며 더 최신 `lastAccess`를 merge한다. Claude snapshot은 CP-owned generation의 durable `preparing→committing` transaction으로 crash recovery한다.
- **reaper access freshness 안전 부채**: `SnapshotIfIdle`은 Lease를 잡고 authoritative `lastAccess`를 재조회해 List 이후 이미 완료된 접근은 걸러낸다. 그러나 read/write는 그 Lease를 공유하지 않고 agent I/O 뒤 `Touch`가 best-effort라, 재조회와 checkpoint 사이에 완료되는 접근은 여전히 예상보다 일찍 동결될 수 있다. read/write fencing 또는 실패를 표면화하는 정책이 필요하다.
- **restore hard-crash 안전 부채 (모든 workload)**: `RestoreInto`가 새 pod를 만든 뒤 final aggregate CAS 전에 control plane이 hard crash하면 target pod를 orphan할 수 있다. 일반 오류·Renew loss는 best-effort Stop으로 정리하지만 process crash에는 durable RestoreTransaction, deterministic target 또는 reconciler가 필요하다.
- **snapshot 실패 안전 부채 (shell)**: CRIU dump가 shell을 종료한 뒤 durable upload·pod Stop·final metadata 저장이 실패하면 record와 pod 상태가 어긋날 수 있다. Claude archive는 owner-fenced prepare/commit recovery를 갖지만 shell은 leave-running 또는 명시적 transition/reconcile 프로토콜이 필요하다.
- **delete retention/crash-window 안전 부채**: 공개 DELETE는 lifecycle Lease heartbeat 아래 live/current/source pod를 회수하고 레코드를 제거한다. 다만 pod Stop과 metadata Delete 사이 hard crash에는 durable `deleting` marker/reconciler가 없고, 이미 업로드된 checkpoint/archive object는 checkpoint-store retention 정책에 남는다.

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
| 2026-07-04 | **J5-S4 쉘 상태 축적·이어짐 (CRIU) 실 코드**. `Checkpointer` 포트 뒤 K8s-native 실 어댑터(`criu.ContainerCheckpointer` — kubelet `ContainerCheckpoint` API) 구현, `RestoreInto`를 restore-target pod 스펙(`AnnotationRestoreCheckpoint`)으로 분기(빈 쉘 기동 방지), 복원 후 cursor 연속성(scrollback을 CRIU images와 같은 archive에 직렬화) 확정, AC-D4 마커 왕복 in-process 검증(`TestScenario4_CRIUIntegrity`) 채움. `criu-verification.md`의 5개 열린 결정을 구체 선택으로 확정, 게이트 on 실검증은 프로비저닝으로 인계. AC 수·연결 변화 없음(AC-D4/B3 구현). | CRIU=미검증 스텁, 5개 결정 미확정, Scenario4=의도적 실패 | CRIU=실 코드(미검증), 5개 결정 확정, Scenario4=마커 왕복(런타임 시 실검증) |
| 2026-07-04 | 체크포인트 저장소(결정 ③)를 **노드 로컬 1차 → S3(내구)** 로 개정. 노드 인스턴스 프로파일을 베이스로 STS AssumeRole(`CHECKPOINT_S3_ROLE_ARN`)로 접근하는 S3 스토어(`internal/adapter/checkpointstore`, aws-sdk-go-v2) 구현. `ContainerCheckpointer`가 kubelet 노드 로컬 아카이브를 업로드하고 `Ref=s3://…`(+`SizeBytes`) 기록, 버킷 미설정 시 노드 로컬 폴백. `criu-verification.md` 결정 ③·실구현 요약·인계 체크리스트 갱신. | 저장소=노드 로컬(1차), 오브젝트 스토리지=후속 | 저장소=S3(assume-role), 노드 로컬은 중간 산물 |
| 2026-08-08 | **V7 "에이전트 세션 — 클로드 코드 CLI" 추가 + PRD-클로드코드(AC-E1~E6) + T-클로드코드 신설**. 세션 워크로드를 단수(쉘)에서 **복수 타입**(`shell` 기본 / `claude-code`)으로 확장: write=프롬프트 1회 실행(`claude --model … -p`, 직렬 큐잉), read=실행 출력(커서 규약은 쉘과 동일), 대화·작업디렉터리 연속성, **CRIU 비대상 — 파일시스템 아카이브로 동결·복원**, 자격증명=Secret 주입·model=세션별. V2·V3 서술을 타입 중립으로 개정, V6를 "기본 타입"으로 격하 표기, AC-A1/A2·B1/B2/B3·C2/C3/C4에 타입 분기 상호참조 추가, PRD-쉘워크로드에 범위(`workloadType=shell`) 한정 명시. | 가치 6개, PRD 4, AC 15, 테스트 4 / 워크로드 타입 단수 | 가치 7개, PRD 5, AC 21, 테스트 5 / 워크로드 타입 복수 |
| 2026-08-08 (2) | **가치 V6·V7 삭제** — 워크로드 타입(인터랙티브 쉘 / 클로드 코드 CLI)을 가치로 올린 것은 과도하게 구체적이라는 판단. 가치 문서에는 타입 중립 품질 가치 V1~V5만 유지하고, "무엇이 도는가"는 PRD 소관으로 이관. 두 워크로드 PRD의 AC 달성 가치를 **상위 AC로부터 상속**하도록 재배선(AC-D1→V1, D2→V3, D3→V3·V4, D4→V3, D5→V2 / AC-E2→V3, E3→V3·V4, E4→V3, E5→V2·V3, E6→V1). 하류 참조(J5 여정, `workspace.html` 매핑, e2e 표, `data-plane/README.md`)도 재연결. PRD·AC·테스트 수는 변화 없음. | 가치 7개, AC 미연결 0 | 가치 5개, AC 미연결 1(AC-E1) |
| 2026-08-08 (4) | **AC-E1 구현 착수 — 워크로드 타입 축이 control plane에 반영됨**. `session.WorkloadType`(`shell` 기본 / `claude-code`) 도입, `CreateSessionRequest`·`Session`·OpenAPI·ConfigMap 레코드에 `workloadType` 추가(미지정=`shell`, 허용값 외 400, 수명 중 불변), `PodOrchestrator.Start`/`RestoreInto`가 타입을 받아 타입별 이미지·`DATA_PLANE_WORKLOAD` env·`workload-type` 라벨로 pod를 분기, 이미지 미설정 타입은 프로비저닝 전 거부. 문서 수·AC 수 변화 없음(구현만 전진). | AC-E1~E6 = 명세만, 구현 0 | AC-E1의 control plane 절반 구현·검증, data plane 절반과 AC-E2~E6은 후속 |
| 2026-08-08 (3) | **V8 "목적에 맞는 작업 환경 선택" 신설** — V6·V7 삭제로 미연결이 된 AC-E1을 뒷받침. V8은 "선택할 수 있다"는 사실만 담고 어떤 환경을 제공하는지는 PRD 소관으로 남겨, 워크로드 타입이 늘어도 가치 문서가 변하지 않도록 설계. `values.md`에 **삭제된 가치 표**(V6·V7 식별자 재사용 금지) 추가. AC·테스트 수 변화 없음. | 가치 5개, AC 미연결 1(AC-E1) | 가치 6개(V1~V5·V8), AC 미연결 0 |
| 2026-08-08 (5) | **AC-E1~E6 제품 경로 구현** — model/workload 계약과 Secret pod 주입, multi-workload data plane, 안전한 positional prompt argv, serial queue·cursor·resume, filesystem archive, workload별 SPA/J6 테스트를 추가. gate-off synthetic snapshot의 pod 삭제를 차단하고 ID entropy를 128-bit로 확대. | AC-E1 control-plane 절반, AC-E2~E6 명세만 | AC-E1~E6 코드 경로 + 결정적 단위/UI 계약 검증, 실제 provider smoke·AC-C1 전이 fencing은 후속 |
| 2026-08-08 (6) | **Claude quota + crash-safe lifecycle 보강** — prompt 1 MiB/413, invocation 16 MiB truncation marker, cumulative 256 MiB terminal marker/507, accepted queue drain과 archive cursor/full-state 보존을 추가. Secret은 HTTPS-only loopback sidecar에만 두고, archive snapshot은 CP-owned generation의 durable `preparing→committing` owner fence로 복구한다. Snapshot/Restore/recovery Lease heartbeat·per-Renew timeout, partial Touch, aggregate CAS, reaper cutoff 재검증을 추가했다. | prompt/output 무상한, agent-owned generation, 15초 Lease 무갱신, split metadata update | bounded prompt/output + public 413/429/507, owner-fenced crash recovery, renewable Lease + aggregate CAS; restore hard-crash orphan·shell post-dump·reaper in-flight access race는 추적 |
| 2026-08-08 (7) | **공개 세션 삭제 경로 추가** — `DELETE /sessions/{id}`(204/404/409), lifecycle Lease heartbeat·transaction owner claim·current/source pod 회수, ConfigMap 삭제 후 token-safe Unlock, 목록/Workspace/Restore 확인 UI와 실패 재시도·키보드 포커스 Playwright 계약을 추가했다. | 내부 `Terminate`만 존재, 공개 API/UI 없음 | 공개 API/OpenAPI/SPA/단위·브라우저 테스트 연결; 외부 archive retention과 Stop→metadata hard-crash window는 추적 |
| 2026-08-09 | **Claude 응답 live streaming** — credential proxy의 provider SSE도 EOF 전에 chunk-safe redaction·raw-byte 상한을 적용해 전달하고, runner의 partial stream-json을 사용자 출력으로 증분 투영하며, passive cursor SSE·자동 재연결·read reconcile·stale-cursor reset·snapshot Restore 전환을 제품/여정/mockup/테스트 계약에 연결했다. | proxy/invocation 종료 뒤 일괄 append + SPA bounded polling/수동 Refresh | provider→proxy→runner→SSE 전 구간 자동 append + UTF-8 경계 byte cursor·reset 전체 replay; 문서 수·AC 수 변화 없음 |
