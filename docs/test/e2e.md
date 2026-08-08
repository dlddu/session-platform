# 테스트 문서: kind 기반 풀스택 e2e

`make test-integration`이 핸들러를 **인프로세스**로 띄워 검증한다면, e2e 스위트는
`deploy/`로 **kind 클러스터에 배포된 control-plane(SUT)** 를 대상으로 API와 브라우저
양쪽에서 해피패스를 종단 검증한다.

> **충실도**: PodOrchestrator와 StateStore 모두 실 구현이다 — 세션 생성 시 **진짜 Pod
> 오브젝트**가 1:1로 기동되고(client-go), 세션 상태는 **ConfigMap + Lease**에 저장된다(클러스터에
> 배포된 SUT 기준). 세션 pod는 **실 data plane 에이전트 이미지**(`data-plane/`,
> `session-platform/data-plane:dev`를 kind에 load)로 뜨므로 pod 안에서 PTY에 연결된
> 인터랙티브 쉘이 실제로 기동되고, create는 pod Ready에 더해 **쉘 도달(Reach, attach 스트림
> open/close)**까지 확인한 뒤에야 `active`를 반환한다(AC-D1). SUT는 **2 replica**로 배포되어
> 상태를 공유하므로 교차-replica 원자성(AC-C1)을 실제로 검증한다. Checkpointer(CRIU)는
> 오버레이(`deploy/`)에서 게이트 ON(agent-driven in-pod CRIU + MinIO 아카이브 저장소 +
> test-only snapshot 트리거)으로 배포되어, **snapshot→pod 회수→접근 시 새 pod로 복원→쉘
> 상태 보존 왕복이 실 단언으로 검증**된다(AC-B2/B3/D4 파일; e2e 워크플로의 CRIU 프로브가
> 러너 커널의 in-pod criu 지원을 확인). 다만 **운영 idle→snapshot 트리거(reaper 타이밍
> 정책)는 아직 미확정**(`TODO(policy)`, AC-B1)이라, 트리거 없이 둔 세션은 base 경로에서
> 여전히 `active`로 머문다.
> 검증 범위는 아래 「AC ↔ e2e 파일 매핑」 표가 정본이다 — **AC 21개 중 14개가 전용 e2e
> 파일을 갖고, AC-B1은 예외 등재, AC-E1~E6(claude-code 워크로드)은 구현 미착수라 공백**이다.
> 남은 미검증 분기는 `idle` 상태에 도달할 방법이 없어서 생긴 것뿐이며(read/write/switch의
> idle 경로), AC-B1의 트리거 정책과 함께 풀린다.

## 빠른 실행 (로컬)

전제: Docker, [kind](https://kind.sigs.k8s.io), `kubectl`, Go 1.24+, Node 22+.

```bash
make check-ac-mapping                # AC ↔ e2e 1:1 정합성 (정적 — 클러스터 불필요)
make e2e-up                          # kind 생성 + 이미지 빌드/load + deploy/ 적용 + 헬스 대기
cd control-plane && go test -tags=e2e ./test/...   # API e2e (AC별 전용 파일 + 스모크)
cd web && npx playwright test        # 브라우저 e2e (smoke + journeys/) — 최초 1회 `npx playwright install chromium`
make e2e-down                        # kind 클러스터 제거
```

`make e2e-api` / `make e2e-web` / `make e2e`(둘 다)도 같은 일을 한다. 두 스위트 모두
`E2E_BASE_URL`(기본 `http://localhost:8080`)로 SUT를 찾으므로, 다른 곳에 떠 있는
control-plane을 대상으로도 그대로 돌릴 수 있다. 매핑 체크는 SUT 없이 언제든 돌아간다.

## 매니페스트 구조 (kustomize base/overlay)

프로덕션 `k8s/`가 **base**(kustomization.yaml: rbac·deployment·service)이고,
`deploy/`는 그 위에 kind 전용 차이만 얹는 **overlay**다:

- `images` 변환으로 control-plane 이미지를 `ghcr.io/...:latest` → 로컬 빌드
  `session-platform/control-plane:dev`(+ `imagePullPolicy: IfNotPresent`)로 교체한다.
- control-plane Service를 **NodePort**(`nodePort: 30080`)로 patch한다(base는 ClusterIP).
- control-plane Deployment를 **2 replica**로 patch해 교차-replica 상태 공유(AC-C1)를 e2e에서
  검증한다(base는 `replicas: 1`; 프로덕션 스케일은 별개 운영 결정).

`kubectl apply -k deploy/`(= `scripts/e2e/up.sh`) 한 줄로 base + patch가 적용된다.
Flux는 `k8s/`를 그대로 적용한다.

## SUT 도달 방식

- overlay가 control-plane Service를 **NodePort**(`nodePort: 30080`)로 patch한다.
- `deploy/kind-config.yaml`의 `extraPortMappings`가 host `:8080` → node `:30080`을 연결한다.
- 따라서 백그라운드 port-forward 없이 `http://localhost:8080`으로 SUT에 직결된다.
- NodePort는 **overlay에 한정**된다. 프로덕션 base(`k8s/`)의 Service는 ClusterIP 그대로다.

`scripts/e2e/up.sh`는 멱등하다 — 클러스터가 이미 있으면(CI의 `helm/kind-action`이 만든
경우) 생성 단계를 건너뛰고 build/load/deploy/대기만 수행한다.

## CI

`.github/workflows/e2e.yml`이 `control-plane/**`·`data-plane/**`·`web/**`·`deploy/**`·
`scripts/e2e/**`·`Makefile` 변경 PR과 `workflow_dispatch`에서만 돈다(무관 PR은 트리거되지
않음). 흐름:
kind 생성(`helm/kind-action`) → `make e2e-up` → `go test -tags=e2e` → Playwright. 실패 시
Playwright 리포트/trace를 아티팩트로 올린다. ci.yml의 lint/unit/build/integration 잡은 종전대로
모든 PR에서 돌고, **envtest 잡**이 실 kube-apiserver로 CAS/Lease 단일-승자(AC-C1)를 검증한다.

ci.yml에는 e2e 파일을 클러스터 없이 지키는 게이트 둘이 더 있다(둘 다 모든 PR에서 실행):

- **`AC ↔ e2e 1:1 mapping` 잡** — `scripts/e2e/check-ac-mapping.sh`. 아래 매핑 표·예외
  목록·집계가 실제 파일 헤더 선언 및 `docs/prd`의 AC 집합과 일치하는지 검사한다. 파일을
  추가·이동·삭제했는데 등재를 갱신하지 않으면 여기서 막힌다.
- **`go vet (e2e build tag)`** — e2e 스위트는 빌드 태그 뒤에 있어 기본 vet이 컴파일하지
  않는다. 이 스텝이 없으면 e2e 파일의 타입 오류를 kind 잡에 가서야 알게 된다.


## AC ↔ e2e 매핑 규칙 (1:1)

이 문서는 **AC ↔ e2e 파일 매핑의 등재 SSOT**다. AC 자체의 SSOT는 `../prd`의 `### AC-…`
헤딩이고, 매핑의 SSOT는 각 매칭 단위 파일 헤더의 `// 검증 AC:` 선언이다. 이 절의 표들은
그 둘을 잇는 등재이며, `scripts/e2e/check-ac-mapping.sh`가 셋의 일치를 기계적으로 강제한다
(`make check-ac-mapping`, ci.yml에서 모든 PR에 실행 — 클러스터 불필요).

**매칭 단위**는 두 스위트를 합친 하나의 공간이다:

- `control-plane/test/e2e_*_test.go` — Go API e2e (빌드 태그 `e2e`)
- `web/e2e/*.spec.ts` — Playwright (**최상위만**)

규칙:

1. **AC → 파일 유일**: 예외 목록에 없는 AC는 자신을 선언한 파일을 정확히 1개 갖는다.
2. **파일 → AC 유일**: 매칭 단위 파일은 정확히 1개의 AC만 선언한다. 셋업으로 다른 AC의
   경로를 경유하는 것은 검증으로 세지 않는다 — 선언한 것만 센다.
3. **비-AC 파일**: 스모크/인프라 검증만 하는 파일은 `// 검증 AC: 없음 (스모크·인프라)`로
   선언하고 아래 「비-AC 파일 등재」에 올린다. 어디에도 없는 파일은 고아(위반).
4. **예외 목록**: e2e 자동 검증이 곤란한 AC는 사유·대체 검증 수단과 함께 아래 「AC 예외
   목록」에 올린다. 등재된 AC는 파일이 없어도 위반이 아니다.
5. **참조 무결성**: 존재하지 않는 AC를 선언하거나 예외로 등재하면 위반.
6. **집계 일치**: 아래 「집계」가 1~5의 실제 상태와 일치해야 한다.

## AC ↔ e2e 파일 매핑

파일 단위 표다(테스트 함수 단위가 아니다). 한 AC의 전용 파일은 두 스위트 중 **한쪽에만**
있다.

<!-- ac-mapping:begin -->
| AC | 파일 | 스위트 | 무엇을 단언하나 |
| --- | --- | --- | --- |
| AC-A1 | `control-plane/test/e2e_a1_plane_separation_test.go` | go | 세션 워크로드가 control-plane pod 밖의 자기 Pod에서 돈다 + control-plane pod엔 워크로드(쉘)가 없다(distroless — exec 실패) |
| AC-A2 | `control-plane/test/e2e_a2_dedicated_pod_test.go` | go | 생성 = active + 전용 pod, `session-id` 라벨 1:1, N 세션 → 고유 Pod N개 |
| AC-A3 | `control-plane/test/e2e_a3_pod_reclaim_test.go` | go | 동결 시 API `pod:""` + 클러스터 그라운드-트루스로 Pod 삭제/terminating 확인 |
| AC-B2 | `control-plane/test/e2e_b2_snapshot_restore_test.go` | go | snapshot 세션 접근(switch) → `active` 전이 + **새 pod**로 복원(동결 전 pod가 아님) |
| AC-B3 | `control-plane/test/e2e_b3_restore_integrity_test.go` | go | 동결 전 발급 커서가 복원 후에도 유효(델타만), `offset=0`은 동결 전·후 전체 이력을 순서대로 |
| AC-C1 | `control-plane/test/e2e_c1_atomic_state_test.go` | go | 2-replica 공유 store에 24-way 동시 요청 → 단일 active 수렴·중복 pod 없음 |
| AC-C2 | `control-plane/test/e2e_c2_read_branches_test.go` | go | read 분기: `active` / `snapshot->restore->read`, 호출 후 항상 active |
| AC-C3 | `control-plane/test/e2e_c3_write_branches_test.go` | go | write 분기: `active` / `snapshot->restore->write`(거부 아님), 호출 후 항상 active |
| AC-C4 | `control-plane/test/e2e_c4_session_switch_test.go` | go | active 대상 switch = no-op(재기동 없음), 다건 세션을 오가도 각자 상태·pod 보존 |
| AC-D1 | `control-plane/test/e2e_d1_pty_shell_test.go` | go | 세션 pod 안 PTY 부착 프로세스 정확히 1개이며 `bash` |
| AC-D2 | `control-plane/test/e2e_d2_shell_write_test.go` | go | write → 쉘 stdin 주입(계산 마커로 실행 확인), 3초 명령에 비블로킹, 연속 write가 같은 쉘로 |
| AC-D3 | `control-plane/test/e2e_d3_read_cursor_test.go` | go | 커서 read는 델타만, `offset=0` 재조회는 전체 이력 순서 보존(비파괴) |
| AC-D4 | `control-plane/test/e2e_d4_process_tree_test.go` | go | 동결 전 `export`/`cd`가 복원 후 그대로(`$D4MARK`, `pwd`) — B3의 인메모리 구체 마커 |
| AC-D5 | `control-plane/test/e2e_d5_idle_definition_test.go` | go | read만 해도 `lastAccess` 갱신 / 쉘 자체 출력만으로는 미갱신(GET은 접근으로 세지 않음) |
<!-- ac-mapping:end -->

## AC 예외 목록

e2e 자동 검증이 곤란해 전용 파일을 두지 않는 AC. 등재된 AC는 파일이 없어도 위반이 아니다.

<!-- ac-exceptions:begin -->
| AC | 사유 | 대체 검증 수단 |
| --- | --- | --- |
| AC-B1 | 운영 트리거가 **60분 실시간 유휴 대기**라 e2e에서 그대로 재현할 수 없고, 트리거 정책(grace/override, 바쁜 쉘 처리) 자체가 아직 미확정이다 — `control-plane/internal/service/session.go`·`reaper.go`의 `TODO(policy)`. 상태 전이·pod 회수라는 **관측 계약**은 test-only `POST /sessions/{id}/snapshot`으로 이미 AC-A3·B2·B3·D4 파일이 검증하고 있어, 남은 미검증분은 **타이밍 정책** 하나다. | `internal/service/reaper_test.go`(유휴 경계 60분 전/후 동작을 가짜 시계로 단위 검증) + AC-A3/B2/B3/D4의 e2e(동결→회수→복원 계약). 트리거 정책이 확정되면 예외를 걷고 `e2e_b1_*_test.go`를 신설한다. |
<!-- ac-exceptions:end -->

## 비-AC 파일 등재

AC 대신 스모크/인프라를 검증하는 매칭 단위 파일(규칙 3).

<!-- ac-nonac:begin -->
| 파일 | 스위트 | 무엇을 검증하나 |
| --- | --- | --- |
| `control-plane/test/e2e_smoke_test.go` | go | healthz 200 / `{"status":"ok"}`, 생성→목록→조회 API 표면 왕복, 없는 id → 404 에러 매핑 |
| `web/e2e/smoke.spec.ts` | playwright | SPA 부팅·baseURL 배선(Sessions 콘솔 헤딩 + New session 진입점) |
<!-- ac-nonac:end -->

## 매칭 단위 밖

아래는 e2e로 돌지만 **AC 매칭 공간 밖**이므로 1:1 계산에 들어가지 않는다.

| 대상 | 왜 밖인가 |
| --- | --- |
| `control-plane/test/harness_shared_test.go` | 공용 하네스(HTTP DTO·헬퍼·kube 클라이언트)만 담는다. 파일명이 `e2e_`로 시작하지 않아 매칭 단위가 아니다. |
| `web/e2e/journeys/**.spec.ts` (J1·J3·J5·deferred) | 여정 spec은 **여정 하나를 통째로** 훑어 여러 AC의 화면을 경유한다. 통합 1:1 공간에서 각 AC의 주검증은 더 날카로운 단언을 가진 Go 파일이 소유하므로(예: write 비블로킹, PTY 프로세스 수, 커서 델타), 여정 spec은 최상위 밖으로 내려 매칭 대상에서 제외하고 **브라우저 회귀 커버리지로 계속 실행**한다(playwright `testDir: ./e2e`가 재귀 탐색). 여정 통합 검증을 1:1 공간의 1급 유형으로 올리려면 모델 판정 기준 개정이 필요하다. |
| `control-plane/test/integration_test.go`·`client_orchestrator_test.go` | 빌드 태그 `integration` — 인프로세스 통합. |
| `control-plane/internal/**/*_test.go`, envtest 잡 | 단위·envtest. 예외 목록의 "대체 검증 수단"으로만 인용된다. |
| `scripts/e2e/*.sh`, `deploy/`, `web/playwright.config.ts`, `Makefile`, `.github/workflows/e2e.yml` | 실행 하네스. |

## 집계

<!-- ac-summary:begin -->
- AC 총계: 21
- 예외: 1
- AC 매칭 파일: 14
- 공백: 6 — AC-E1 AC-E2 AC-E3 AC-E4 AC-E5 AC-E6
<!-- ac-summary:end -->

**공백**은 전용 파일도 예외 등재도 아직 없는 AC다. AC-E1~E6(claude-code 워크로드)는 구현이
아직 착수 단계라 e2e를 붙일 대상이 없다 — 구현이 수렴하는 대로 각 AC의 전용 파일을 신설하거나
(외부 LLM 자격증명·비결정 산출물처럼 곤란한 것은) 예외로 등재해 이 표를 0으로 만든다.

## 남은 미검증 분기 (공백은 아님)

전용 파일은 있으나 그 안에서 아직 단언하지 못하는 경로. 전부 **`idle` 상태에 도달할 방법이
없다**는 한 가지 이유이며, AC-B1의 트리거 정책과 함께 풀린다.

| 경로 | 소유 파일 | 막힌 이유 |
| --- | --- | --- |
| read `idle->active->read` | `e2e_c2_read_branches_test.go` | idle 진입 트리거 없음(AC-B1 정책 미확정) |
| write `idle->active->write` | `e2e_c3_write_branches_test.go` | 〃 |
| switch의 idle 대상 승격 | `e2e_c4_session_switch_test.go` | 〃 |
