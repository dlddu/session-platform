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
> 상태 보존 왕복이 실 단언으로 검증**된다(`TestDeferred_CRIUIntegrity`, AC-B2/B3/D4; e2e
> 워크플로의 CRIU 프로브가 러너 커널의 in-pod criu 지원을 확인). 운영 reaper는 마지막
> read/write부터 60분에 도달한 세션을 스캔해 snapshot한다. 다만 배포 e2e에는 60분 시계를
> 가속하거나 `lastAccess`를 주입하는 제품 API가 없어 실제 시간 경계 시드는 skip이다.
> 검증 범위는 **생성/목록/조회/switch·read·write 해피패스 + 실 Pod 단언(AC-A1/A2) + PTY 쉘
> 런타임 단언(AC-D1) + 쉘 stdin/stdout 시맨틱(AC-D2/D3: write→쉘 stdin 주입, read→offset
> 커서 델타·offset=0 전체 재조회) + 교차-replica 일관성(AC-C1) + CRIU 왕복(AC-B2/B3/D4)**
> 이다. reaper 시간 경계(B1)와 중간 `idle` 상태의 read/write 분기(C2/C3)는 각각
> 제어 가능한 시계와 operational idle-state producer가 생기면 배포 e2e로 채운다.

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

## Deferred 시드 ↔ 문서 시나리오 매핑

`go test -tags=e2e`와 `npx playwright test` 실행 시 아래 케이스는 **사유와 함께
"skipped"** 로 표시된다. 표의 구체 선결조건이 갖춰지면 skip을 제거하고 본문을 채운다.

| 시드 (테스트) | 스위트 | 문서 시나리오 / 여정 | AC | 막힌 이유 (선결조건) |
| --- | --- | --- | --- | --- |
| ~~`TestDeferred_RealPodProvisioned`~~ → **채움** | go | architecture 시나리오 1·2 | A1, A2 | (해소: 실 client-go PodOrchestrator 적용 — 위 커버 표로 이동) |
| ~~`TestDeferred_RealPodReclaimed`~~ → **채움** | go | architecture 시나리오 3 | A3 | (해소: 실 client-go PodOrchestrator의 Stop이 Pod를 삭제 + test-only snapshot 트리거로 동결 경로 도달 — 위 커버 표로 이동) |
| `TestDeferred_IdleToSnapshot` | go | lifecycle 시나리오 1 | B1 | reaper는 구현됨; 배포 SUT의 60분 경계를 가속할 clock/lastAccess test seam 필요 |
| `TestDeferred_SnapshotRestore` | go | lifecycle 시나리오 2 | B2 | B2는 `TestDeferred_CRIUIntegrity`(복원→새 pod)로 이미 커버 — 이 시드는 focused restore-only 잉여 placeholder(비차단) |
| ~~`TestDeferred_CRIUIntegrity`~~ → **채움** | go | lifecycle 시나리오 3 | B2, B3, D4 | (해소: deploy/ 오버레이가 CRIU 게이트 ON(agent-driven in-pod CRIU + MinIO) + test-only snapshot 트리거 → snapshot→복원→상태 보존 왕복 실단언; e2e CRIU 프로브가 러너 커널 지원 확인 — 위 커버 표로 이동) |
| `TestDeferred_ReadIdleAndSnapshotBranches` | go | state-api 시나리오 2 | C2 | idle 분기만 잔여(snapshot 분기는 `TestDeferred_CRIUIntegrity`가 커버) — operational `idle` 상태 producer 필요 |
| `TestDeferred_WriteIdleAndSnapshotBranches` | go | state-api 시나리오 3 | C3 | idle 분기만 잔여(snapshot 분기는 `TestDeferred_CRIUIntegrity`가 커버) — operational `idle` 상태 producer 필요 |
| ~~`TestDeferred_CrossReplicaAtomicity`~~ → **채움** | go | state-api 시나리오 1 | C1 | (해소: ConfigMap/Lease StateStore + 2-replica 오버레이로 교차-replica 일관성 단언 — 위 커버 표로 이동. 단일-승자 CAS/Lease는 envtest 스위트가 실 apiserver로 검증) |
| `J2: session freezes to a snapshot after idle` | playwright | J2 | B1 | reaper 60분 경계를 가속할 배포 test seam 필요 |
| `J2: thaw & resume restores a snapshot session` | playwright | J2 | B2 | deployed snapshot fixture 필요(J6 archive Restore 화면은 route fixture로 별도 검증) |
| `J4: concurrent access stays consistent` | playwright | J4 | C1 | UI 비대상(백엔드 동시성) — Go e2e(`TestDeferred_CrossReplicaAtomicity`) + envtest로 검증 |
