# 2026-09-05 — 컨트롤 플레인의 나머지 전부 (판정 142줄)

`internal/api` · `internal/store` · `internal/static` · `internal/service`의 등재 밖 1파일 ·
두 모듈의 `go.mod`. **제거 57 · 유지 85.** 이 패스로 컨트롤 플레인에서 `control-plane/test/`
밖의 판정 대상이 남지 않는다.

**주석 0줄 파일도 등재했다**(`cmd/control-plane/main_test.go` · `internal/api/errors_test.go`).
R2는 *등재된 파일 목록만* 재측정하므로, 등재하지 않으면 그 파일에 나중에 생기는 주석이 rc=0
아래에서 영원히 미판정으로 남는다 — 4차 패스가 정확히 그 형태로 68줄을 놓쳤고 5차 패스가
편입해 닫았다. 같은 함정을 이번에는 등재로 미리 막는다.

**낡아 거짓이 된 주석 2건을 바로 아래 코드로 반증했다.**

| 위치 | 주장 | 반증 |
| --- | --- | --- |
| `control-plane/go.mod` 상단 | 「Both reuse the k8s.io dependencies below — **no extra runtime deps**」 | **바로 아래 require 블록**에 직접 의존이 `aws-sdk-go-v2` · `…/config` · `…/credentials` · `…/service/s3` · `…/service/sts` · `gorilla/websocket` 여섯이 더 있다 |
| 같은 곳 | 「Only the Checkpointer (CRIU) remains an **in-memory stub**, so its external deps are **not required yet**」 | `internal/adapter/criu/{agent_checkpointer,container_checkpointer}.go`와 `internal/adapter/checkpointstore/store.go`(S3)가 실재하고, 그 외부 의존이 위의 aws-sdk 다섯이다. 5차 패스가 이미 그 파일들을 판정해 등재했다 |

두 문장이 같은 주석 블록에 있고 **반증이 같은 파일 다음 다섯 줄에 있다.** 「주석은 아무도
검증하지 않는다」의 가장 값싼 예라 블록 전체를 지웠다.

**제거 57줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `control-plane/go.mod` 상단 (5→0) | 위 표의 거짓 2건 + 「PodOrchestrator와 StateStore는 둘 다 client-go 위에 서고 ConfigMap·Lease에 상태를 둔다」 | **거짓이다**(위) · ① `internal/store/store.go` 패키지 doc이 *「the concrete adapter lives under internal/adapter」*로, 5차 패스가 등재한 `adapter/configmap/store.go`가 그 구현으로 말한다 |
| `data-plane/go.mod` 상단 (3→0) | 「creack/pty가 셸을 의사 터미널에 붙이고(AC-D1) gorilla/websocket이 `/attach`를 낸다 · 둘 다 의존이 없다」 | ② `prd/*` AC-D1 · ① `data-plane/cmd/agent/main.go`의 `pty.StartWithSize`와 `mux.HandleFunc("GET /attach", …)` · ① 이 `go.mod` 자신 — indirect 블록이 없다는 것이 「의존이 없다」이다 |
| `store.go` 패키지 doc (6→3) | 「ConfigMap+Lease 구현이 모든 연산을 k8s API로 받쳐 전이·점유가 복제본 간 atomic하다(AC-C1)」 · 「도메인 에러는 session 패키지에 있다」 | ① 바로 아래 `StateStore` doc이 *「Every state transition and occupancy claim must be atomic (AC-C1)」*로 같은 말을 한다(같은 파일 두 벌) · ② `prd/state-api.md` AC-C1 · ① 메서드 doc들이 `session.ErrConflict`를 그대로 적는다. **포인터(`internal/adapter`)는 남기고 사본을 지웠다** |
| `store.go` `CompareAndSwapSession` doc (3→0) | 「lifecycle state와 durable snapshot transaction이 **둘 다** 기대값과 맞을 때만 aggregate를 통째 교체한다」 | ① 시그니처의 `expectedState`·`expectedTxn`·`next` 파라미터 이름이 그 계약 자체다. 그리고 같은 인터페이스의 `Put`·`Get`·`List`·`Unlock`은 doc이 **0줄**이라 이 파일은 「모든 메서드에 doc」 규약을 갖지 않는다(1차 패스가 `CreateRequest is the input to Manager.Create.`를 지운 것과 같은 형태) |
| `static.go` 패키지 doc (7→6) | 「그 디렉터리는 컨트롤 플레인 바이너리에 임베드된다」 | ① 여덟 줄 아래 `//go:embed all:dist` + `var embedded embed.FS` |
| `static.go` `Handler` doc (3→1) | 「정적 자산은 그대로, 비-API·비-자산 경로는 index.html로 폴백해 클라이언트 라우팅이 된다」 | ① 본문 20줄이 그대로 그것이고, **같은 함수 안 인라인 2건**이 다시 말한다 — 한 파일에 세 벌 |
| `static.go` 폴백 인라인 (2→1) | 「요청한 파일이 있으면 그것을 낸다」 | ① 바로 아래 `if _, err := fs.Stat(sub, p); err != nil` 분기. 「history-mode routing」이라는 이름만 남겼다 |
| `api.go` 패키지 doc (3→2) | 「핸들러는 얇다: decode → manager 위임 → encode」 · 「도메인 에러는 여기서 HTTP 상태로 매핑된다」 | ① 모든 핸들러 본문이 정확히 그 세 줄이다 · ① 같은 파일 `writeErr`의 `switch`가 그 매핑표다 |
| `api.go` `API` doc (1→0) · `Option` doc (1→0) · `New` doc (1→0) | `API holds the dependencies the handlers need.` · `Option customises the API surface.` · `New returns an API bound to a session.Manager.` | ① 각각 필드 선언(`mgr session.Manager` …) · `type Option func(*API)` · 시그니처 `New(mgr session.Manager, opts ...Option) *API`. 정책의 「그 1줄이 시그니처를 그대로 옮겨 적기만 하면」 조항 그대로다 |
| `api.go` `WithClaudeCodeModelConfig` doc (3→2) | 「카탈로그는 표시 설정이지 API 허용목록이 아니다」 | ① `cmd/control-plane/main.go`의 `claudeCodeModels` 필드 doc — **6차 패스가 바로 그 구분을 유지 판정해 정본으로 남겼다** · ① 같은 패키지 `config_test.go`의 `catalog-soft-choice` 케이스 |
| `api.go` snapshot 라우트 인라인 (2→0) | 「리퍼의 수동 대응물: 즉시 아카이브하고 파드를 회수한다. 나중의 switch가 복원한다」 | ① 같은 파일 `snapshotSession` doc(*「freezes a session and reclaims its pod (AC-B1/AC-A3)」*) · ① `service/manager.go` `Service.Snapshot` doc(*「Explicit snapshots have no idle precondition」*) — 2차 패스가 `reaper.go`의 같은 대비 서술을 그 근거로 이미 지웠다 |
| `api.go` `createReq.Model` doc (2→1) | 「claude-code에서만 받고, 생략하면 platform-default 별칭으로 해석되며 워크로드 타입과 함께 불변」 | ② AC-E6 · ① `session.PlatformDefaultModel` doc와 `NormalizeWorkloadType` doc(1차 패스가 그쪽을 정본으로 남겼다). 포인터(AC-E6)만 남겼다 |
| `api.go` `readReq.Offset` doc (2→1) | 「직전 read가 발급한 nextOffset 커서이고 0(또는 본문 없음)은 세션 시작부터 전체」 | ② AC-D3의 커서 규약 — 2차 패스가 `service/manager.go` `Read`의 **같은 사본**을 그 근거로 이미 지웠다 |
| `api.go` `runtimeConfig` 인라인 (3→2) | 「이 카탈로그는 Secret 기반 환경변수로 기동 시 들어온다」 | ① `cmd/control-plane/main.go`의 설정 필드 doc와 `env(...)` 읽기. `no-store`의 **이유**(롤아웃이 즉시 보이게)는 코드가 말하지 않아 남겼다 |
| `api.go` read·write 본문 인라인 (2→0) | 「본문은 선택이다」 두 벌 | ① 두 자리 모두 `decodeRequestBody(r.Body, &req, false)` — 그 `false`가 `required`다 · ② 빈 write의 의미는 AC-C3(2차 패스가 `manager.go` `Write`의 같은 사본을 그 근거로 지웠다) |
| `api_test.go` 테스트 doc 6곳 (12→2) | 이름과 본문 단언을 옮겨 적은 서술(«204를 내고 파드를 회수하고 이후 read에서 사라진다», «모르는 세션의 snapshot은 404», «경합하는 lifecycle 보유자는 409») | ① 각 테스트 **함수 이름**과 바로 아래 `t.Fatalf` 기대값. 2차 패스가 `manager_test.go`에서 같은 형태 15줄을 지운 것과 같다 |
| `api_test.go` 절 라벨 (2→0) | `// create` · `// list` | ① 바로 아래 `http.Post(.../sessions)` · `http.Get(.../sessions)` — 아무 주장도 담지 않는다 |
| `api_test.go` `// A negative offset is invalid input.` (1→0) | | ① 바로 아래 `offset: -1` 요청과 400 단언 |
| `workload_type_test.go` 인라인·doc 3곳 (5→2) | AC-F1 모델 계약 서술의 **두 번째 벌**(같은 파일 위에 이미 있다) · 「기존 세션 라우트는 불변 필드 변경 시도를 거부한다」 · 「스냅샷/복원 왕복에서 모델이 유지되고 재프로비저닝에도 같은 값이 간다」 | ① 같은 파일 앞선 `// AC-F1: the model contract is AC-E6's, unchanged.` · ① 아래 `for _, path := range []string{"/read","/write","/switch"}` 루프와 단언 · ① 테스트 이름 + 본문 |
| `approval_idle_test.go` 파일 헤더 (4→2) | 시나리오 5의 기대 결과 3줄 사본 | ② **주석이 스스로 인용한** `docs/test/approval-gated-workload.md` 시나리오 5 · ① 아래 세 테스트 doc이 그 셋을 각각 다시 말한다. 포인터만 남겼다 |
| `approval_idle_test.go` `approvalReportingAgent` doc (3→2) | 「widened `agent.Client`가 아니라 별도 타입인 이유」 | ③ PR 본문 · ④ 커밋 메시지 — 5차 패스가 `approvalWaitReporter` doc의 **같은 형태**를 같은 근거로 지웠다 |
| `approval_idle_test.go` `testClock` doc (1→0) | 「시나리오가 요구하는 "시간을 제어할 수 있는 테스트 하네스"」 | ② 시나리오 5 · ① 같은 파일 형제 헬퍼(`mustCreate` · `scan` · `mustGet`)는 doc이 **0줄**이다 |
| `approval_idle_test.go` 테스트 doc·인라인 6곳 (13→7) | AC-F3 요구사항 4줄 사본 · 「한 시간 뒤에도 여전히 대기 중」 · 「사람이 결정한다」의 배경 · 「대조군」 서술의 본문 재진술 · 「읽을 수 없으면 AC-F3 이전으로 돌아간다」 | ② `prd/approval-gated-workload.md` AC-F3 「유휴 기준」 항목이 축자에 가깝다 · ① 각 `t.Fatalf` 메시지가 AC 번호까지 들어 그 단언을 적는다. **AC 포인터와 「왜 이 스캔이 두 번인가」는 남겼다** |

**유지 85줄** — 「지울까」를 검토했다가 남긴 것들.

- **`store.go`의 `AC mapping:` 블록(5줄)** — 1차 패스가 `session.Manager`의 같은 형태를 남긴 것과
  같은 근거다. 개별 대응은 ②로 복원되지만 **포트 메서드에서 AC로 가는 방향의 색인**은 어느 문서에도
  한 덩어리로 없다. **같은 형태에 다른 기준을 적용하지 않는다.**
- **`store.go`의 펜싱·순서 계약(`Delete`·`Touch`·`Lock`·`Renew`, 9줄)** — 「token이 lifecycle 펜스를
  쥔 동안에만」, 「호출자가 락을 계속 쥐고 `Unlock`으로 따로 놓는다」, 「낡은 read의 lifecycle·recovery
  메타데이터를 덮어쓰지 않는다」, 「긴 아카이브 전송이 다른 복제본의 회수를 막는 데 쓴다」. 전부 여러
  메서드에 걸친 시간 순서라 한 선언으로는 보이지 않는다.
- **`api.go`의 절 배너 3개**(`// ---- request/response DTOs ----` 등) — 5차 패스가
  `adapter/configmap/store.go`의 `// ---- helpers ----`를 **유지 판정했다.** 같은 형태에 다른 기준을
  적용하지 않는다.
- **`api.go` `createReq.WorkloadType`의 「raw로 두어 생략과 명시적 empty/null을 가른다」** — `json.RawMessage`
  타입은 「raw」만 말하고 **왜**는 말하지 않는다. 이 구분이 AC-E1 거부 규칙의 전제다.
- **`api.go` `streamSession` doc(3줄)** — `Last-Event-ID`가 쿼리 커서를 이긴다는 우선순위와 **그 이유**
  (네이티브 `EventSource` 재연결이 브라우저가 마지막으로 받은 이벤트부터 재개해야 한다). 외부 시스템의
  동작이라 코드가 말하지 않는다.
- **`api.go` `Routes`의 「Go 1.22+ method+path 패턴이라 라우팅에 의존이 없다」** — 「없는 것」의 근거라
  코드에 나타나지 않는다. 라우터를 넣으려는 다음 사람이 읽어야 할 문장이다.
- **`approval_idle_test.go` `newApprovalIdleService` doc(3줄)** — 「모든 워크로드 타입에 checkpointer를
  주었으므로, active로 남는 타입은 **AC-F3 때문**이지 동결 전략이 없어서가 아니다」. 이 한 줄이 없으면
  대조군 테스트가 vacuous한지 판별할 수 없다. 이번 패스에서 가장 값이 큰 주석이다.
- **`api_test.go` `// switch (active -> active no-op)`** — 같은 자리의 `// create`·`// list`는 지웠는데
  이것만 남겼다. 앞 둘은 아래 호출을 이름으로 되풀이할 뿐이지만, 이것은 **본문이 단언하지 않는 사실**
  (전이가 active→active라 no-op)을 말한다. 본문은 200만 확인한다.
- **`config_test.go`의 「카탈로그는 UI 피커이지 깨지는 API 허용목록이 아니다」(1줄)** — `api.go`에서
  같은 문장을 지우고 이것을 남긴 것은 **갈린 판정**이다. 지운 쪽은 필드 doc이고 이쪽은 「목록 밖 모델이
  201을 받는다」는 놀라운 기대값의 자리다. 정본은 `main.go`의 `claudeCodeModels` doc이므로 세 벌 중
  둘로 줄었다. 다음 패스가 이것마저 지우려면 `t.Fatalf` 메시지에 이유를 옮기는 편이 낫다.

**갈린 판정**: 위의 `// switch (…)`와 `config_test.go` 한 줄, 그리고 `api.go`의 절 배너 3개. 셋 다
「지울 수 있다」와 「이 자리에서만 보인다」가 팽팽했고, 정책의 **「애매하면 남긴다」**로 남겼다.

**이 패스가 원장 자신의 좌표 부패를 함께 고쳤다 — 그리고 재발을 R5로 끊었다.**
이 문서의 `파일:줄` 좌표를 전수(**6건**) 뽑아 현재 트리와 대조하니 **5건이 어긋나 있었고, 그중 4건은
직전 패스(6차)가 만든 것**이다. 6차 패스가 `cmd/control-plane/main.go`에서 주석 19줄을 지우자
`signal.NotifyContext` 선언이 밀렸고(좌표 1건), **6차 패스가 지운 바로 그 주석**을 가리키던 좌표가
셋 더 있었다. 나머지 1건은 자매 원장에서 인용해 온 선재 부패이고, 6번째(AC-F3 문단을 가리키는 것)는
지금은 맞지만 같은 형태다.

**숫자를 고쳐 적지 않았다.** 그렇게 하면 이번 패스의 제거 57줄이 다시 좌표를 밀어 낸다. 대신 전부
**심볼 앵커**로 바꿨고(이 문서 대부분이 이미 그 형태다 — `ScanOnce` doc · `sessionPodRefs` doc),
「판정 절차」 3번의 *「어느 파일 몇 줄이」* 라는 지시 자체를 *「어느 파일의 어느 심볼이」* 로 고쳤다.
좌표 부패의 원인은 개별 실수가 아니라 **절차가 줄 번호를 요구한 것**이었다.

그리고 **R5**가 그것을 기계로 강제한다. R1~R4 중 어느 것도 좌표를 재측정하지 않아 이 축은
**영원히 초록**이었다 — 지문과 합계는 기계가 지키는데 좌표만 사람이 지켰다. R5는 이 문서 안의
`파일:줄` 형태를 금지한다. 순수 경로 포인터(`docs/test/e2e.md`)는 권장 형태라 걸리지 않는다.
