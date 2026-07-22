# CRIU 체크포인트/복원 — 결정 확정 + 게이트 on 실검증 인계

> 상태: **결정 확정 · 실 코드 준비됨(미검증)** — *2026-07-04 (J5-S4)*.
> 이전까지 자리표시자였던 5개 열린 결정을 구체 선택으로 확정하고, `Checkpointer`
> 포트 뒤에 K8s-native CRIU 어댑터(`criu.ContainerCheckpointer`)를 실구현했다.
> 게이트(`CRIU_ENABLED`) off는 여전히 no-op 스텁으로 통과한다. 남은 것은
> **CRIU 지원 런타임에서의 게이트 on 실검증** 하나이며, 이는 런타임/클러스터/저장소
> **프로비저닝(별도 작업)**으로 인계한다(맨 아래 인계 섹션).

## 왜 별도 항목이었나 (배경)
- K8s `ContainerCheckpoint`(kubelet) API는 **alpha**이며 클러스터/런타임 설정이 필요하다.
- "체크포인트를 **새 pod로 복원**"(AC-B2)은 더 미성숙하여, 표준 경로가 확립되어 있지 않다.
- 따라서 happy-path(생성/목록/전환)는 CRIU 없이 동작하도록 설계했고(스텁), CRIU는
  실 코드를 작성하되 검증은 런타임이 준비된 뒤로 미룬다.

## 확정된 결정 (5)

### ① 검증 클러스터 — CRIU 지원 노드 필요 (kind 단독 불가)
- **선택**: containerd + CRIU 지원 커널을 갖춘 노드가 있는 클러스터. 순수 kind 기본 노드로는
  불가(런타임/커널 CRIU 미탑재).
- **근거**: `ContainerCheckpoint`는 kubelet + 컨테이너 런타임 + 커널의 CRIU 지원이 모두 있어야 동작한다.
- **후속(프로비저닝)**: CRIU 커널 옵션을 켠 노드 이미지 준비. base 이미지는 이미 CRIU 친화적
  (ubuntu:24.04 + glibc, `data-plane/Dockerfile`).

### ② 컨테이너 런타임 — containerd + runc(CRIU 빌드) + kubelet 게이트
- **선택**: containerd + CRIU 지원 runc 빌드. kubelet feature gate `ContainerCheckpoint`(체크포인트)와,
  복원까지 검증하려면 `ContainerCheckpointRestore` 계열 지원을 활성.
- **근거**: 체크포인트 생성은 kubelet `ContainerCheckpoint`가 담당하고, 실제 freeze/thaw는 runc→CRIU가 수행한다.
- **후속(프로비저닝)**: 노드 kubelet 플래그 + runc 빌드 확인.

### ③ 체크포인트 저장소 — S3 (assume-role), 노드 로컬은 중간 산물
- **선택** *(2026-07-04 개정)*: 내구 저장소는 **S3**. kubelet이 노드 로컬 tar
  (`/var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<container>-<ts>.tar`)로 아카이브를 생성하면
  체크포인터가 이를 **S3로 업로드**하고 `session.Checkpoint.Ref`에
  `s3://<bucket>/<prefix>/<ns>/<pod>/<file>`를 기록한다(업로드 크기를 `SizeBytes`에 반영).
  노드 로컬 경로는 중간 산물이며, 버킷 미설정 시 노드 로컬 ref로 폴백한다.
- **권한(코드)**: 정적 키를 두지 않는다. **노드 인스턴스 프로파일(또는 IRSA)** 을 베이스 자격증명으로,
  코드가 **STS AssumeRole**(`CHECKPOINT_S3_ROLE_ARN`)로 대상 역할을 위임받아 S3에 접근한다
  (`internal/adapter/checkpointstore` — aws-sdk-go-v2 `stscreds.NewAssumeRoleProvider` +
  `aws.NewCredentialsCache`). 자격증명은 최초 요청 시 지연 해석된다.
- **근거**: 노드 로컬 아카이브는 노드가 회수되면 사라지고 다른 노드로 복원할 때 접근 불가. S3는
  클러스터 전역에서 접근 가능하여 복원 pod가 어느 노드로 스케줄되어도 아카이브를 가져올 수 있다.
- **설정(env)**: `CHECKPOINT_S3_BUCKET`(설정 시 S3 활성), `CHECKPOINT_S3_ROLE_ARN`,
  `CHECKPOINT_S3_REGION`(없으면 `AWS_REGION`), `CHECKPOINT_S3_PREFIX`(기본 `checkpoints`),
  `CHECKPOINT_S3_SESSION_NAME`. 버킷은 설정됐는데 역할/리전이 없으면 기동 시 fail-fast.
- **경계/후속**: control plane(또는 업로더 사이드카)이 노드의 체크포인트 디렉터리를 읽을 수 있어야 한다
  (hostPath 마운트 또는 노드 사이드카) — 배포 결정이며, 코드는 아카이브 열기를 `archiveOpener` 심 뒤로
  격리했다. 대용량 아카이브 멀티파트 스트리밍(`manager.Uploader`)·업로드 후 노드 로컬 tar 정리·수명주기
  (만료) 정책은 후속.

### ④ 무결성 검증법 — AC-D4 마커 왕복 (in-process Scenario4)
- **선택**: 동결 전 인메모리 마커(환경 변수 `MARKER`·작업 디렉터리)를 세팅 → 스냅샷 → 복원 →
  `echo $MARKER`·`pwd`로 왕복 확인. 여기에 **커서 연속성**(복원 전 발급 커서로 델타 read) 검증을 더한다.
- **구현**: `control-plane/test/integration_test.go`의 `TestScenario4_CRIUIntegrity` — 실 클러스터 백엔드
  `Service`에 대해 in-process로 `Service.Snapshot`을 직접 호출(HTTP 스냅샷 엔드포인트 미추가).
- **근거**: AC-D4가 AC-B3(무결성)의 구체 마커. in-process가 브라우저 e2e보다 결정적·저비용.

### ⑤ 새 pod 복원 — 아카이브→복원, 복원용 pod=restore-target 스펙
- **선택**: 복원용 pod는 `Start`와 달리 **엔트리포인트로 새 쉘을 띄우지 않는다**.
  `k8s.ClientOrchestrator.RestoreInto`가 pod에 `AnnotationRestoreCheckpoint`(체크포인트 아카이브 ref)를
  달아 **restore target**으로 만들고, CRIU 지원 런타임이 이 어노테이션을 자신의 복원 메커니즘
  (예: CRI-O `io.kubernetes.cri-o.restore`, 또는 아카이브로 빌드한 체크포인트 OCI 이미지)에 매핑하여
  **동결된 프로세스 트리를 재개**한다. pod의 컨테이너/포트/readiness는 `Start`와 동일하므로, 재개 후
  agent `/healthz`는 **복원된 쉘**의 생존을 반영하고 pod-Ready는 AC-D1 의미를 유지한다.
- **대안(각주)**: `ContainerCheckpointRestore` 표준 경로가 미성숙하면 **runc restore + pod 재부착**을
  대안으로 둔다. 이 경우 실제 아카이브 적용은 `criu.CheckpointDriver.Restore` 심(seam)에서 수행한다.
- **근거**: `RestoreInto == Start`면 빈 쉘이 떠 상태가 소실된다 → 스펙 분기가 필수.

## 게이트 동작 (현재 스캐폴딩)
- 환경변수 `CRIU_ENABLED`(기본 `false`).
- **off**: `criu.StubCheckpointer`가 합성 메타데이터로 no-op 성공 → 스냅샷/복원 플로우가
  엔드투엔드로 도는 골격을 유지(런타임 불필요).
- **on**: `criu.ContainerCheckpointer`(실 어댑터)가 kubelet `ContainerCheckpoint` API를 구동한다.
  `main.go`가 게이트로 실 어댑터/스텁을 주입 분기한다. `TestScenario4_CRIUIntegrity`는 게이트 on +
  CRIU 지원 런타임에서 **마커 왕복을 실검증**하며, 런타임 부재 시 skip(런타임 없는 CI는 정상 skip).
  런타임은 있으나 복원 경로가 아직 매핑되지 않았다면 **의도적으로 실패**하여 "복원 메커니즘 매듭 필요"를
  가시화한다(=프로비저닝의 완료 신호).

## 실구현 요약 (코드)
- `control-plane/internal/adapter/criu/checkpointer.go` — `Checkpointer` 포트 + 게이트 off 스텁.
- `control-plane/internal/adapter/criu/container_checkpointer.go` — **실 어댑터**.
  `ContainerCheckpointer.Checkpoint`는 pod의 노드를 조회한 뒤 `CheckpointDriver`로 kubelet
  `POST /api/v1/nodes/{node}/proxy/checkpoint/{ns}/{pod}/{container}`를 호출해 아카이브 경로를 얻고,
  `CheckpointStore`가 설정돼 있으면 아카이브를 업로드해 `Ref`에 `s3://…`(+`SizeBytes`)를 기록한다
  (미설정 시 노드 로컬 경로). `Restore`는 그 ref를 드라이버에 넘긴다. 런타임 호출은 `CheckpointDriver`
  (실체 `kubeletDriver`), 아카이브 열기는 `archiveOpener` 심 뒤로 격리되어 런타임/AWS 없이 컴파일·
  단위 테스트가 통과한다(가짜 드라이버·스토어 주입).
- `control-plane/internal/adapter/checkpointstore/store.go` — **S3 스토어**(`NewS3`). 노드 인스턴스
  프로파일을 베이스로 STS AssumeRole(`CHECKPOINT_S3_ROLE_ARN`)로 위임받아 `Put`(업로드)/`Get`(복원 시
  가져오기)을 수행. S3 API를 `objectAPI` 인터페이스 뒤로 격리해 가짜 API로 단위 테스트.
- `control-plane/internal/adapter/k8s/client_orchestrator.go` — `RestoreInto`가 restore-target pod 스펙
  (`AnnotationRestoreCheckpoint`)을 생성. `Start`(신규 세션)는 어노테이션 없이 불변.
- `control-plane/internal/service/manager.go` — `Snapshot`/`Restore` 오케스트레이션. `Restore`가
  `RestoreInto`에 체크포인트 ref를 전달하고 커서를 리셋하지 않음(버퍼-인-체크포인트).
- `data-plane/cmd/agent/main.go` — scrollback이 에이전트 메모리 상주 → 체크포인트에 포함(AC-D4),
  복원 후 커서 유효.
- `control-plane/test/integration_test.go` — `TestScenario4_CRIUIntegrity`(마커 왕복 + 커서 연속성).
- `control-plane/test/e2e_deferred_test.go` — `TestDeferred_CRIUIntegrity`(B3/D4, deferred 시드).

## 게이트 on 실검증 인계 (프로비저닝 작업 → "확인 필요")

코드는 준비됐다. 프로비저닝 작업은 아래를 세우고 확인 명령을 green으로 만들면 된다.

**전제 체크리스트**
- [ ] CRIU 지원 노드: 커널 CRIU 옵션 + CRIU 지원 runc 빌드 (결정 ①②).
- [ ] kubelet feature gate `ContainerCheckpoint`(+ 복원 검증 시 `ContainerCheckpointRestore` 계열) 활성.
- [x] control plane ServiceAccount RBAC: `nodes/proxy`에 `create`(프록시 경유 POST) — `k8s/rbac.yaml`의
      ClusterRole/ClusterRoleBinding(`session-platform-node-checkpoint`)으로 **포함됨**(Flux 자동 적용).
      확인만 필요: ClusterRoleBinding subject의 namespace(`session-platform`)가 실제 배포 네임스페이스와 일치하는지.
- [ ] `DATA_PLANE_IMAGE` 주입(퍼블리시된 data plane 에이전트 이미지) + `CRIU_ENABLED=1`.
- [ ] 복원 경로 런타임 매핑: `k8s.AnnotationRestoreCheckpoint`(pod 어노테이션)를 런타임 복원 메커니즘
      (CRI-O `io.kubernetes.cri-o.restore` / 아카이브 기반 체크포인트 OCI 이미지)에 연결. 미성숙 시 대안 ⑤ 각주.
- [ ] S3 저장소(결정 ③): 버킷 생성 + `checkpoint-s3` Secret 프로비저닝(`bucket`/`role-arn`/`region`/`prefix` 4개 키).
      control-plane Deployment가 이 Secret을 `secretKeyRef`로 읽으며 **필수**다 — Secret이 없으면 pod가
      기동하지 않는다(CRIU off라도). 프로덕션은 external-secrets가, kind e2e는 overlay 플레이스홀더가 제공,
      검증은 `kubectl create secret generic checkpoint-s3 …`(예시: `k8s/checkpoint-s3-secret.example.yaml`).
- [ ] IAM: 노드 인스턴스 프로파일(또는 IRSA)이 `CHECKPOINT_S3_ROLE_ARN` 역할을 `sts:AssumeRole` 할 수 있고,
      그 역할이 버킷에 `s3:PutObject`(복원 시 `s3:GetObject`) 권한을 가질 것.
- [ ] 업로더의 아카이브 접근성: control plane(또는 사이드카)이 노드 체크포인트 디렉터리를 읽도록
      hostPath 마운트 또는 노드 사이드카(결정 ③ 경계). 복원 pod가 다른 노드로 스케줄되어도 S3에서 가져오므로 무방.

**확인 명령(green이면 AC-D4 + 커서 연속성 검증 완료)**
```
CRIU_ENABLED=1 DATA_PLANE_IMAGE=<published-agent-image> \
  go test -tags=integration ./test/... -run TestScenario4_CRIUIntegrity -v
```
- **skip**: 런타임/클러스터/이미지 미준비(정상 — 아직 세울 게 남음).
- **fail**: 런타임은 있으나 복원 경로 미매듭(위 매핑 항목 완료 필요).
- **pass**: 동결 전 `MARKER`·cwd가 복원 후 그대로 재개 + 복원 전 커서로 델타 read 유효 → 완료.
> in-process Scenario4는 노드 로컬 아카이브로 상태 무결성(AC-D4)만 검증하므로 S3 env가 필요 없다
> (아카이브가 노드에 있고 테스트 프로세스에서 열 수 없음). **S3 업로드 경로**는 (1) 단위 테스트
> (`checkpointstore`/`criu`)와 (2) `checkpoint-s3` Secret이 주입된 **배포된 control-plane**에서 검증된다.

## 리스크 / 대안
- **"새 pod 복원" alpha/미성숙**: 코드를 `Checkpointer` 포트 + `CheckpointDriver` 심 뒤로 격리했으므로
  메커니즘 교체(runc restore 대안, 결정 ⑤ 각주)가 국소적이다.
- **미검증 코드 재작업 위험**: 결정 ①~⑤로 API/저장/복원 타겟을 고정하고 인터페이스 경계를 좁혔다.
- **실행 중 포그라운드 프로세스/FD 캡처 온전성**: AC-D4 마커는 우선 env·cwd 위주. 실행 중 프로세스
  케이스는 트리거 정책 확정과 함께 후속 시나리오로(범위 밖, `doc-tracker.md`).

## 관련 문서
- `docs/prd/shell-workload.md` — AC-D4(보존 상태), offset·복원 설계 노트(커서 유효).
- `docs/test/shell-workload.md` 시나리오 4 / `docs/test/lifecycle.md` 시나리오 3 — 마커 왕복.
- `deploy/kind-config.yaml` — kind는 CRIU 미활성(게이트 off) 명시.
