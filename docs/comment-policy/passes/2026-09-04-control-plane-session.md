# 2026-09-04 — `control-plane/internal/session/` (판정 162줄)

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
