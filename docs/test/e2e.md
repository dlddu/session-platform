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
(`TestDeferred_CRIUIntegrity`, AC-B2/B3/D4; e2e 워크플로의 CRIU 프로브가 러너 커널의 in-pod
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
| `CRIU-GATE` | `GATE` | e2e SUT는 항상 `deploy/` 오버레이로 뜨고(`scripts/e2e/up.sh`의 `kubectl apply -k deploy/`) 거기서 게이트가 **ON**이라 실 CRIU 경로를 탄다 — 에이전트가 pod 안에서 쉘 트리를 dump/restore하고, 아카이브는 인클러스터 MinIO를 향해 **프로덕션과 같은 S3 코드 경로**로 오간다. 동결은 제품 endpoint(`POST /api/v1/sessions/{id}/snapshot`)로 유발되고, 왕복 전체가 `TestDeferred_CRIUIntegrity`의 실 단언이다(AC-B2/B3/D4). | 프로덕션 base(`k8s/`, `CRIU_ENABLED: "false"`)의 쉘 동결·복원은 **no-op 스텁이고 어떤 e2e도 그 경로를 타지 않는다**. 검증된 런타임이 서고 base가 on으로 올라가기 전까지, 프로덕션 구성에서 쉘 세션 동결이 상태를 실제로 보존하는지는 미검증이다. |
| `DELETE-CONFLICT-ERR` | `NET` | `web/e2e/session-deletion.spec.ts`의 스냅샷 삭제 테스트는 **실 SUT 위에서** 돈다 — 세션을 `POST /api/v1/sessions`로 실제로 만들고 제품 `POST /api/v1/sessions/{id}/snapshot`으로 진짜 `snapshot` 상태(pod 회수 포함)까지 얼린 뒤, `/restore/{id}` 렌더·세션 목록·단건 조회·**재시도 DELETE(204)** 가 모두 배포된 control-plane의 실 응답이다. 인터셉트가 만드는 것은 **첫 DELETE 한 번의 409 응답뿐**이고, 나머지 요청은 같은 핸들러에서 `route.continue()`로 그대로 흘려보낸다. | 409를 낳는 **진짜 경합** — 다른 라이프사이클 연산이 세션 Lease를 쥔 상태에서 들어온 DELETE(`service.Terminate`의 `store.Lock` → `session.ErrConflict`) — 자체는 브라우저 e2e에서 검증되지 않는다. 주입은 UI의 실패 표시·재시도 경로만 확인하고, 서버가 그 상태에서 실제로 409를 내는지는 control-plane 단위 테스트와 envtest의 CAS/Lease 충돌 케이스가 담당한다. |
<!-- /fidelity:registry -->

### 미해소 위반 (승인된 예외가 아니다)

여기 있는 치환은 **정책 위반이며 제거 대상**이다. 등재의 목적은 정당화가 아니라 회계다 —
개수에 상한이 걸려 있어(`상한` 집계) 새 위반이 들어오면 CI가 막고, 줄어들 때만 상한을 함께
내린다.

남은 한 파일 `web/e2e/j6-agent-prompt-loop.spec.ts`가 `page.route("**/api/v1/**")`로
**control-plane API 표면 전체**를 가로채 세션 목록·단건·config·SSE 스트림·snapshot을 손으로 지은
픽스처로 응답한다. 그중 일부(config 실패, 응답 보류로 만드는 중간 UI 상태, past-end cursor
`reset` 이벤트)는 `NET` 자격을 만족하지만, 그것들이 **API 전체를 삼키는 인터셉트 안에** 들어
있어 행 단위로 승인할 수 없다. 실 SUT가 낼 수 있는 데이터(세션 목록·상태·pod)까지 함께
위조되기 때문이다.

제거가 이 슬라이스에서 끝나지 않는 이유는 **kind SUT가 claude-code 세션을 아예 띄울 수 없기
때문**이고, 선결조건은 하나가 아니라 **둘**이다. 둘이 다 풀려야 j6를 실 세션 위에서 다시 쓸 수
있다.

1. **세션 pod가 기동하지 못한다(먼저 걸리는 벽).** `data-plane/entrypoint.sh`는
   `DATA_PLANE_WORKLOAD=claude-code`일 때 agent를 exec하기 **전에** `K3S_MCP_TOKEN`으로 K3s MCP를
   호출해 단명 GitHub token을 받고 **비공개** `dlddu/plugin-marketplace`에서 플러그인을 설치한다
   (`docs/prd/claude-code-workload.md`의 AC). `set -eu` + `curl --fail`이라 이 단계의 실패는 곧
   컨테이너 종료다. kind e2e가 주입하는 `k3s-mcp-token`은
   `deploy/claude-code-credentials-secret.yaml`의 placeholder이고 `.github/workflows/e2e.yml`도
   실토큰을 넣지 않으므로 bootstrap은 **항상** 실패한다 → pod가 Ready에 이르지 못해
   `POST /api/v1/sessions`(`workloadType: claude-code`)가 애초에 성공할 수 없다. `K3S_MCP_URL`은
   환경 변수로 갈아끼울 수 있지만 marketplace의 GitHub URL은 entrypoint에 상수로 박혀 있다.
   e2e 전용 우회 스위치를 새로 다는 것은 이 정책이 명시적으로 금지하는 **"미구현 우회 stub"**
   이므로 해법이 될 수 없다 — 남는 길은 CI에 실자격을 넣거나(운영 결정), plugin bootstrap을
   기동 필수 조건에서 떼어내는 제품 설계 변경이다.
2. **제공자에 도달할 수 없다.** 1이 풀려도 `claude-code-credentials`의
   `base-url: https://127.0.0.1:9`("Deliberately unroutable")가 남는다. 정책의 `EXT` 규칙이
   가리키는 해법은 **MinIO 선례대로 인클러스터 가짜 provider를 배포**하고 `base-url`을 그쪽으로
   돌려 실 세션이 결정적 응답을 받게 하는 것이다.

*해소된 것*: 직전 슬라이스의 `web/e2e/session-deletion.spec.ts`에 이어
`web/e2e/manual-archive.spec.ts`도 여기서 빠졌다. 워크스페이스의 Archive 액션은 **workload와
무관하게** 렌더되고(`web/src/screens/Workspace.tsx` — 갈리는 것은 문구뿐이다) 그 액션이 부르는
것은 제품 `POST /api/v1/sessions/{id}/snapshot`이므로, 실 SUT에 만든 **실 shell 세션**을 그
버튼으로 얼려 워크스페이스·진행 중 버튼 상태·목록 복귀·토스트·카드 상태·pod 회수를 전부 배포된
control-plane이 응답하게 했다. 인터셉트가 0건이 되어 이 파일은 회계 표에서도 사라졌고, 상한은
4에서 2로 내려갔다. 브라우저 커버리지에서 빠지는 것은 claude-code **문구** 분기
(`Archive now` / `Session archived — pod reclaimed`)뿐이고, 그것은 위 선결조건과 함께 아래
「Deferred 시드」 표에 시드로 남겼다. 앞서 `session-deletion.spec.ts`가 그랬듯 이 전환도
**개수를 줄이려고 인터셉트를 다른 파일로 옮긴 것이 아니다** — 옮기기는 회계 행만 줄이고 치환은
그대로 남기므로 이 정책이 세는 대상을 속이는 짓이다.

<!-- fidelity:violations -->
| 파일 | 토큰 | 무엇을 위조하는가 | 제거 경로 (선결조건) |
| --- | --- | --- | --- |
| `web/e2e/j6-agent-prompt-loop.spec.ts` | `.route(` | `installAgentApi`가 12개 테스트 전부에 `/api/v1/**` 핸들러를 설치해 세션·config·SSE 스트림을 위조한다. | **선결조건 ①**(entrypoint의 K3s MCP + 비공개 marketplace bootstrap 없이는 claude-code pod가 Ready에 못 간다)과 **②**(`claude-code-credentials`의 unroutable `base-url`)를 위 본문대로 해소한 뒤, 실 claude-code 세션 위에서 재작성. ①이 남아 있는 한 ②만 고쳐도 세션을 만들 수 없다. |
| `web/e2e/j6-agent-prompt-loop.spec.ts` | `route.fulfill(` | 위와 같은 핸들러의 응답 생성부 — JSON·`text/event-stream` 본문을 직접 짓는다. | 위와 같다. `reset`·`output` 이벤트는 인클러스터 가짜 provider의 결정적 응답으로 실 SUT가 생성하게 한다. |
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
| `web/e2e/j6-agent-prompt-loop.spec.ts` | `.route(` | `위반` | 미해소 위반 — 위 표 참조. |
| `web/e2e/j6-agent-prompt-loop.spec.ts` | `route.fulfill(` | `위반` | 미해소 위반 — 위 표 참조. |
| `web/e2e/session-deletion.spec.ts` | `.route(` | `DELETE-CONFLICT-ERR` | 스냅샷 삭제 테스트가 첫 DELETE만 가로채려고 그 세션 URL에 거는 핸들러. |
| `web/e2e/session-deletion.spec.ts` | `route.fulfill(` | `DELETE-CONFLICT-ERR` | 주입되는 단 하나의 응답 — 409 `session state changed concurrently`(실서버 메시지와 동일). |
| `web/e2e/session-deletion.spec.ts` | `route.continue(` | `DELETE-CONFLICT-ERR` | 같은 핸들러의 통과 분기. 단건 GET도, **재시도 DELETE도** 실 control-plane이 응답하게 한다 — 이 줄이 없으면 승인 범위가 409 하나를 넘어선다. |
| `.github/workflows/e2e.yml` | `test-only` | `—` | CRIU 프로브 주석이 "이제 test 전용 트리거가 없다"는 사실을 밝히는 서술. 치환이 아니다. |
| `Makefile` | `CRIU_ENABLED` | `—` | `make test-integration` 설명 주석. 인프로세스 통합 하네스(`//go:build integration`)는 e2e SUT 경로가 아니라 이 정책의 범위 밖이다. |
| `control-plane/test/e2e_deferred_test.go` | `E2E_SESSION_NAMESPACE` | `—` | 어서션용 kube 클라이언트가 볼 namespace 지정(기본 `default`). `E2E_BASE_URL`과 같은 성격의 배선이라 SUT 충실도를 낮추지 않는다. 지문에서 아예 빼는 편이 맞지만 그것은 정합성 모델 definition 개정 사안이라, 여기서는 비-seam으로 회계만 맞춘다. |
| `control-plane/test/e2e_deferred_test.go` | `test-only` | `—` | deferred skip 사유 산문("reaper 또는 test 전용 endpoint가 필요하다"). 코드 경로가 아니다. |
| `deploy/minio.yaml` | `test-only` | `—` | MinIO가 test 전용 저장소가 **아님**을 밝히는 서술이다 — 프로덕션과 같은 S3 코드 경로를 타는 실 배포라 `EXT` 대상이 아니다. |
| `web/e2e/deferred.spec.ts` | `test-only` | `—` | deferred skip 사유 산문. 인터셉트가 아니다. |
<!-- /fidelity:ledger -->

### 집계

<!-- fidelity:summary -->
- 등재 seam **2**개 — GATE **1** · TRIG **0** · EXT **0** · NET **1**
- 코드 마커 지점 **4** / 마커 파일 **4**
- 지문 회계 행 **15** — 등재 귀속 7 · 미해소 위반 **2** · 비-seam **6**
- web e2e 인터셉트 **5**건 (승인 3 · 위반 2, 상한 **2**)
<!-- /fidelity:summary -->

이 숫자들도 체커가 실제와 대조한다(R8) — 표만 고치고 집계를 잊으면 실패한다. 상한을 넘는
인터셉트가 들어오면 R9가 막는다.

## 빠른 실행 (로컬)

전제: Docker, [kind](https://kind.sigs.k8s.io), `kubectl`, Go 1.24+, Node 22+.

```bash
make e2e-up                          # kind 생성 + 이미지 빌드/load + deploy/ 적용 + 헬스 대기
cd control-plane && go test -tags=e2e ./test/...   # API e2e
cd web && npx playwright test        # 브라우저 e2e (J1, J3, J5, J6, smoke) — 최초 1회 `npx playwright install chromium`
make e2e-down                        # kind 클러스터 제거
```

`make e2e-api` / `make e2e-web` / `make e2e`(둘 다)도 같은 일을 한다. 두 스위트 모두
`E2E_BASE_URL`(기본 `http://localhost:8080`)로 SUT를 찾으므로, 다른 곳에 떠 있는
control-plane을 대상으로도 그대로 돌릴 수 있다.

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

## 현재 커버되는 시나리오

| 검증 | 스위트 | AC |
| --- | --- | --- |
| healthz 200 / `{"status":"ok"}` | go API | — |
| 생성 → `active` + 전용 pod, 3건 → 고유 pod 3개 | go API | A1, A2 |
| 생성된 세션의 pod 이름 = 실 Pod 오브젝트(라벨 `session-id` 1:1), N건 → 고유 Pod N개 | go API (`TestDeferred_RealPodProvisioned`) | A1, A2 |
| 세션 동결(snapshot) 시 대상 Pod 오브젝트 삭제 + 자원 회수(API `pod:""` + 클러스터 그라운드-트루스로 Pod 삭제/terminating 확인) | go API (`TestDeferred_RealPodReclaimed`, architecture 시나리오 3) | A3 |
| 세션 pod 안에 PTY에 연결된 쉘 프로세스 정확히 1개(`bash`) | go API (`TestShell_ExactlyOnePTYShellInSessionPod`, shell-workload 시나리오 1) | D1 |
| control-plane pod에는 쉘 없음(distroless — 쉘 exec 자체가 실패) | go API (`TestShell_ControlPlaneRunsNoShell`, shell-workload 시나리오 1) | D1 |
| 목록 포함 / 단건 조회 일치 | go API | V5 |
| active 세션 switch = no-op | go API | C4 |
| active read(`path:"active"` + 쉘 프롬프트 payload 수신 대기 + `nextOffset` 커서) / write(`path:"active"`) | go API | C2, C3 |
| write→쉘 실행→read로 출력 회수, write는 명령 완료 미대기(3초 명령에 비블로킹) | go API (`TestShell_WriteThenReadRecoversOutput`, shell-workload 시나리오 2) | D2, D3 |
| 커서 read는 델타만, `offset=0` 재조회는 전체 이력 순서 보존(비파괴) | go API (`TestShell_ReadCursorDeltaAndFullReplay`, shell-workload 시나리오 3) | D3 |
| 없는 id → 404 | go API | (에러 매핑) |
| 동시 접근(get/read/write/switch 24-way)에서 단일 active 상태로 수렴·중복 pod 없음(2 replica 공유 store) | go API (`TestDeferred_CrossReplicaAtomicity`) | C1 |
| CRIU 왕복: 동결(snapshot) 세션 접근 시 새 pod로 복원 + 쉘 상태(env·cwd)·read 커서 보존(마커 왕복, 프리즈 전/후 이력 순서 유지) | go API (`TestDeferred_CRIUIntegrity`, lifecycle 시나리오 3, CRIU-on 오버레이) | B2, B3, D4 |
| J1: 생성 → `/session/:id` → 쉘 명령 실행($((…)) 마커) → switch | playwright | A1/A2, C2/C3, D2/D3 |
| J3: 다건 목록 노출 → 카드 클릭/전환 | playwright | C4, V4 |
| J5: 콘솔 명령 입력→출력 누적 표시, 재진입 시 `offset=0`으로 전체 이력 복원 | playwright | D2, D3, V3 |
| J6: no-store config의 non-empty soft catalog picker와 empty catalog free-text fallback, workload/model 요청, agent route/card, exact prompt payload, UTF-8 경계 SSE 자동 append, byte cursor 재접속/read reconcile, stale cursor reset 전체 replay, snapshot 오류 후 Restore 화면 | playwright route fixture (`j6-agent-prompt-loop.spec.ts`) | E1~E6 UI/API 계약 |
| 세션 삭제: 목록 확인·키보드 포커스, live Workspace 삭제, snapshot Restore의 409 오류 후 재시도 | playwright + route fixture (`session-deletion.spec.ts`) | A3, lifecycle/API 오류 계약 |
| 워크스페이스 수동 아카이브: 실 세션을 Archive 버튼으로 동결 → 진행 중 버튼 상태(`Freezing…`·disabled·`aria-busy`) → 목록 복귀·토스트 → 카드 `data-state="snapshot"` + API 그라운드-트루스(`state=snapshot`, `pod` 비움) | playwright (`manual-archive.spec.ts`, 인터셉트 없음) | A3 (+ 수동 동결 UI 계약) |

## Deferred 시드 ↔ 문서 시나리오 매핑

`go test -tags=e2e`와 `npx playwright test` 실행 시 아래 케이스는 **사유와 함께
"skipped"** 로 표시된다. 표의 구체 선결조건이 갖춰지면 skip을 제거하고 본문을 채운다.

| 시드 (테스트) | 스위트 | 문서 시나리오 / 여정 | AC | 막힌 이유 (선결조건) |
| --- | --- | --- | --- | --- |
| ~~`TestDeferred_RealPodProvisioned`~~ → **채움** | go | architecture 시나리오 1·2 | A1, A2 | (해소: 실 client-go PodOrchestrator 적용 — 위 커버 표로 이동) |
| ~~`TestDeferred_RealPodReclaimed`~~ → **채움** | go | architecture 시나리오 3 | A3 | (해소: 실 client-go PodOrchestrator의 Stop이 Pod를 삭제 + 제품 snapshot endpoint로 동결 경로 도달 — 위 커버 표로 이동) |
| `TestDeferred_IdleToSnapshot` | go | lifecycle 시나리오 1 | B1 | reaper는 구현됨; 배포 SUT의 60분 경계를 가속할 clock/lastAccess test seam 필요 |
| `TestDeferred_SnapshotRestore` | go | lifecycle 시나리오 2 | B2 | B2는 `TestDeferred_CRIUIntegrity`(복원→새 pod)로 이미 커버 — 이 시드는 focused restore-only 잉여 placeholder(비차단) |
| ~~`TestDeferred_CRIUIntegrity`~~ → **채움** | go | lifecycle 시나리오 3 | B2, B3, D4 | (해소: deploy/ 오버레이가 CRIU 게이트 ON(agent-driven in-pod CRIU + MinIO) + 제품 snapshot endpoint → snapshot→복원→상태 보존 왕복 실단언; e2e CRIU 프로브가 러너 커널 지원 확인 — 위 커버 표로 이동) |
| `TestDeferred_ReadIdleAndSnapshotBranches` | go | state-api 시나리오 2 | C2 | idle 분기만 잔여(snapshot 분기는 `TestDeferred_CRIUIntegrity`가 커버) — operational `idle` 상태 producer 필요 |
| `TestDeferred_WriteIdleAndSnapshotBranches` | go | state-api 시나리오 3 | C3 | idle 분기만 잔여(snapshot 분기는 `TestDeferred_CRIUIntegrity`가 커버) — operational `idle` 상태 producer 필요 |
| ~~`TestDeferred_CrossReplicaAtomicity`~~ → **채움** | go | state-api 시나리오 1 | C1 | (해소: ConfigMap/Lease StateStore + 2-replica 오버레이로 교차-replica 일관성 단언 — 위 커버 표로 이동. 단일-승자 CAS/Lease는 envtest 스위트가 실 apiserver로 검증) |
| `J2: session freezes to a snapshot after idle` | playwright | J2 | B1 | reaper 60분 경계를 가속할 배포 test seam 필요 |
| `J2: thaw & resume restores a snapshot session` | playwright | J2 | B2 | deployed snapshot fixture 필요(J6 archive Restore 화면은 route fixture로 별도 검증) |
| `J4: concurrent access stays consistent` | playwright | J4 | C1 | UI 비대상(백엔드 동시성) — Go e2e(`TestDeferred_CrossReplicaAtomicity`) + envtest로 검증 |
| `archives a claude-code session from the workspace` (`manual-archive.spec.ts`) | playwright | J6 / claude-code 수동 아카이브 | E2, A3 | kind SUT가 claude-code 세션을 **띄울 수 없다**: ① `data-plane/entrypoint.sh`의 K3s MCP + 비공개 marketplace bootstrap이 실자격 없이는 실패해 pod가 Ready에 못 간다 ② `claude-code-credentials`의 `base-url`이 의도적으로 unroutable. 「미해소 위반」 절의 선결조건 ①·② 그대로다. shell 문구 분기는 같은 파일의 실 SUT 테스트가 커버한다 |
