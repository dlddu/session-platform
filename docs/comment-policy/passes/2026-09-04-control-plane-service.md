# 2026-09-04 — `control-plane/internal/service/` (판정 299줄)

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
| `reaper.go` `NewIdleReaper`·`Run`·`ScanOnce` (3→0) | 「테스트가 시계를 주입한다」 · 「SIGINT/SIGTERM에 깨끗이 멈춘다」 · 「단일 tick을 위해 export했다」 | ① `reaper_test.go` · ① `cmd/control-plane/main.go`의 `signal.NotifyContext(…SIGINT, SIGTERM)` · ① export 여부는 선언이 말한다 |
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

## 증분 재판정 — AC-F3 유휴 예외가 더한 23줄 (2026-09-04)

유휴 예외 슬라이스가 `manager.go`에 주석 23줄을 더해 이 범위가 R2에 걸렸다. 게이트가 요구한
대로 **더해진 23줄만** 같은 절차로 판정했다 — **제거 13 · 유지 10**(229 → 239).

| 자리 | 판정 | 근거 |
| --- | --- | --- |
| `approvalWaitReporter` doc (3→1) | 2줄 제거 | 「선택적 능력 인터페이스인 이유」는 ③ PR 본문·④ 커밋 메시지에 있다. 게다가 **바로 옆 형제 셋**(`checkpointAborter` · `generationCheckpointer` · `agentCheckpointAborter`)은 주석이 **0줄**이다 — 같은 형태에 다른 기준을 적용하지 않는다. AC 포인터 한 줄만 남겼다 |
| `WithClock` doc (4→2) | 2줄 제거 | 「대기 중에는 카운트가 진행되지 않고, 끝나면 진행된다」는 인용은 `test/approval-gated-workload.md` 시나리오 5의 **기대 결과를 그대로 옮겨 적은 사본**이다. 운영 규칙대로 **경로(포인터)는 남기고 사본을 지웠다.** exported 라 식별자로 시작하는 doc 첫 줄은 유지 |
| `snapshot()` 인라인 (6→2) | 4줄 제거 | 「결정이 나면 갱신이 멈추고 일반 카운트로 돌아간다」는 `prd/approval-gated-workload.md` AC-F3 「유휴 기준」 항목의 사본이다. 반면 **「플랫폼이 클라이언트 접근 없이 `lastAccess`를 전진시키는 유일한 자리」**는 한 지점의 코드로는 보이지 않는 사실(모든 `Touch` 호출자를 훑어야 안다)이라 유지 |
| `holdingForApproval` doc (10→5) | 5줄 제거 | 세 좁힘 중 **둘은 바로 아래 코드가 말한다**(`if sess.WorkloadType != …` 가드 · `if err != nil { return false }`) → 복원 경로 ①. 남긴 하나는 **호출자의 성질**(「유일한 호출자가 리퍼 경로다」)이라 이 자리에서 보이지 않고, 수동 동결이 여전히 어는 이유를 지탱한다 |

**갈린 판정**: `snapshot()`의 「유일한 자리」와 `holdingForApproval`의 「유일한 호출자」는 둘 다
*지금은* 참이지만 **코드가 바뀌면 조용히 거짓이 되는 형태**다(정책이 경계하는 바로 그 종류).
그럼에도 남긴 이유는 둘 다 **한 지점을 읽어서는 확인할 수 없는 전역 사실**이고, 틀렸을 때의
비용이 「없어서 다시 전수 조사하는」 비용보다 작다고 봤기 때문이다. 뒤집고 싶으면 이 표를 고친다.

## 2026-09-12 (2) 증분 재판정 — AC-F5 후반(아카이브) 슬라이스

`workload_type_test.go` 에 초안이 11줄을 더했고, 같은 슬라이스가 **기존 6줄을 거짓으로 만들어**
함께 걷었다. **11줄 판정, 제거 6 · 유지 5**(239 → 238).

걷힌 기존 6줄은 `TestSnapshotIsRefusedForApprovalGated` 의 doc 이다. 첫 문장이 「AC-F5 는 구현되지
않았다」로 시작하는데 이 PR 이 그 전제를 뒤집었다 — 낡아 거짓이 된 주석이므로 같은 hunk 에서
처분하는 것이 정책의 기본 방향이다. 그 doc 이 담고 있던 **왜 fail-closed 인가**는 사라지지 않는다:
`manager.go` 의 `checkpointerFor` 가 같은 내용을 자기 doc 으로 이미 소유한다(①).

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| 새 성공 케이스 doc (4→1) | 「approval-gated 가 claude-code 와 같은 방식으로 언다」 · 「거부를 대체한 단언이다」 · 「아래 자매를 보라」 | ① 첫째는 테스트 본문 · ③④ 둘째는 이 PR 과 커밋 · ① 셋째는 바로 아래 함수 |
| 남은 거부 케이스 doc (5→2) | `checkpointerFor` 의 fail-closed 근거와 데이터 손실 형태 | ① `manager.go` `checkpointerFor` 의 doc 이 정본이다 |

**유지 5줄** — 둘 다 **공허하지 않음의 논거**다. 성공 케이스가 셸 체크포인터를 `NewStubCheckpointer(false)`
로 주입하는 이유(그것이 판별자라, `true` 로 「고치면」 단언이 아무것도 사지 못한다)와, 거부 케이스가
**어떤 배포 형태를 대표하는가**(아카이브 게이트를 끈 배포) — 후자가 없으면 이 케이스는 전략이 등록된
세상에서 왜 살아남았는지 설명되지 않아 다음 슬라이스가 지운다.
