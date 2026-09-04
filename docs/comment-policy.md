# 주석 정책 — 복원 가능한 주석은 남기지 않는다

> 이 문서는 `dlddu/session-platform`의 **주석 판정 기준(SSOT)** 이다. 어떤 주석을 남기고 어떤
> 주석을 지울지에 대한 판단은 개별 리뷰의 취향이 아니라 이 문서를 근거로 한다.
> 판정을 뒤집고 싶으면 **개별 주석이 아니라 이 문서를 먼저 고친다.**

## 왜 이 정책이 있나

주석은 아무도 검증하지 않는다. 코드·문서·테스트는 CI가 붙잡지만, 주석이 코드와 어긋나도
빌드는 초록이다. 그래서 **다른 곳에서 확인할 수 있는 내용을 되풀이하는 주석**은 값이 없는 데서
그치지 않는다 — 원본만 고쳐질 때 조용히 거짓이 되어, 다음 사람을 틀린 방향으로 이끈다.

정합 상태 = 판정 대상 범위의 주석 중 **복원 가능한 내용을 담은 것이 없음.**

## 판단 기준

### 복원 경로는 넷이다

읽는 사람이 아래 넷 중 **어디서든** 같은 내용을 확인할 수 있으면, 그 내용은 주석의 자리가 아니다.

| # | 복원 경로 | 예 |
| --- | --- | --- |
| ① | **코드 자체** | 시그니처, 상수값, 정규식 quantifier, 바로 아래 분기 |
| ② | **저장소 문서** | `docs/prd/*`의 AC, `docs/test/*`, `docs/doc-tracker.md`의 결정 기록 |
| ③ | **PR 제목·본문·리뷰 코멘트** | 어떤 대안을 왜 버렸는지 |
| ④ | **커밋 메시지** | 어떤 결정이 언제 뒤집혔는지 |

### 유지 대상 (이 밖은 제거 후보)

1. **기계가 읽는 주석** — 정책 대상이 아니며 판정에서 제외한다:
   `//go:` · `nolint` · `eslint-` · `@ts-` · `검증 AC:` · `mock-exception:` · `mockup:`
   (각각 Go 툴체인 · `scripts/e2e/check-ac-mapping.sh` · `scripts/check-render-fidelity.py` ·
   `scripts/check-fidelity-allowlist.py`가 파싱한다. 지우면 게이트가 깨진다.)
2. **doc 주석** — exported Go 식별자는 Go 관례대로 **식별자 이름으로 시작하는 1줄** doc 주석과
   패키지 주석을 유지한다. 다만 **그 1줄이 시그니처를 그대로 옮겨 적기만 하면**
   (`CreateRequest is the input to Manager.Create.`) 관례를 채우는 것이 아니라 선언을
   되풀이하는 것이므로 제거 후보다. TS/TSX에는 doc 주석 의무가 없다.
3. **복원 불가능한 지식** — CRIU·커널·컨테이너 런타임 제약, **온클러스터에서 관측한 사실**
   (날짜·환경이 붙은 것), 동시성·락·순서 계약, 코드 형태만으로는 보이지 않는 함정,
   외부 시스템(k8s API·CRIU·Claude Code)의 문서화되지 않은 동작.

### 운영 규칙 — 포인터는 남기고, 포인터가 가리키는 내용의 사본은 지운다

가장 자주 마주치는 형태는 「출처를 밝히면서 그 내용을 함께 옮겨 적은」 주석이다.

```go
// The state machine (see docs/prd/lifecycle.md, docs/prd/state-api.md):
//
//	active  ──idle 60m──▶ idle ──idle 60m total──▶ snapshot
//	...
```

**주석이 스스로 출처를 밝히고 있다는 것은 그 내용이 복원 가능하다는 자백이다.** 이때
`see docs/...`라는 **경로(포인터)는 남기고**, 그 뒤에 옮겨 적은 **사본은 지운다.** 포인터는
재진술이 아니라 SSOT로 가는 길이고, 사본은 원본이 고쳐질 때 어긋나는 쪽이다.
위 예에서 `idle 60m`은 PRD(AC-B1)·`MaxIdle` 상수·이 다이어그램 **세 곳**에 있었다.

### 충돌 시 기본 방향

코드·문서·이력이 SSOT다. 주석이 그것들과 어긋나면 **주석이 틀린 것으로 본다.**

- 복원 가능하면 → 지운다.
- 복원이 안 되는 이유가 **원본의 부실**이면 → 주석을 남기는 대신 **원본(문서·이름·커밋 메시지)을 고친다.**
- 복원 불가능하면 → 남긴다.
- **애매하면 남긴다.** 이 유형은 제거 쪽으로 기울지 않는다. 중복 주석의 비용은 작지만, 유일한
  지식을 담은 주석을 지우면 **그 지식은 어디에도 남지 않는다.** 비용이 비대칭이므로 판단이
  갈리는 주석은 보존하고, **갈렸다는 사실**을 아래 판정 이력에 남긴다.

## 판정 절차

1. 판정 범위를 **파일 단위로** 정한다(`control-plane` · `data-plane` · `web/src` · `web/e2e`).
   한 번에 전 범위를 훑지 않는다 — 초기 격차가 크다는 것은 알려진 사실이고, 한 번에 좁힐
   대상이 아니다.
2. 범위 안의 주석을 **한 줄도 빠짐없이** 읽는다. 「흔한 중복 유형 목록」에서 찾지 않는다 —
   그런 목록은 가설이지 판정이 아니다.
3. 제거하는 줄마다 **좌표**를 적는다: 어느 파일 몇 줄이, 또는 어느 문서의 어느 AC가 이미 이
   내용을 말하는가. 좌표 없이 「중복이다」라고만 적으면 리뷰가 불가능하다.
4. 유지 판정도 판정이다. 특히 **판단이 갈린 것**은 왜 갈렸는지를 남긴다 — 다음 판정의 입력이 된다.
5. 판정한 줄 수(제거 + 유지)를 아래 이력에 기록한다. **성과는 지운 줄 수가 아니라 판정된 줄 수다.**

## 판정 이력

> **범위는 줄 수와 측정 커밋까지 적는다.** 디렉터리 이름만 적은 행은 「이 경로는 판정 완료」로
> 오독된다 — 판정을 잰 커밋과 머지 커밋 사이에 다른 PR이 그 경로에 주석을 더하면, 그 증분은
> 어느 행에도 속하지 않은 채 영원히 미판정으로 남는다. 자매 레포에서 실제로 그렇게 됐다.

| 날짜 | 범위 (판정 줄 수 @ 측정 커밋) | 판정 | 제거 | 유지 |
| --- | --- | --- | ---: | ---: |
| 2026-09-04 | `control-plane/internal/session/` — `session.go`·`manager.go`, 162줄 @ `acde617` | 첫 판정 패스 | 19 | 143 |
| 2026-09-04 | `control-plane/internal/service/` — 6파일 전부, 299줄 @ `539819e` | 2차 판정 패스 | 75 | 224 |

### 2026-09-04 — `control-plane/internal/session/` (판정 162줄)

**제거 19줄** — 각 항목의 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `session.go` `State` | 상태기계 ASCII 다이어그램 + 「전이는 atomic해야 한다」 | ② `docs/prd/lifecycle.md` AC-B1(60분 유휴→snapshot)·AC-B2(접근→복원→active), `docs/prd/state-api.md` AC-C1(atomic 전이). **주석이 스스로 이 둘을 인용하고 있었다.** `idle 60m`은 ① `MaxIdle` 상수에도 있다 |
| `session.go` `WorkloadType` | AC-A1·AC-A2의 괄호 해설(「control plane은 워크로드를 직접 실행하지 않는다」, 「1:1 워크로드 파드 + 세션 전속 보조 파드」) | ② `docs/prd/architecture.md` AC-A1 설명 · AC-A2 「보조 파드」 절이 같은 내용을 더 자세히 적는다 |
| `session.go` `WorkloadTypeApprovalGated` | 「claude-code와 같은 one-shot 실행 모델이고 프록시가 워크로드 파드 밖으로 나간다」 | ② `docs/prd/approval-gated-workload.md` AC-F1(「`model` 필드의 계약도 AC-E6을 그대로 따른다」)·AC-F2(egress 격리와 프록시 배치), `docs/doc-tracker.md` 「공급자 프록시의 배치 (AC-F2 ↔ AC-F4/F6)」 해결된 결정 |
| `session.go` `PlatformDefaultModel` | 「벤더 모델 버전을 API 계약에 박지 않기 위해 별칭을 쓴다」는 근거 | ② `docs/doc-tracker.md` 「claude-code 모델 정책 (AC-E6)」 — *「특정 공급자 버전을 API에 고정하지 않는 `platform-default` 별칭」* 거의 축자 일치 |
| `session.go` `Session.WorkloadType` 필드 | 「생성 시 확정·이후 불변」과 「타입 축 이전 저장본은 ""로 디코드된다」 | ① 같은 파일 `WorkloadType` 타입 doc(불변성)과 `NormalizeWorkloadType` doc(저장본 디코딩)이 각각 이미 말한다 |
| `manager.go` `CreateRequest` | `CreateRequest is the input to Manager.Create.` | ① 시그니처 `Create(ctx, req CreateRequest)` 그 자체 — 선언 재진술의 교과서적 형태 |
| `manager.go` `Manager` | 「design docs의 "SessionManager"가 이것이다」 | ① 같은 패키지의 패키지 주석(`session.go` 첫 문단)이 *「the SessionManager port that the REST API depends on」* 으로 이미 말한다 |

**유지 143줄** — 아래는 「지울까」를 실제로 검토했다가 남긴 것들이다(갈린 이유를 함께 남긴다).

- **`Manager`의 `AC mapping:` 블록(12줄)** — 메서드→AC 대응표. 개별 대응은 ②로 복원되지만
  (`state-api.md`가 read/write/switch를, `architecture.md`가 create/terminate를,
  `lifecycle.md`가 snapshot/restore를 각각 적는다), **Go 메서드에서 AC로 가는 방향의 색인은
  어느 문서에도 한 덩어리로 존재하지 않는다.** 갈렸으므로 남겼다. 이 표가 실제로 썩기
  시작하면(메서드가 늘었는데 표가 그대로면) 그때가 제거 시점이다.
- **`MaxIdle`의 `TODO(policy)`(6줄)** — 작업 흔적처럼 보이지만 `docs/doc-tracker.md`의
  「스냅샷 트리거 정책 (AC-B1)」 항목이 **이 주석을 거꾸로 참조한다**(*「`session.go`의
  `TODO(policy)`」*). 원장이 가리키는 앵커라 지우면 원장의 참조가 끊긴다. 유지.
- **`modelNamePattern`의 「Model identifiers are at most 128 characters」** — 정규식
  `{0,127}`이 ①로 복원한다고 볼 수도 있으나, **그 quantifier가 룬을 세는지 바이트를 세는지는
  코드가 말하지 않는다.** 주석의 「characters」가 그 모호함을 없앤다. 유지. 이어지는 OpenRouter
  `~` 별칭 설명은 외부 시스템 지식이라 명백히 유지 대상이다.
- **`SnapshotPhase`·`SnapshotTransaction`의 복구 계약(약 12줄)** — preparing/committing이 각각
  무엇을 보장하는지는 크래시 복구의 순서 계약이고, 코드 형태만으로는 보이지 않는다. 유지.
- **`State` 상수 3종·`Valid`·`Pods`의 1줄 doc** — Go 관례를 채우는 최소 doc. 유지.

**정정 — 「중복 유형」은 가설이었다.** 이 모델을 등록할 때 표본에서 관측했다고 적은 세 유형
(① 선언 재진술 ② 문서·AC 재진술 ③ 작업 흔적) 중, 레포 전역 실측에서 **① 순수 선언 재진술은
사실상 1건**(`manager.go`의 `CreateRequest`)뿐이었고 ②의 `Scenario N (AC-..)` 헤더도 9건이었다.
이 레포의 주석은 「같은 말을 두 번 쓰는」 형태보다 **「출처를 밝히면서 그 내용을 함께 옮겨 적는」**
형태가 압도적으로 많다 — 그래서 위 「포인터는 남기고 사본은 지운다」가 이 레포의 주된 판정
도구가 된다. 다음 판정도 **유형 목록에서 찾지 말고 범위를 통째로 읽을 것.**

### 2026-09-04 — `control-plane/internal/service/` (판정 299줄)

도메인 코어(`internal/session/`) 다음의 서비스 계층. 6파일 전부를 통째로 읽었다 —
`manager.go`(152) · `manager_test.go`(63) · `reaper.go`(25) · `auxiliary_pods_test.go`(32) ·
`workload_type_test.go`(24) · `reaper_test.go`(8).

**측정 정정 — 지문이 세는 304줄 중 5줄은 주석이 아니다.** as-is 지문의 패턴
`^[[:space:]]*(//|/\*|\*[^/])`는 Go의 **포인터 역참조문**(`*snapshotted = true`, `manager.go` 2곳)과
**임베드 필드**(`*agent.StubClient` 등 3곳)도 주석으로 센다. 그래서 이 패키지의 실제 판정 대상은
**299줄**이고, 위 표의 숫자는 그 299 기준이다(지문 기준으로는 304 → 229). 이 오검출은 레포 전체에
**36줄** 있다. 「사각지대」(줄 끝 주석·raw string)와 반대 방향의 결함이라 정의가 유예한 항목에
포함되지 않는다 — 별도 후속으로 넘긴다.

**제거 75줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `manager.go` 패키지 주석 (12→3) | resume-on-access 3분기 서술 · 워크로드별 read/write 의미 · `TODO(policy)` 재진술 | ② `prd/state-api.md` AC-C2/AC-C3의 분기 목록과 「구체화」 문단이 축자에 가깝다 · ① 같은 파일의 `activate`·`Read`·`Write` doc이 각각 다시 말한다 · ① `session.MaxIdle`의 `TODO(policy)` |
| `manager.go` `Service` (4→1) | 「pod 연산은 orchestrator, 상태 변경은 store, 아카이브는 checkpointer, I/O는 agent client」 | ① 바로 아래 필드 선언 `orch k8s.PodOrchestrator`·`store store.StateStore`·`ckpts …criu.Checkpointer`·`agent agent.Client` — 타입 이름이 그 배분을 그대로 말한다 |
| `manager.go` `var _ session.Manager` (1→0) | `compile-time assertion that Service satisfies the port.` | ① 그 선언 자체가 컴파일타임 단언이다 |
| `manager.go` `Create` (2→1) | `AC-E1: an omitted type creates a shell session …; an unknown one is rejected` | ① `session.NormalizeWorkloadType` doc — *「omitted workloadType creates a shell session … rejects any other unknown value」* 축자 일치 |
| `manager.go` `activate` (6→3) | active/idle/snapshot 3분기 열거 + 「Switch는 read/write 없는 activate」 | ① 바로 아래 `switch sess.State`의 3 case · ① 같은 파일 `Switch` · ② AC-C2의 분기 목록 |
| `manager.go` `Read` (3→2) | `Offset 0 replays the full session history; reads are non-consuming.` | ② AC-C2 구체화 — *「append-only byte offset 커서 규약(비파괴·`offset=0`=전체)」* |
| `manager.go` `Stream` (5→4) | 「SPA가 실패한 소스를 닫고 사용자에게 복원을 묻는다」 | ② `prd/state-api.md`의 📌 passive stream 문단 · `prd/lifecycle.md` AC-B1 검증 방법이 같은 SPA 동작을 적는다. **포인터만 남겼다** |
| `manager.go` `Write` (4→2) | 「shell은 stdin, Claude는 직렬 프롬프트 큐」 · 「수락 후 반환」 | ② AC-C3 구체화(워크로드별 write 의미) · AC-C3 「비블로킹 반환 규약」 |
| `manager.go` `Switch` (4→2) | 「activate 코어를 공유해 셋이 동일하게 재개」 · 「전환이 격리를 깨지 않는다(AC-A2)」 | ① 본문이 `s.activate(...)`를 부른다 · ② AC-C2의 *「switch(AC-C4)와 동일한 "접근 시 active화" 원칙」* |
| `manager.go` 파드 회수 주석 4곳 (10→0) | 「세션이 소유한 모든 파드 — 워크로드 파드 + 수명이 세션에 묶인 보조 파드 — 를 회수 (AC-A3, AC-F4)」의 **네 번 반복** | ① 같은 파일 `sessionPodRefs`/`sessionReclaimRefs`의 doc이 이미 그 집합의 정의다 · ② `prd/architecture.md` AC-A3 *「워크로드 파드와, 있다면 보조 파드」* |
| `manager.go` `stopPodsBestEffort`·`Create` Reach (5→3) | 보조 파드 설명의 재진술 | ② AC-A2 「보조 파드」 절 *「보조 파드는 세션 워크로드를 실행하지 않고」* |
| `reaper.go` `IdleReaper` (15→5) | 스캔 동작 서술 · `/snapshot` 엔드포인트와의 대비 · `TODO(policy)` 5줄 사본 | ① `ScanOnce` doc과 본문 · ① `Service.Snapshot` doc(*「Explicit snapshots have no idle precondition」*) · ① `session.MaxIdle`의 `TODO(policy)` — **원본은 doc-tracker가 앵커로 참조해 1차 패스가 보존한 그 블록이다.** 포인터만 남겼다 |
| `reaper.go` `SnapshotIfIdle` 재진술 (2→0) | 「Lease를 잡고 LastAccess를 다시 읽는다」 · 「일반 매니저는 Snapshot을 쓴다」 | ① `Service.SnapshotIfIdle` doc이 같은 계약을 말한다 · ① `ScanOnce`의 `idleSnapshotManager` 타입 단언 |
| `reaper.go` `NewIdleReaper`·`Run`·`ScanOnce` (3→0) | 「테스트가 시계를 주입한다」 · 「SIGINT/SIGTERM에 깨끗이 멈춘다」 · 「단일 tick을 위해 export했다」 | ① `reaper_test.go` · ① `cmd/control-plane/main.go:152`의 `signal.NotifyContext(…SIGINT, SIGTERM)` · ① export 여부는 선언이 말한다 |
| `manager_test.go` 헬퍼·테스트 doc 9곳 (32→17) | 본문이 그대로 단언하는 서술(«pod가 회수되고 새 pod가 생긴다», «active는 그대로, idle은 승격, snapshot은 복원») | ① 각 테스트 본문의 단언과 `res.Path` 기대값 · ② `docs/test/lifecycle.md` 시나리오 1·2, AC-C2/AC-C3 |
| `workload_type_test.go` doc 5곳 (13→8) | 「저장본은 shell 기본값으로 읽혀야 한다」 등 | ① `NormalizeWorkloadType` doc — *「records written before the type axis existed … resolves to shell, the only type those sessions could have been」* 축자 일치 |
| `auxiliary_pods_test.go` doc 4곳 (14→9) | 파일 상단의 테스트 목록 재진술 · AC-F4 인용문 · 「보조 파드는 상태를 갖지 않아 복원이 아니라 재생성」 | ① 바로 아래 테스트 **함수 이름들**(`TestSnapshotReclaims…`·`TestTerminateReclaims…`·`TestRestoreProvisions…`·`TestFailedCreateReclaims…`) · ② AC-F4 · ① `manager.go` `Restore`의 같은 설명(그쪽을 정본으로 남겼다) |
| `reaper_test.go` doc (7→4) | 「59분 경계에서는 동결되지 않는다」 · 「ticker 대신 ScanOnce 한 번을 돌린다」 | ② **주석이 스스로 인용한** `docs/test/lifecycle.md` 시나리오 1(*「경계값(예: 59분)에서는 동결되지 않으며」*) · ① 본문. 포인터만 남겼다 |

**유지 224줄** — 「지울까」를 검토했다가 남긴 것들.

- **`renewLeaseContext` 주변(약 20줄)** — 「wedged된 요청이 15초 Lease 만료 전에 실패해야 한다」,
  「`stopRenewal`은 정상 종료이지 소유권 상실이 아니다」. 시간 순서 계약이고 코드 형태로는 보이지 않는다.
- **스냅샷 트랜잭션의 복구 계약(약 35줄)** — preparing/committing 각 단계에서 무엇이 내구화되는지,
  DELETE 결과 모호성을 왜 abort가 아니라 commit 쪽으로 해석하는지, 만료된 소유자가 왜 밑에서
  abort하면 안 되는지. `prd/state-api.md` AC-C1은 「atomic 전이」만 말하고 이 순서는 말하지 않는다.
- **`Restore`의 CAS 응답 유실 분기(약 8줄)** — 「API server는 커밋했는데 응답이 유실된 경우 파드를
  지우면 살아 있는 세션이 깨진다」. 되돌릴 수 없는 실수의 근거라 남긴다.
- **`checkpointerFor`의 「synthetic checkpoint 뒤에서 파드를 회수하지 말 것 — 프로덕션 CRIU 게이트가
  꺼졌을 때의 데이터 손실 경로였다」** — 온이력 관측 사실이고 코드가 말하지 않는다.
- **`auxiliary_pods_test.go`의 「아직 보조 파드를 만드는 워크로드 타입이 없다 — 스텁의
  `SetAuxiliaryPods`가 그 미래 타입을 대신해 계약을 먼저 고정한다」** — 테스트가 왜 스텁 위에
  서 있는지의 근거. 어느 문서에도 없다.
- **`workload_type_test.go`의 AC-F5 미구현 서술(6줄)** — 「approval-gated는 아카이브 전략이 없어
  동결을 거부해야 하고, shell CRIU로 흘러가면 복원 불가능한 체크포인트 뒤에서 파드 쌍이 회수된다」.
  `checkpointerFor` 주석과 일부 겹치지만 **거부가 언제 사라지는지**(그 타입의 전략 등록 시점)를
  더 말한다. 갈렸으므로 남겼다.
- **`commitThenErrorStore`·`deleteBeforeRestoreCASStore`의 모델 설명** — 그 스텁이 무슨 실패
  모양을 흉내내는지는 타입 이름만으로 복원되지 않는다.

**갈려서 남긴 것 — `manager.go` `Stream` doc의 「열린 브라우저 탭과 SSE heartbeat가 유휴 회수를
무력화해서는 안 된다」.** AC-B1 구체화가 *「passive SSE 연결·output/reset event·comment keepalive는
… 활동이 아니다」* 로 같은 말을 한다. 그러나 그 문서는 **규칙**을 적고 이 주석은 **그 규칙이 없으면
무슨 일이 일어나는지**를 적는다 — `Stream`이 왜 `touch`를 부르지 않는지는 규칙만으로는 한 번 더
추론해야 한다. 비용 비대칭에 따라 남겼다.

**관측 — 열려 있는 PR [#54](https://github.com/dlddu/session-platform/pull/54)는 이 정책의 운영
규칙을 거꾸로 적용한다.** 64파일에서 주석 1,126줄을 지우는 그 PR(2026-09-03, 정책 문서보다 하루
앞선다)은 산문 사본을 남기고 **`(AC-…)` 포인터를 지운다** — 예: `// Read … the nextOffset cursor
(AC-C2, AC-D3/E3). Offset 0 replays the full session history` → `… the nextOffset cursor. Offset 0
replays the full session history`. 이 문서의 「포인터는 남기고 사본은 지운다」와 정확히 반대
방향이라, 머지하면 다음 판정이 기댈 AC 색인이 사라진다. 게다가 base가 낡아 현재 main과
`mergeable:false`(dirty)이고 리뷰가 0건이다. **머지가 아니라 종료(close)를 권한다** — 그 PR이
담은 판단 중 이 정책과 일치하는 부분은 이번 슬라이스가 흡수했고, 나머지 경로는 후속 슬라이스가
정책 기준으로 다시 판정한다.

## 범위 밖

- 라이선스 헤더, 생성 코드, `docs/`의 마크다운·목업 HTML, `k8s/`·`deploy/` 매니페스트,
  `scripts/`·`tools/`·`.github/`의 검사·CI 스크립트, `web/`의 설정 파일.
- **문서·커밋 메시지 자체의 품질** — 다만 「주석을 지우려면 원본을 고쳐야 한다」는 판정이
  그 원본을 고치는 작업을 낳을 수 있고, 그건 정상이다.
- **주석의 정확성**(코드와 맞는가) — 이 정책은 중복만 본다. 다만 되풀이된 주석이 낡아 틀려
  있으면 그것은 제거 근거를 강화한다.
- 줄 끝 주석(`x := 1 // 이유`)과 Go raw string 안의 주석은 현재 판정 지문이 보지 못한다.
  그런 주석이 늘어나면 패턴을 넓히는 것을 후속 작업에서 다룬다.
