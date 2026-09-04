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

> **범위는 파일 단위로 적고, 줄 수와 지문을 함께 적는다.** 디렉터리 이름만 적은 행은 「이 경로는
> 판정 완료」로 오독된다 — 판정을 잰 커밋과 머지 커밋 사이에 다른 PR이 그 경로에 주석을 더하면,
> 그 증분은 어느 행에도 속하지 않은 채 영원히 미판정으로 남는다. 자매 레포에서 실제로 그렇게 됐다.
>
> **그 오독을 이제 기계가 막는다.** `scripts/check_comment_policy.py`가 매 PR에서 아래 행들을
> 모델 as-is 지문과 **같은 추출·정규화·정렬로 재측정**해 대조한다(R1 범위 실재 · R2 줄 수·지문
> 일치 · R3 이중 등재 금지 · R4 합계 미러). 등재된 범위에 누가 주석을 더하면 그 자리에서 빨개진다.
> 그래서 이전 판의 「@ 측정 커밋」 표기는 더 필요하지 않다 — **지문이 그 자리를 대신한다.**
>
> 읽는 법: **`주석 줄`·`지문`은 그 범위의 *현재* 상태**이고(게이트가 대조하는 값), **판정한 줄 수와
> 제거·유지는 `결과` 칸**에 있다. 둘은 다르다 — 판정은 지운 줄만큼 범위를 줄이기 때문이다.
> 범위를 더하거나 뺄 때는 아래 합계 마커도 **같은 PR에서** 함께 움직여야 한다(R4).

<!-- 판정-원장 -->

| 판정일 | 범위 | 주석 줄 | 지문 | 결과 |
| --- | --- | ---: | --- | --- |
| 2026-09-04 | `control-plane/internal/session/session.go` · `control-plane/internal/session/manager.go` | 143 | `b43d405ac066` | 첫 판정 패스 — **162줄 판정, 제거 19 · 유지 143**. 상세 ↓ |
| 2026-09-04 | `control-plane/internal/service/manager.go` · `control-plane/internal/service/manager_test.go` · `control-plane/internal/service/reaper.go` · `control-plane/internal/service/reaper_test.go` · `control-plane/internal/service/auxiliary_pods_test.go` · `control-plane/internal/service/workload_type_test.go` | 229 | `f708d58cdb19` | 2차 판정 패스 — **299줄 판정, 제거 75 · 유지 224**(지문 기준 304 → 229, 오검출 5줄 포함). 상세 ↓ |
| 2026-09-04 | `control-plane/internal/adapter/k8s/client_orchestrator.go` · `control-plane/internal/adapter/k8s/orchestrator.go` · `control-plane/internal/adapter/k8s/network_policy.go` · `control-plane/internal/adapter/k8s/orchestrator_test.go` | 328 | `b09a85723f8c` | 3차 판정 패스 — **473줄 판정, 제거 153 · 유지 320**. 이후 #66이 더한 14줄을 재판정(제거 6 · 유지 8) — 누적 **487줄 판정, 제거 159 · 유지 328**. 상세 ↓ |
<!-- /판정-원장 -->

판정 완료 합계 **<!-- 판정-합계 -->700<!-- /판정-합계 -->줄**(등재 범위의 현재 줄 수 합).
전체 대비 비율과 미판정 잔량은 **게이트가 출력한다** — 프로즈에 적으면 낡는다.

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

> ✅ **처리 (3차 패스)**: #54는 위 권고대로 **종료한다** — 3차 패스가 그 종료를 첫 단계로
> 삼고, 이 문단이 그 판단의 기록이다. 근거는 셋이다: ① 그 PR은 정책 문서(#63)보다 먼저
> 만들어져 5단계 판정 절차를 한 번도 거치지 않았고, ② 64파일이 **모든 후속 슬라이스 후보와
> 교집합**이라 열어 둔 채로는 어떤 범위도 안전하게 집을 수 없으며, ③ 2026-09-03 이후 무변경인
> 채로 main이 여러 번 전진해 `mergeable:false`다. 그 PR이 만졌던 경로는 이 원장이 하나씩
> 정책 기준으로 다시 판정한다 — `internal/adapter/k8s/`가 3차 패스로 그 첫 사례다.

### 2026-09-04 — `control-plane/internal/adapter/k8s/` (판정 473줄)

레포 최대 밀집 구간. 4파일 전부를 통째로 읽었다 — `client_orchestrator.go`(327) ·
`orchestrator.go`(86) · `network_policy.go`(53) · `orchestrator_test.go`(7).

**왜 이 범위인가**: 이 패키지의 주석은 AC-A2·A3·E1·E6·F2·F4·F6이 세 번의 PR(#61 계열 ·
#64 · #62)에 걸쳐 착지하면서 **PRD 문장이 그때마다 코드로 한 번씩 더 복사된** 자리다. 그래서
제거율이 앞선 두 패스(12% · 25%)보다 높은 **32%**다 — 판정이 엄해진 것이 아니라 같은 문장이
여러 벌 있었다. 포트(`orchestrator.go`)와 구현(`client_orchestrator.go`)이 **한 패키지 안에서
서로를 되풀이**하므로 파일 하나만 집으면 「지운 쪽의 사본」을 소유하지 못한다 — 패키지 경계로
잡은 이유다.

**제거 153줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `client_orchestrator.go` 파일 헤더 (7→1) | 「세션당 전용 파드를 몰고(AC-A1/A2), stop에서 회수하고(AC-A3), 워크로드 에이전트에 닿음을 증명(AC-D1/E1)」 · 「워크로드는 data plane 이미지의 entrypoint가 시작한다」 · 「main은 BuildClient로 client/namespace를 만든다」 | ① 같은 파일 `ClientOrchestrator` 타입 doc이 첫 절을 그대로 다시 말한다 · ① `buildPod`의 「No command override」가 둘째를 · ① `NewClientOrchestrator` doc이 셋째를 · ① `orchestrator.go`의 패키지 주석이 「포트·구현·스텁이 어디 있는지」를 이미 말한다(포인터만 남겼다) |
| `client_orchestrator.go` 자격 증명 분리 4곳 (25→8) | Claude 런타임 const 블록(7), approval-gated const 블록(6), `buildPod`의 approval-gated case(8→3), `claudeCredentialProxy`(4→1)·`credentialProxyContainer`(6→3) — **같은 성질을 네 번** | ② `prd/claude-code-workload.md` AC-E6(*「주 컨테이너에는 실제 공급자 자격 증명을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다」*)과 `prd/approval-gated-workload.md` AC-F6의 두 불릿(*「게이트웨이 URL·API key와 …userId → 헬퍼 파드의 MCP 컨테이너에만」*, *「공급자 base-url·auth-token → …credential-proxy 컨테이너에만」*) · ① 바로 아래 `secretEnv(...)` 목록이 어느 키가 어느 컨테이너로 가는지 그 자체로 보여 준다 |
| `WithCheckpointPrivileged`(8→4) · `buildPod` privileged 인라인(5→2) | 2026-07-23 검증의 세부(CHECKPOINT_RESTORE+SYS_PTRACE의 netns EPERM, containerd AppArmor의 mount 차단, read-only `ns_last_pid`, privileged에서 `criu check` 완전 통과, 최소화 후속) | ② `docs/criu-verification.md` 「2차 (2026-07-23 …)」가 그 다섯을 그대로 적는다. **인라인 쪽은 이미 「see WithCheckpointPrivileged」라고 자백하면서 내용을 또 옮겼다** — 포인터만 남겼다 |
| `AnnotationRestoreCheckpoint` (8→4) | CRI-O `io.kubernetes.cri-o.restore` 어노테이션·체크포인트 OCI 이미지 매핑 서술 · 「export한 이유는 그 런타임 매핑이 복원 계약의 일부라서」 | ② `docs/criu-verification.md`의 「CRI-O 대안(미배선)」 항목들 — **주석이 스스로 그 문서를 인용하고 있었다.** 포인터와 「미배선」 사실만 남겼다 · ① export 여부는 선언이 말한다 |
| `orchestrator.go` 포트 메서드 doc 3곳 (11→3) | `Start`/`Stop`/`RestoreInto` doc의 AC 해설과 「variadic이라 whole set을 넘긴다」·「unique name이라 terminating pod와 충돌하지 않는다」 | ① 같은 doc 블록 바로 위의 `AC mapping:` 색인(유지) · ① 시그니처가 가변인자임을 말한다 · ① `restorePodName` doc이 이름 충돌 회피를 정본으로 적는다 |
| `client_orchestrator.go` `Start`(4→1) · `RestoreInto`(6→3) · `Stop`(5→4) | 포트 doc이 이미 말한 계약의 재진술 | ① `orchestrator.go`의 `PodOrchestrator` 인터페이스 doc — **포트와 구현이 같은 패키지에 있어 한 번만 말하면 된다** |
| `var _ PodOrchestrator = …` (1→0) | `compile-time assertion that ClientOrchestrator satisfies the port.` | ① 그 선언 자체가 컴파일타임 단언이다. **직전 패스가 `service.Service`에서 지운 것과 같은 형태** |
| `workloadImages` 필드 주석 (3→0) · `WithWorkloadImage`(6→2) · `WithImage`(2→1) | 「기본 타입은 `image`로 폴백, 미설정 타입은 거부」의 **3중 서술** · 「alpine 폴백은 readiness probe를 통과하지 못한다」 사본 | ① `imageFor` doc이 그 규칙의 정본이다(유지) · ① `defaultDataPlaneImage` doc이 alpine 함정의 정본이다 — `WithImage`는 이미 「see defaultDataPlaneImage」라 적고 있었다 |
| `SessionMCPContainerName` (3→2) | 「이 슬라이스는 컨테이너만 프로비저닝하고 **게이트 자체는 아직 구현되지 않았다**」 | ② `docs/doc-tracker.md`(2026-09-04) — *「승인 게이트가 착지했다 … `tools/call`이 승인 게이트웨이에 요청을 만들어 … APPROVED일 때만 실제 외부 GET을 한다」*. **#64가 머지되며 거짓이 된 주석**이다 |
| `ContainerName` (3→2) | 「shell 파드는 이것뿐, Claude 파드는 격리된 credential-proxy 사이드카를 더한다」 | ① 같은 파일 `buildPod`의 `switch workloadType` 세 case · 게다가 approval-gated가 생기며 **불완전해졌다**(그 타입은 사이드카 대신 헬퍼 파드다) |
| K3s MCP·마켓플레이스 URL const 블록 (7→3) | 「플러그인 부트스트랩이 닿는 두 엔드포인트」 서술과 「조직 엔드포인트에 못 닿는 환경(kind e2e SUT)이 같은 코드 경로를 인클러스터 대역으로 돌릴 수 있다」 | ② `prd/claude-code-workload.md` AC-E6이 부트스트랩 왕복을, `docs/test/e2e.md`의 `PLUGIN-CRED` 행이 **그 대역 치환을 통째로** 적는다 · ① `optionalSecretEnv` 호출이 optional임을 말한다 |
| `agentAttachPath`(3→1) · `Reach`(4→3) · 「Opening the stream is the proof」(1→0) · readiness probe 인라인(2→0) | attach가 무엇인지·healthz가 왜 readiness인지의 재진술 | ① `Reach` doc과 `agentHealthzPath` doc이 각각 정본이다 |
| `helperPodSpec`(10→6) · `SessionIDEnvVar`(6→3) · `ApprovalGateway*` 트리오(3→1) · `SessionMCPURLEnvVar`(3→2) · `HelperCredentialProxyContainerName`(3→2) | 「보조 파드라 AC-A2를 깨지 않는다」·「상태를 갖지 않아 동결 시 버려진다」·「`{세션ID}:{요청ID}`이고 게이트웨이가 중복을 거부한다」·「자격 증명이 아니라 파드가 이미 라벨로 다는 값」·「MCP가 파드 밖 유일한 도구 표면」 | ② AC-F4·AC-F3·AC-F6 본문이 전부 축자에 가깝다 · ② `doc-tracker.md`가 `SESSION_ID`를 *「자격 증명이 아니라 파드가 이미 라벨로 다는 값」* 으로 그대로 적는다 |
| `network_policy.go` 파일 헤더 (20→12) | 「PRD는 이 성질을 워크로드 파드가 무엇에 닿을 수 있는지로 적는다: kube-dns와 자기 세션 헬퍼 파드, 그 밖은 전부 차단」 · 「집행 여부는 CNI의 몫이고 kindnet은 NetworkPolicy를 구현하지 않는다」 | ② AC-F2 본문이 허용·차단 목록을 그대로 적는다 · ② `doc-tracker.md`의 열린 항목이 집행 미검증을 적고, **주석이 이미 그 문서를 가리키고 있었다** — 「오브젝트만 세운다」는 사실과 포인터만 남겼다 |
| `orchestrator.go` `SessionPods`(7→3) · `SetAuxiliaryPods`(8→6) · `RunningCount`(5→3) · `SessionsCount`(3→1) · `StubOrchestrator.Start` 인라인(3→0) · 부분 회수 인라인(2→0) | 「shell·claude-code는 보조 파드가 없고 approval-gated는 정확히 하나」 · 「보조 파드가 함께 기동·회수·복원된다」 · 「RunningCount가 예전에 세션 수를 셌다」 | ② AC-A2의 보조 파드 절이 타입별 개수를 적는다 · ② `doc-tracker.md`의 해소 항목이 스텁 노브가 단정하는 네 가지를 그대로 열거한다 · ④ RunningCount의 과거 의미는 커밋 이력이 갖는다 |
| `orchestrator_test.go` 테스트 doc 2곳 (7→3) | 「이름이 서로 달라 각각 회수할 수 있고 All이 워크로드 파드를 앞에 둔다」 · 「워크로드 파드만 멈추면 완전 회수로 보이면 안 된다」 | ① 바로 아래 본문의 단언과 실패 메시지 — 특히 *「(the auxiliary pod leaked)」* 가 같은 말을 한다 |
| 그 밖 doc 축약 10여 곳 | `WithAgentPort`·`WithReadiness`의 「테스트가 주입한다」, `NewClientOrchestrator`의 「fake clientset」, `BuildClient`의 in-cluster/kubeconfig 분기 서술, `provision`·`startSet`·`buildPod`의 checkpointRef 분기 재진술, `hardenedSecurityContext`·`helperPodName`·`helperRestorePodName`의 본문 재진술, `claudeCredentialsSecret` 관련 옵션 doc | ① 전부 같은 파일의 본문·시그니처·다른 doc이 말한다 |

**유지 320줄** — 「지울까」를 검토했다가 남긴 것들.

- **`PodOrchestrator`의 `AC mapping:` 색인(5줄)** — 직전 패스가 `session.Manager`의 같은 블록을
  남긴 것과 **같은 이유**: 개별 대응은 ②로 복원되지만 **Go 메서드에서 AC로 가는 방향의 색인은
  어느 문서에도 한 덩어리로 존재하지 않는다.** 다만 각 항목에 붙어 있던 괄호 해설은 AC 재진술이라
  지우고 **색인만** 남겼다 — 그것이 이 블록이 유일하게 갖는 값이다.
- **`restorePodName`의 이름 충돌 레이스(8줄)** — 「동결이 지운 파드가 아직 Terminating일 때
  같은 이름을 재사용하면 create가 AlreadyExists로 진다」. 되돌리기 어려운 실수의 근거이고
  어느 문서에도 없다. `restoreSuffix`의 63자 DNS 라벨 산술(2줄)도 같은 성격이다.
- **`pullPolicyForImage`(7줄 + 파싱 인라인 3줄)** — 「같은 태그의 낡은 캐시를 노드가 내주어
  새 control plane이 그 라우트가 없던 옛 에이전트에 `/read`를 걸었다」는 **관측된 실패**와,
  레지스트리 포트 콜론을 태그 구분자로 오인하지 않기 위한 파싱 근거.
- **헬퍼 파드 MCP의 exec probe 이유(6줄)** — 「HTTP probe는 파드가 아니라 kubelet에서 출발하는데
  AC-F2의 ingress 정책은 그 세션 워크로드 파드만 허용한다. 정책을 집행하는 CNI 아래에서는
  probe가 경계가 거절하도록 설계된 바로 그 호출자가 되어 헬퍼 파드가 영영 Ready에 못 간다.」
  AC-F2는 이 상호작용을 말하지 않는다.
- **`network_policy.go`의 NetworkPolicy 의미론(약 15줄)** — 「peer에 namespace selector가 없으면
  이 네임스페이스 밖 파드는 매치될 수 없고, 남의 세션 헬퍼를 배제하는 것은 selector의 세션 id다」,
  「정책은 additive라 복원 중 두 라운드의 쌍이 무해하게 겹친다」, `ownerReferenceTo`의
  「`BlockOwnerDeletion`을 일부러 끄는 이유는 pods/finalizers 업데이트 권한이 정책 회수 값어치보다
  넓은 부여이기 때문」. 전부 코드 형태로는 보이지 않는다.
- **cross-module 「Keep in sync with data-plane/cmd/agent」 4건** — `AgentPort`·`SessionIDEnvVar`·
  `restoreModeEnvVar`·`CredentialProxyPlacementEnvVar`. 컴파일러가 잡지 못하는 계약이라 포인터가
  유일한 방어다. `CredentialProxyPlacementEnvVar`의 「바인드 주소에서 추론하지 않고 선언한다 —
  선언이 없으면 제한적인 쪽」도 data plane 쪽 동작이라 남겼다.
- **`defaultDataPlaneImage`(6줄)** — alpine 폴백이 readiness probe를 통과할 수 없다는 함정.
  `claudeCodeStateDir`의 「활성 마운트 지점 rename은 EBUSY」(3줄), `BuildClient`의 「deferred
  kubeconfig 로더는 in-cluster 네임스페이스를 읽지 않는다」(3줄)도 같은 성격의 외부 시스템 지식이다.

**갈려서 남긴 것 셋.**

1. **`LabelPodRole`의 selector 함정(4줄)** — 「모든 파드가 세션 id를 달고 있어, `LabelSessionID`만
   보는 selector는 헬퍼 파드를 그 세션의 두 번째 워크로드 파드로 센다」. AC-A2의 1:1 성질로
   복원된다고 볼 수도 있으나, **성질과 그 성질을 selector로 옮길 때의 함정은 다르다.** 남겼다.
2. **`helperPodSpec`의 「Kubernetes 신원을 주지 않는다」(2줄)** — `automount=false`는 ①로 보이지만,
   *왜*(안에 API server가 필요한 것이 없고, 이 파드가 플랫폼의 외부 비밀 **둘**을 쥐고 있다)는
   어느 문서에도 없다. AC-F6은 비밀 배치만 말하고 신원 부재는 말하지 않는다.
3. **`startSet`의 보조 파드 선행 기동(5줄)** — AC-F4는 「복원된 워크로드 파드에 그 복원의 헬퍼 파드
   주소가 주입된다」를 말하지만, **그래서 기동 순서가 강제된다**는 것은 말하지 않는다. 순서 계약은
   유지 대상이다.

**지문 사각지대 — 이 범위에서 실측했다.** 줄 끝 주석 **7건**(`ClientOrchestrator`의 필드 4개,
`StubOrchestrator`의 맵 3개)이 판정 지문에 보이지 않는다. 반대로 직전 패스가 `internal/service/`에서
5줄 보고한 **오검출**(포인터 역참조·임베드 필드)은 이 범위에 **0건**이다. 둘 다 정의가 유예한
후속 항목이며, 지금은 **게이트가 지문을 강제하므로 사각지대를 넓히는 변경은 지문을 바꾸지 못한다** —
패턴을 넓히려면 게이트·모델 정의·이 문서를 한 번에 고쳐야 한다(그 세 곳이 글자 그대로 같아야 한다는
요구를 스크립트 상단에 못박아 두었다).

#### 재판정 — #66이 더한 14줄 (게이트의 첫 양성)

이 패스를 실은 PR(#69)이 머지되기 5분 전에 형제 PR #66이 같은 범위에 주석 **14줄**을 더했다.
게이트는 머지 직후 main에서 즉시 R2로 그 사실을 보고했다(등재 320 != 실측 334). **이것이 이
게이트를 세운 이유 그 자체다** — 게이트가 없었다면 그 14줄은 「판정 완료」로 표시된 범위 안에
조용히 들어앉아, 어느 행에도 속하지 않은 채 영원히 미판정으로 남았을 증분이다. 아래는 그
증분에 대한 재판정이고, 이 행의 줄 수·지문 갱신이 곧 그 기록이다.

**제거 6줄**

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `client_orchestrator.go` `AnthropicCACertEnvVar` doc (9→8) | 「Secret 키가 optional이라 생략하면 프록시는 시스템 풀에 남고 `k8s/`는 그대로다」 | ① 같은 파일 `optionalSecretEnv`(선언 자체가 `Optional=true`를 세운다) · ① **바로 아래 이웃 블록**이 optional 키 일반에 대해 *「absent, the entrypoint keeps its built-in defaults, so k8s/ needs no change」* 로 이미 말한다 |
| `credentialProxyContainer` 인라인 (5→0) | 「private gateway일 때만 존재한다」 · 「자격 증명과 같은 컨테이너에 실려 두 배치가 이 한 함수에서 같이 받는다」 · 「tool-running 컨테이너는 loopback 주소 말고는 공급자에 대해 아무것도 모른다(AC-E6/AC-F6)」 | ① 바로 위 `AnthropicCACertEnvVar` doc(private gateway 근거, 유지했다) · ① **그 함수 자신의 doc** *「builds the provider proxy in either of its two placements. One behaviour contract (AC-E6) for both; only the placement and bind address differ (AC-F6)」* · ② AC-E6 *「주 컨테이너에는 실제 공급자 자격 증명을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다」* + ① `claudeCredentialProxy` doc |

**유지 8줄** — `AnthropicCACertEnvVar` doc의 나머지.

- **존재 이유**(「공개 저장소가 모르는 CA가 발급한 private gateway를 시스템 루트에 *더해* 신뢰한다」)는
  상수 이름이 말하지 않고 어느 AC에도 없다. AC-E6은 `base-url`·`auth-token`·`model`만 다루고
  `ca-cert`를 언급하지 않는다.
- **분류 근거**(「주소류 설정이지 자격 증명이 아니다 — CA 인증서는 구성상 공개된 값이다」)는
  이 파일의 credential/address 이분법이 왜 이쪽으로 갈렸는지를 말한다. 어느 문서에도 없어 남겼다.
  **「Like the two bootstrap URLs below」의 상호참조는 rebase 후 다시 확인했다** — 이 패스가 그
  이웃 블록을 7줄 → 3줄로 줄였지만 블록과 「optional 키」 성질은 남아 있어 참조가 성립한다.
- **포인터**(`docs/test/e2e.md`의 `CLAUDE-PROVIDER`)는 SSOT로 가는 길이라 남긴다.
- **「Keep in sync with data-plane/cmd/agent (providerCACertEnv)」** — 컴파일러가 잡지 못하는
  cross-module 계약. 이 패스가 같은 형태 4건을 남긴 것과 같은 이유다.

## 범위 밖

- 라이선스 헤더, 생성 코드, `docs/`의 마크다운·목업 HTML, `k8s/`·`deploy/` 매니페스트,
  `scripts/`·`tools/`·`.github/`의 검사·CI 스크립트, `web/`의 설정 파일.
- **문서·커밋 메시지 자체의 품질** — 다만 「주석을 지우려면 원본을 고쳐야 한다」는 판정이
  그 원본을 고치는 작업을 낳을 수 있고, 그건 정상이다.
- **주석의 정확성**(코드와 맞는가) — 이 정책은 중복만 본다. 다만 되풀이된 주석이 낡아 틀려
  있으면 그것은 제거 근거를 강화한다.
- 줄 끝 주석(`x := 1 // 이유`)과 Go raw string 안의 주석은 현재 판정 지문이 보지 못한다.
  그런 주석이 늘어나면 패턴을 넓히는 것을 후속 작업에서 다룬다.
