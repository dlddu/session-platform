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

| 날짜 | 범위 | 판정 | 제거 | 유지 |
| --- | --- | --- | ---: | ---: |
| 2026-09-04 | `control-plane/internal/session/` (`session.go`·`manager.go`) | 첫 판정 패스 | 19 | 143 |

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

## 범위 밖

- 라이선스 헤더, 생성 코드, `docs/`의 마크다운·목업 HTML, `k8s/`·`deploy/` 매니페스트,
  `scripts/`·`tools/`·`.github/`의 검사·CI 스크립트, `web/`의 설정 파일.
- **문서·커밋 메시지 자체의 품질** — 다만 「주석을 지우려면 원본을 고쳐야 한다」는 판정이
  그 원본을 고치는 작업을 낳을 수 있고, 그건 정상이다.
- **주석의 정확성**(코드와 맞는가) — 이 정책은 중복만 본다. 다만 되풀이된 주석이 낡아 틀려
  있으면 그것은 제거 근거를 강화한다.
- 줄 끝 주석(`x := 1 // 이유`)과 Go raw string 안의 주석은 현재 판정 지문이 보지 못한다.
  그런 주석이 늘어나면 패턴을 넓히는 것을 후속 작업에서 다룬다.
