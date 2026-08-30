# 테스트 문서: kind 기반 풀스택 e2e

`make test-integration`이 핸들러를 **인프로세스**로 띄워 검증한다면, e2e 스위트는
`deploy/`로 **kind 클러스터에 배포된 control-plane(SUT)** 를 대상으로 API와 브라우저
양쪽에서 해피패스를 종단 검증한다.

## e2e 충실도 허용목록 (모킹 최소화 정책)

기본값은 **실환경**이다. PodOrchestrator와 StateStore 모두 실 구현이라 세션 생성 시 **진짜
Pod 오브젝트**가 1:1로 기동되고(client-go), 세션 상태는 **ConfigMap + Lease**에 저장된다
(클러스터에 배포된 SUT 기준). 세션 pod는 **실 data plane 에이전트 이미지**(`data-plane/`,
`session-platform/data-plane:dev`를 kind에 load)로 뜨므로 pod 안에서 PTY에 연결된 인터랙티브
쉘이 실제로 기동되고, create는 pod Ready에 더해 **쉘 도달(Reach, attach 스트림 open/close)**
까지 확인한 뒤에야 `active`를 반환한다(AC-D1). SUT는 **2 replica**로 배포되어 상태를
공유하므로 교차-replica 원자성(AC-C1)을 실제로 검증한다. Checkpointer(CRIU)는
오버레이(`deploy/`)에서 게이트 ON(agent-driven in-pod CRIU + MinIO 아카이브 저장소)으로
배포되고 **제품** snapshot endpoint(`POST /api/v1/sessions/{id}/snapshot`)를 통해
**snapshot→pod 회수→접근 시 새 pod로 복원→쉘 상태 보존 왕복이 실 단언으로 검증**된다
(AC-B2/B3/D4 전용 파일 `e2e_b2_snapshot_restore_test.go`·`e2e_b3_restore_integrity_test.go`·`e2e_d4_process_tree_test.go`; e2e 워크플로의 CRIU 프로브가 러너 커널의 in-pod
criu 지원을 확인). 운영 reaper는 마지막 read/write부터 60분에 도달한 세션을 스캔해
snapshot한다. 다만 배포 e2e에는 60분 시계를 가속하거나 `lastAccess`를 주입하는 제품 API가
없어 실제 시간 경계 시드는 skip이다.

검증 범위는 **생성/목록/조회/switch·read·write 해피패스 + 실 Pod 단언(AC-A1/A2) + PTY 쉘
런타임 단언(AC-D1) + 쉘 stdin/stdout 시맨틱(AC-D2/D3: write→쉘 stdin 주입, read→offset
커서 델타·offset=0 전체 재조회) + 교차-replica 일관성(AC-C1) + CRIU 왕복(AC-B2/B3/D4)** 이다.
reaper 시간 경계(B1)와 중간 `idle` 상태의 read/write 분기(C2/C3)는 각각 제어 가능한 시계와
operational idle-state producer가 생기면 배포 e2e로 채운다.

충실도를 낮추는 치환은 **아래에 등재된 것만** 허용한다. 카테고리는 넷뿐이다:

- `GATE` — 프로덕션 기능 게이트가 off일 때 no-op으로 떨어지는 분기. off가 **프로덕션의 의도된
  동작**이고, 그 기능을 검증하는 e2e는 **오버레이에서 게이트를 on으로 올려** 실 경로를 탈 때만.
- `TRIG` — 실환경에서 결정적으로 유발할 수 없는 상태 전이를 위한 test 전용 트리거. **운영
  트리거가 별도로 실재**하고 그 대기 시간만 단축할 때. *(현재 0건 — 과거의 `SNAPSHOT-TRIG`는
  수동 아카이브 기능이 들어오면서 제품 endpoint로 승격돼 seam 자체가 사라졌다.)*
- `EXT` — 클러스터에 존재시킬 수 없는 외부 시스템 의존. kind에 실물로 배포 가능하면 그쪽이
  우선이라 쓸 수 없다(MinIO가 그 예 — 실 배포이므로 모킹이 아니다).
- `NET` — web e2e의 네트워크 인터셉트. 실 SUT가 요청 시점에 낼 수 없는 상태(서버 실패 응답·
  응답 지연 주입)로만 한정한다.

편의를 위한 치환 — 대기 시간 단축, 어서션 단순화, 플레이키 무마, **미구현 우회 stub**, 시드로
만들 수 있는 데이터의 고정 — 은 등재 대상이 아니라 **제거** 대상이다. 판정이 애매하면 제거
쪽으로 기운다. 예외는 늘지 않는 방향으로만 관리한다.

자격을 만족하지 못하는 치환이 이미 코드에 있으면 그것은 **승인 예외가 아니라 위반**이다.
숨기지도 승인하지도 않고 아래 「미해소 위반」에 상한과 함께 등재해, 줄어드는 방향으로만
움직이게 한다.

### 표기 규약

허용된 seam은 그 지점 **직전 줄**에 사유 주석을 단다. YAML·셸·Makefile은 `#` 주석을 쓰되
토큰은 동일하게 유지한다.

```
// mock-exception: <CODE> — <실환경으로 불가능한 이유 한 줄>
```

주석만 있고 미등재이거나, 등재만 있고 코드에 없으면 drift다. `scripts/check-fidelity-allowlist.py`가
아래 네 블록과 코드를 **양방향으로** 대조해 강제한다 — `make check-fidelity`(= `make lint`에 포함),
그리고 모든 PR에서 도는 `ci.yml`의 `fidelity` 잡.

### 등재된 seam

<!-- fidelity:registry -->
| CODE | 카테고리 | e2e에서 실제로 구동되는 구간 | 치환으로 검증되지 않는 잔여 |
| --- | --- | --- | --- |
| `CRIU-GATE` | `GATE` | e2e SUT는 항상 `deploy/` 오버레이로 뜨고(`scripts/e2e/up.sh`의 `kubectl apply -k deploy/`) 거기서 게이트가 **ON**이라 실 CRIU 경로를 탄다 — 에이전트가 pod 안에서 쉘 트리를 dump/restore하고, 아카이브는 인클러스터 MinIO를 향해 **프로덕션과 같은 S3 코드 경로**로 오간다. 동결은 제품 endpoint(`POST /api/v1/sessions/{id}/snapshot`)로 유발되고, 왕복 전체가 AC-B2/B3/D4 전용 파일의 실 단언이다. | 프로덕션 base(`k8s/`, `CRIU_ENABLED: "false"`)의 쉘 동결·복원은 **no-op 스텁이고 어떤 e2e도 그 경로를 타지 않는다**. 검증된 런타임이 서고 base가 on으로 올라가기 전까지, 프로덕션 구성에서 쉘 세션 동결이 상태를 실제로 보존하는지는 미검증이다. |
| `DELETE-CONFLICT-ERR` | `NET` | `web/e2e/journeys/session-deletion.spec.ts`의 스냅샷 삭제 테스트는 **실 SUT 위에서** 돈다 — 세션을 `POST /api/v1/sessions`로 실제로 만들고 제품 `POST /api/v1/sessions/{id}/snapshot`으로 진짜 `snapshot` 상태(pod 회수 포함)까지 얼린 뒤, `/restore/{id}` 렌더·세션 목록·단건 조회·**재시도 DELETE(204)** 가 모두 배포된 control-plane의 실 응답이다. 인터셉트가 만드는 것은 **첫 DELETE 한 번의 409 응답뿐**이고, 나머지 요청은 같은 핸들러에서 `route.continue()`로 그대로 흘려보낸다. | 409를 낳는 **진짜 경합** — 다른 라이프사이클 연산이 세션 Lease를 쥔 상태에서 들어온 DELETE(`service.Terminate`의 `store.Lock` → `session.ErrConflict`) — 자체는 브라우저 e2e에서 검증되지 않는다. 주입은 UI의 실패 표시·재시도 경로만 확인하고, 서버가 그 상태에서 실제로 409를 내는지는 control-plane 단위 테스트와 envtest의 CAS/Lease 충돌 케이스가 담당한다. |
<!-- /fidelity:registry -->

### 미해소 위반 (승인된 예외가 아니다)

여기 있는 치환은 **정책 위반이며 제거 대상**이다. 등재의 목적은 정당화가 아니라 회계다 —
개수에 상한이 걸려 있어(`상한` 집계) 새 위반이 들어오면 CI가 막고, 줄어들 때만 상한을 함께
내린다.

남은 두 파일 모두 `page.route("**/api/v1/**")`로 **control-plane API 표면 전체**를 가로채 세션
목록·단건·SSE 스트림·snapshot·delete를 손으로 지은 픽스처로 응답한다. 그중 일부(config 실패,
응답 보류로 만드는 중간 UI 상태, past-end cursor `reset` 이벤트)는 `NET` 자격을 만족하지만,
그것들이 **API 전체를 삼키는 인터셉트 안에** 들어 있어 행 단위로 승인할 수 없다. 실 SUT가 낼
수 있는 데이터(세션 목록·상태·pod)까지 함께 위조되기 때문이다.

제거가 이 슬라이스에서 끝나지 않는 이유는 두 스위트의 검증 대상이 외부 LLM 제공자에 달려
있고 e2e SUT가 그 제공자를 **의도적으로 도달 불가**로 배포하기 때문이다
(`deploy/claude-code-credentials-secret.yaml`: `base-url: https://127.0.0.1:9`, "Deliberately
unroutable"). 정책의 `EXT` 규칙이 가리키는 해법은 **MinIO 선례대로 인클러스터 가짜 provider를
배포**하고 실 세션 위에서 돌리는 것이다.

*해소된 것*: `web/e2e/journeys/session-deletion.spec.ts`는 여기서 빠졌다. 세션 생성과 동결을 제품 API로
돌려 실 SUT 위에서 돌게 만들고, 남은 인터셉트를 첫 DELETE의 409 응답 하나로 좁혀
`DELETE-CONFLICT-ERR`(`NET`)로 **승인 등재**했다. 그만큼 상한도 6에서 4로 내려갔다.

<!-- fidelity:violations -->
| 파일 | 토큰 | 무엇을 위조하는가 | 제거 경로 (선결조건) |
| --- | --- | --- | --- |
| `web/e2e/journeys/j6-agent-prompt-loop.spec.ts` | `.route(` | `installAgentApi`가 12개 테스트 전부에 `/api/v1/**` 핸들러를 설치해 세션·config·SSE 스트림을 위조한다. | 인클러스터 가짜 Claude provider를 `deploy/`에 배포(MinIO 선례)하고 `claude-code-credentials`의 `base-url`을 그쪽으로 돌린 뒤, 실 claude-code 세션 위에서 재작성. |
| `web/e2e/journeys/j6-agent-prompt-loop.spec.ts` | `route.fulfill(` | 위와 같은 핸들러의 응답 생성부 — JSON·`text/event-stream` 본문을 직접 짓는다. | 위와 같다. `reset`·`output` 이벤트는 가짜 provider의 결정적 응답으로 실 SUT가 생성하게 한다. |
| `web/e2e/journeys/manual-archive.spec.ts` | `.route(` | active claude-code 세션과 그 목록·단건·stream을 위조한다. | 실 SUT에서 claude-code 세션을 만들고 제품 `POST /snapshot`으로 아카이브. 중간 "Archiving…" 상태만 남으면 그 지연 주입은 `NET`(LAT)으로 **행 단위 승인 가능**하다. |
| `web/e2e/journeys/manual-archive.spec.ts` | `route.fulfill(` | 위 핸들러의 응답 생성부 + snapshot 응답을 보류해 중간 상태를 만든다. | 위와 같다. |
<!-- /fidelity:violations -->

### seam 지문 회계

seam 스캔이 잡는 `(파일, 토큰)` 쌍마다 한 행이다. 등재 seam에 귀속되거나, `위반`으로
회계되거나, 등재 대상이 아닌 이유를 밝히거나 — **셋 중 하나여야** 한다. 회계에 없는 쌍이
코드에 생기면 그것이 미등재 seam이고 CI가 막는다(체커 R5).

<!-- fidelity:ledger -->
| 파일 | 토큰 | CODE | 역할 / 등재하지 않는 이유 |
| --- | --- | --- | --- |
| `control-plane/cmd/control-plane/main.go` | `CRIU_ENABLED` | `CRIU-GATE` | 게이트 값 로드(`envBool`)와 그 값에 달린 체크포인트 저장소·체크포인터 선택. |
| `control-plane/cmd/control-plane/main.go` | `NewStubCheckpointer` | `CRIU-GATE` | 게이트 off일 때 주입되는 no-op 체크포인터. |
| `deploy/kustomization.yaml` | `CRIU_ENABLED` | `CRIU-GATE` | base의 off를 on으로 올리는 `env/2` replace. |
| `k8s/deployment.yaml` | `CRIU_ENABLED` | `CRIU-GATE` | 프로덕션 base의 게이트 값(`"false"`). |
| `web/e2e/journeys/j6-agent-prompt-loop.spec.ts` | `.route(` | `위반` | 미해소 위반 — 위 표 참조. |
| `web/e2e/journeys/j6-agent-prompt-loop.spec.ts` | `route.fulfill(` | `위반` | 미해소 위반 — 위 표 참조. |
| `web/e2e/journeys/manual-archive.spec.ts` | `.route(` | `위반` | 미해소 위반 — 위 표 참조. |
| `web/e2e/journeys/manual-archive.spec.ts` | `route.fulfill(` | `위반` | 미해소 위반 — 위 표 참조. |
| `web/e2e/journeys/session-deletion.spec.ts` | `.route(` | `DELETE-CONFLICT-ERR` | 스냅샷 삭제 테스트가 첫 DELETE만 가로채려고 그 세션 URL에 거는 핸들러. |
| `web/e2e/journeys/session-deletion.spec.ts` | `route.fulfill(` | `DELETE-CONFLICT-ERR` | 주입되는 단 하나의 응답 — 409 `session state changed concurrently`(실서버 메시지와 동일). |
| `web/e2e/journeys/session-deletion.spec.ts` | `route.continue(` | `DELETE-CONFLICT-ERR` | 같은 핸들러의 통과 분기. 단건 GET도, **재시도 DELETE도** 실 control-plane이 응답하게 한다 — 이 줄이 없으면 승인 범위가 409 하나를 넘어선다. |
| `.github/workflows/e2e.yml` | `test-only` | `—` | CRIU 프로브 주석이 "이제 test 전용 트리거가 없다"는 사실을 밝히는 서술. 치환이 아니다. |
| `Makefile` | `CRIU_ENABLED` | `—` | `make test-integration` 설명 주석. 인프로세스 통합 하네스(`//go:build integration`)는 e2e SUT 경로가 아니라 이 정책의 범위 밖이다. |
| `control-plane/test/harness_shared_test.go` | `E2E_SESSION_NAMESPACE` | `—` | 어서션용 kube 클라이언트가 볼 namespace 지정(기본 `default`). `E2E_BASE_URL`과 같은 성격의 배선이라 SUT 충실도를 낮추지 않는다. 지문에서 아예 빼는 편이 맞지만 그것은 정합성 모델 definition 개정 사안이라, 여기서는 비-seam으로 회계만 맞춘다. |
| `deploy/minio.yaml` | `test-only` | `—` | MinIO가 test 전용 저장소가 **아님**을 밝히는 서술이다 — 프로덕션과 같은 S3 코드 경로를 타는 실 배포라 `EXT` 대상이 아니다. |
| `web/e2e/journeys/deferred.spec.ts` | `test-only` | `—` | deferred skip 사유 산문. 인터셉트가 아니다. |
<!-- /fidelity:ledger -->

### 집계

<!-- fidelity:summary -->
- 등재 seam **2**개 — GATE **1** · TRIG **0** · EXT **0** · NET **1**
- 코드 마커 지점 **4** / 마커 파일 **4**
- 지문 회계 행 **16** — 등재 귀속 7 · 미해소 위반 **4** · 비-seam **5**
- web e2e 인터셉트 **7**건 (승인 3 · 위반 4, 상한 **4**)
<!-- /fidelity:summary -->

이 숫자들도 체커가 실제와 대조한다(R8) — 표만 고치고 집계를 잊으면 실패한다. 상한을 넘는
인터셉트가 들어오면 R9가 막는다.

> **AC 검증 범위**: 아래 「AC ↔ e2e 파일 매핑」이 정본이다 — **AC 21개 중 14개가 전용 e2e
> 파일을 갖고, AC-B1은 예외 등재, AC-E1~E6(claude-code 워크로드)은 공백**이다(사유는 「집계」
> 절). 남은 미검증 분기는 `idle` 상태에 도달할 방법이 없어서 생긴 것뿐이며(read/write/switch의
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

`.github/workflows/e2e.yml`이 `control-plane/**`·`data-plane/**`·`web/**`·`deploy/**`·`k8s/**`·
`scripts/e2e/**`·`Makefile`·e2e workflow 자체 변경 PR과 `workflow_dispatch`에서만 돈다(무관 PR은
트리거되지 않음). 흐름:
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
| AC-B1 | 운영 트리거가 **60분 실시간 유휴 대기**라 e2e에서 그대로 재현할 수 없고, 트리거 정책(grace/override, 바쁜 쉘 처리) 자체가 아직 미확정이다 — `control-plane/internal/service/session.go`·`reaper.go`의 `TODO(policy)`. 상태 전이·pod 회수라는 **관측 계약**은 제품 `POST /sessions/{id}/snapshot`으로 이미 AC-A3·B2·B3·D4 파일이 검증하고 있어, 남은 미검증분은 **타이밍 정책** 하나다. | `internal/service/reaper_test.go`(유휴 경계 60분 전/후 동작을 가짜 시계로 단위 검증) + AC-A3/B2/B3/D4의 e2e(동결→회수→복원 계약). 트리거 정책이 확정되면 예외를 걷고 `e2e_b1_*_test.go`를 신설한다. |
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
| `web/e2e/journeys/**.spec.ts` (j1·j3·j5·j6·deferred·session-deletion·manual-archive) | 여정 spec은 **여정 하나를 통째로** 훑어 여러 AC의 화면을 경유한다. 통합 1:1 공간에서 각 AC의 주검증은 더 날카로운 단언을 가진 Go 파일이 소유하므로(예: write 비블로킹, PTY 프로세스 수, 커서 델타), 여정 spec은 최상위 밖으로 내려 매칭 대상에서 제외하고 **브라우저 회귀 커버리지로 계속 실행**한다(playwright `testDir: ./e2e`가 재귀 탐색). 여정 통합 검증을 1:1 공간의 1급 유형으로 올리려면 모델 판정 기준 개정이 필요하다.<br>**2026-08-30 추가**: `j6-agent-prompt-loop`(#24)·`session-deletion`(#27)·`manual-archive`(#36)도 같은 이유로 여기에 있다 — j6는 E1~E6를 한 파일에서 묶어 훑고, `session-deletion`의 자원 회수 계약은 `e2e_a3_pod_reclaim_test.go`가 이미 AC-A3로 소유하며(양쪽에서 선언하면 규칙 1 중복), `manual-archive`는 claude-code 아카이브 UI 경로다. 셋 다 브라우저 회귀 커버리지로 계속 실행된다. |
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

**공백**은 전용 파일도 예외 등재도 아직 없는 AC다. AC-E1~E6(claude-code 워크로드)가 여기
남는다.

> **왜 이번에도 E 계열을 범위 밖에 두는가** *(2026-08-30 재판정)*. 원래 근거였던 "구현 전무"는
> #24 *Implement Claude Code workload end to end* 로 **더 이상 성립하지 않는다**. 그럼에도 범위
> 밖으로 두는 이유는 둘이다. ① `web/e2e/journeys/j6-agent-prompt-loop.spec.ts`가 E1~E6를 **한
> 파일에서 묶어** 검증해(654줄), 규칙 2(파일당 AC 1개)를 만족시키려면 AC별 6분할이라는 별도
> 슬라이스가 필요하다. ② j6와 `manual-archive`는 위 「미해소 위반」 원장에 올라 있는
> **승인되지 않은 네트워크 인터셉트**를 쓴다 — 지금 이들을 AC 전용 파일로 승격하면 "모킹으로
> 검증된 AC"를 1:1 정본에 박아 넣게 된다. 실 SUT 전환이 먼저이고 그 작업은 이미 진행 중이다
> (`manual-archive`는 PR #42). 그 전환이 끝난 뒤 E 계열 전용 파일을 신설하거나, 외부 LLM
> 자격증명·비결정 산출물처럼 곤란한 것은 예외로 등재해 이 표를 0으로 만든다.

## 남은 미검증 분기 (공백은 아님)

전용 파일은 있으나 그 안에서 아직 단언하지 못하는 경로. 전부 **`idle` 상태에 도달할 방법이
없다**는 한 가지 이유이며, AC-B1의 트리거 정책과 함께 풀린다.

| 경로 | 소유 파일 | 막힌 이유 |
| --- | --- | --- |
| read `idle->active->read` | `e2e_c2_read_branches_test.go` | idle 진입 트리거 없음(AC-B1 정책 미확정) |
| write `idle->active->write` | `e2e_c3_write_branches_test.go` | 〃 |
| switch의 idle 대상 승격 | `e2e_c4_session_switch_test.go` | 〃 |
