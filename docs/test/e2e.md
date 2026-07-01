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
> 상태를 공유하므로 교차-replica 원자성(AC-C1)을 실제로 검증한다. Checkpointer(CRIU)만 아직
> 인메모리 스텁이고 idle→snapshot 트리거가 없으므로 생성된 세션은 여전히 `active`로 머문다.
> 검증 범위는 **생성/목록/조회/switch·read·write 해피패스 + 실 Pod 단언(AC-A1/A2) + PTY 쉘
> 런타임 단언(AC-D1) + 교차-replica 일관성(AC-C1)**이다. read/write의 쉘 stdin/stdout 시맨틱
> (AC-D2/D3)은 J5-S2/S3 범위, B-path(idle → snapshot → restore)와 CRIU 단언은 범위 밖이며,
> 단계 5의 **deferred 시드**(skip)로 골격만 남겨 둔다 — 해당 트리거/런타임이 들어오면 skip을
> 지우며 채운다.

## 빠른 실행 (로컬)

전제: Docker, [kind](https://kind.sigs.k8s.io), `kubectl`, Go 1.24+, Node 22+.

```bash
make e2e-up                          # kind 생성 + 이미지 빌드/load + deploy/ 적용 + 헬스 대기
cd control-plane && go test -tags=e2e ./test/...   # API e2e
cd web && npx playwright test        # 브라우저 e2e (J1, J3, smoke) — 최초 1회 `npx playwright install chromium`
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

`.github/workflows/e2e.yml`이 `control-plane/**`·`data-plane/**`·`web/**`·`deploy/**`·
`scripts/e2e/**`·`Makefile` 변경 PR과 `workflow_dispatch`에서만 돈다(무관 PR은 트리거되지
않음). 흐름:
kind 생성(`helm/kind-action`) → `make e2e-up` → `go test -tags=e2e` → Playwright. 실패 시
Playwright 리포트/trace를 아티팩트로 올린다. ci.yml의 lint/unit/build/integration 잡은 종전대로
모든 PR에서 돌고, **envtest 잡**이 실 kube-apiserver로 CAS/Lease 단일-승자(AC-C1)를 검증한다.

## 현재 커버되는 시나리오 (active 경로)

| 검증 | 스위트 | AC |
| --- | --- | --- |
| healthz 200 / `{"status":"ok"}` | go API | — |
| 생성 → `active` + 전용 pod, 3건 → 고유 pod 3개 | go API | A1, A2 |
| 생성된 세션의 pod 이름 = 실 Pod 오브젝트(라벨 `session-id` 1:1), N건 → 고유 Pod N개 | go API (`TestDeferred_RealPodProvisioned`) | A1, A2 |
| 세션 pod 안에 PTY에 연결된 쉘 프로세스 정확히 1개(`bash`) | go API (`TestShell_ExactlyOnePTYShellInSessionPod`, shell-workload 시나리오 1) | D1 |
| control-plane pod에는 쉘 없음(distroless — 쉘 exec 자체가 실패) | go API (`TestShell_ControlPlaneRunsNoShell`, shell-workload 시나리오 1) | D1 |
| 목록 포함 / 단건 조회 일치 | go API | V5 |
| active 세션 switch = no-op | go API | C4 |
| active read(`path:"active"`+payload) / write(`path:"active"`) | go API | C2, C3 |
| 없는 id → 404 | go API | (에러 매핑) |
| 동시 접근(get/read/write/switch 24-way)에서 단일 active 상태로 수렴·중복 pod 없음(2 replica 공유 store) | go API (`TestDeferred_CrossReplicaAtomicity`) | C1 |
| J1: 생성 → `/session/:id` → read/write/switch | playwright | A1/A2, C2/C3 |
| J3: 다건 목록 노출 → 카드 클릭/전환 | playwright | C4, V4 |

## Deferred 시드 ↔ 문서 시나리오 매핑

`go test -tags=e2e`와 `npx playwright test` 실행 시 아래 케이스는 **사유와 함께
"skipped"** 로 표시된다. 실 어댑터/트리거 PR이 해당 skip을 제거하며 본문을 채운다.

| 시드 (테스트) | 스위트 | 문서 시나리오 / 여정 | AC | 막힌 이유 (선결조건) |
| --- | --- | --- | --- | --- |
| ~~`TestDeferred_RealPodProvisioned`~~ → **채움** | go | architecture 시나리오 1·2 | A1, A2 | (해소: 실 client-go PodOrchestrator 적용 — 위 커버 표로 이동) |
| `TestDeferred_RealPodReclaimed` | go | architecture 시나리오 3 | A3 | terminate/snapshot 경로 + Pod 삭제·자원 회수 단언 |
| `TestDeferred_IdleToSnapshot` | go | lifecycle 시나리오 1 | B1 | idle→snapshot 트리거(reaper/엔드포인트) |
| `TestDeferred_SnapshotRestore` | go | lifecycle 시나리오 2 | B2 | snapshot 상태 세션 + 복원 |
| `TestDeferred_CRIUIntegrity` | go | lifecycle 시나리오 3 | B3 | 검증된 CRIU 런타임 (`docs/criu-verification.md`) |
| `TestDeferred_ReadIdleAndSnapshotBranches` | go | state-api 시나리오 2 | C2 | idle/snapshot 상태 |
| `TestDeferred_WriteIdleAndSnapshotBranches` | go | state-api 시나리오 3 | C3 | idle/snapshot 상태 |
| ~~`TestDeferred_CrossReplicaAtomicity`~~ → **채움** | go | state-api 시나리오 1 | C1 | (해소: ConfigMap/Lease StateStore + 2-replica 오버레이로 교차-replica 일관성 단언 — 위 커버 표로 이동. 단일-승자 CAS/Lease는 envtest 스위트가 실 apiserver로 검증) |
| `J2: session freezes to a snapshot after idle` | playwright | J2 | B1 | idle→snapshot 트리거 |
| `J2: thaw & resume restores a snapshot session` | playwright | J2 | B2 | snapshot 상태 세션 + 복원(Restore 화면) |
| `J4: concurrent access stays consistent` | playwright | J4 | C1 | UI 비대상(백엔드 동시성) — Go e2e(`TestDeferred_CrossReplicaAtomicity`) + envtest로 검증 |
