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

### ⑤ 복원 메커니즘 — 에이전트 주도 in-pod CRIU (2026-07-22 확정)
- **선택**: 복원용 pod는 `Start`와 달리 엔트리포인트로 새 쉘을 띄우지 않는다.
  `RestoreInto`가 pod에 `AnnotationRestoreCheckpoint` + `DATA_PLANE_RESTORE_MODE=1`을 달아 restore-target으로
  만들고, 그 pod의 **에이전트가 셸 없이 기동(healthz 200 = 복원 대기)해 `/restore`로 아카이브를 받아
  criu restore로 셸 프로세스 트리를 부활**시킨다. 체크포인트도 대칭으로 에이전트가 `/checkpoint`에서
  criu dump→tar 아카이브(criu 이미지 + scrollback)를 만든다. 컨트롤플레인은 아카이브를 **에이전트↔S3로
  중계**할 뿐이라 AWS 자격증명을 세션 pod에 퍼뜨리지 않는다. 재개 후 agent `/healthz`는 복원된 쉘을
  반영하고, 서비스 계층의 복원-후 Reach가 AC-D1 불변식을 유지한다.
- **근거(2026-07-22 검증)**: kubelet ContainerCheckpoint의 dump는 성공하나 containerd가 그 아카이브를
  **복원할 방법이 없음**(CRI-O식 어노테이션/체크포인트-이미지 복원 미지원)이 실증됨. 에이전트 주도는
  런타임 교체 없이 이 레포 안에서 라운드트립을 완결한다.
- **대안(유지, 미배선)**: CRI-O 도입 시 kubelet checkpoint(`container_checkpointer.go`) + 체크포인트 OCI
  이미지 경로. 그 경로만 `nodes/proxy` RBAC가 필요하다(에이전트 주도는 pod securityContext capability만 필요).
- **런타임 seam(미검증)**: 실제 criu dump/restore + PTY 재부착은 `data-plane`의 `execCriuEngine` 하나에
  격리 — CRIU-capable 노드에서만 검증. 나머지(아카이브 framing·복원모드·상태 스왑·오케스트레이션)는
  가짜 엔진/클라이언트로 유닛 테스트됨.

## 게이트 동작 (현재 스캐폴딩)
- 환경변수 `CRIU_ENABLED`(기본 `false`).
- **off**: `criu.StubCheckpointer`가 합성 메타데이터로 no-op 성공 → 스냅샷/복원 플로우가
  엔드투엔드로 도는 골격을 유지(런타임 불필요).
- **on**: `criu.AgentCheckpointer`(에이전트 주도 in-pod CRIU)가 pod 에이전트의 `/checkpoint`·`/restore`를
  구동하고 아카이브를 S3로 중계한다. `main.go`가 게이트로 실 어댑터/스텁을 분기하고 세션 pod에 criu
  capability를 붙인다. `TestScenario4_CRIUIntegrity`는 게이트 on + CRIU 지원 런타임에서 **마커 왕복을
  실검증**하며, 런타임 부재 시 skip(런타임 없는 CI는 정상 skip). 런타임은 있으나 `execCriuEngine`(실제
  criu 호출)가 아직 안 맞으면 **실패**로 신호한다(=런타임 반복 조정 지점).

## 실구현 요약 (코드)
- `control-plane/internal/adapter/criu/checkpointer.go` — `Checkpointer` 포트 + 게이트 off 스텁.
- `control-plane/internal/adapter/criu/agent_checkpointer.go` — **실 어댑터(wired)**.
  `AgentCheckpointer.Checkpoint`는 pod 에이전트 `/checkpoint`에서 아카이브 스트림을 받아 `CheckpointStore`로
  업로드하고 `Ref`에 durable ref(+스트림 크기 `SizeBytes`)를 기록, `Restore`는 store에서 받아 restore-target
  pod 에이전트 `/restore`로 스트리밍한다. 에이전트 채널은 `AgentCheckpointClient` 인터페이스 뒤로 격리 —
  런타임/에이전트 없이 가짜 클라이언트로 유닛 테스트.
- `control-plane/internal/adapter/criu/container_checkpointer.go` — **CRI-O 대안(미배선)**. kubelet
  ContainerCheckpoint 경로. 유지·테스트되지만 wired가 아님(위 ⑤ 근거).
- `control-plane/internal/adapter/agent/client.go` — `HTTPClient`에 `/checkpoint`·`/restore` 스트림 메서드
  추가(셸 I/O와 같은 pod-IP 해석 재사용, 아카이브 전송용 별도 client).
- `control-plane/internal/adapter/checkpointstore/store.go` — **S3 스토어**(`NewS3`). 노드 인스턴스
  프로파일을 베이스로 STS AssumeRole(`CHECKPOINT_S3_ROLE_ARN`)로 위임받아 `Put`/`Get`. 가짜 API로 단위 테스트.
- `control-plane/internal/adapter/k8s/client_orchestrator.go` — `RestoreInto`가 restore-target pod를 **고유
  이름**(`sess-<id>-r<suffix>`) + `DATA_PLANE_RESTORE_MODE=1` env로 생성. `WithCheckpointCapabilities`(게이트드)로
  세션 pod에 criu capability(`CHECKPOINT_RESTORE`,`SYS_PTRACE`) 부여. `Start`(신규 세션)는 불변.
- `control-plane/internal/service/manager.go` — `Snapshot`/`Restore` 오케스트레이션. `Restore`가
  `RestoreInto`에 체크포인트 ref를 전달하고 커서를 리셋하지 않음(버퍼-인-체크포인트).
- `data-plane/cmd/agent/main.go` + `checkpoint.go` — 스왑 가능한 셸 홀더 + 복원모드 기동, `/checkpoint`(criu
  dump→tar) / `/restore`(tar→criu restore→셸 부활) 핸들러, scrollback 직렬화. 실제 criu 호출은
  `execCriuEngine` seam(미검증); 나머지는 가짜 엔진으로 유닛 테스트. scrollback은 에이전트 메모리 상주라
  아카이브에 함께 직렬화돼 복원 후 커서 유효(AC-D4).
- `data-plane/Dockerfile` — `ENV GODEBUG=multipathtcp=0`: Go 1.24가 Linux 리스너에 MPTCP를 기본
  활성화하는데 CRIU는 MPTCP 소켓을 체크포인트하지 못하므로, 에이전트 :8090 리스너(및 세션 쉘이
  상속하는 환경)를 plain TCP로 고정.
- `control-plane/test/integration_test.go` — `TestScenario4_CRIUIntegrity`(마커 왕복 + 커서 연속성).
- `control-plane/test/e2e_deferred_test.go` — `TestDeferred_CRIUIntegrity`(B3/D4, deferred 시드).

## 실검증 현황 (2026-07-22, k3s)

`TestScenario4_CRIUIntegrity`를 실 k3s 클러스터(containerd, arm64)에서 실행한 결과
(테스트 바이너리를 클러스터 내 pod에서 in-cluster config로 실행 — pod 네트워크 요구 확인됨):

- ✅ **체크포인트 측 green**: 세션 생성 → pod 프로비저닝 → attach(8090) → 쉘 I/O·pre-freeze 커서 →
  kubelet `ContainerCheckpoint` 호출(RBAC 포함) → 아카이브 생성까지 성공.
- ❌ **복원 측 미배선(설계된 실패)**: 복원 pod가 `AnnotationRestoreCheckpoint`를 이어받을 런타임
  컴포넌트가 없어 새 쉘(`/# `)로 기동 → 마커 미복원으로 fail. k3s의 containerd는 CRI-O식
  어노테이션/체크포인트-이미지 복원을 지원하지 않음이 실증됨.
- 🔁 **대응(설계 전환)**: 이 검증 결과로 복원 메커니즘을 **에이전트 주도 in-pod CRIU**로 전환(결정 ⑤
  확정). 이제 kubelet 경로 대신 pod 에이전트가 직접 criu dump/restore하고 컨트롤플레인이 아카이브를
  S3로 중계한다. 남은 런타임 미검증 지점은 `execCriuEngine`(criu 호출 + PTY 재부착) 하나로 좁혀짐.
- 🔧 **레이스 발견→수정됨**: 복원 pod가 동결 pod의 결정적 이름(`sess-<id>`)을 재사용해 Terminating
  잔재와 AlreadyExists 레이스 가능 → 복원 pod를 고유 이름(`sess-<id>-r<suffix>`)으로 분리(2026-07-22).

## 게이트 on 실검증 인계 (프로비저닝 작업 → "확인 필요")

코드는 준비됐다. 프로비저닝 작업은 아래를 세우고 확인 명령을 green으로 만들면 된다.

**전제 체크리스트 (에이전트 주도 in-pod CRIU 기준)**
- [ ] criu 바이너리를 데이터플레인 이미지에 제공 — **Ubuntu 24.04(noble)는 criu를 패키징하지 않음**
      (`apt-get install criu` = "no installation candidate", CI에서 확인). 소스 빌드 / noble 지원 PPA /
      criu 번들 베이스 중 선택하고, PATH에 없으면 `CRIU_BIN` env로 경로를 지정한다. 런타임(containerd)
      자체 교체는 불필요 — 에이전트가 pod 안에서 criu를 직접 실행한다.
- [ ] 노드 커널 CRIU 옵션 활성(체크포인트/복원 syscall 지원) — 노드 측 확인.
- [x] 세션 pod의 criu capability(`CHECKPOINT_RESTORE`,`SYS_PTRACE`): `WithCheckpointCapabilities`가
      `CRIU_ENABLED=1`에서 자동 부여(`k8s/deployment.yaml` 수정 불필요). 커널/criu 버전에 따라 `SYS_ADMIN`
      추가가 필요할 수 있음(런타임 조정 지점).
- [x] `DATA_PLANE_IMAGE` + `CRIU_ENABLED=1` — *2026-07-22 검증 환경에서 수행됨(criu 미포함 이미지 → 위 항목 필요)*.
- [ ] **`execCriuEngine` 실검증 ← 유일하게 남은 런타임 지점**: 에이전트 `/checkpoint`(criu dump)·
      `/restore`(criu restore + PTY 재부착)의 실동작을 CRIU 노드에서 확인·조정. 이 seam 외 전 경로는
      가짜 엔진/클라이언트로 유닛 테스트됨.
- [ ] S3 저장소(결정 ③, **배포된 control-plane 실동작용**): 버킷 + `checkpoint-s3` Secret(`bucket`/`role-arn`/
      `region`/`prefix`) + IAM(노드 프로파일 → `sts:AssumeRole` → `s3:PutObject`·`GetObject`). Deployment가 이
      Secret을 `secretKeyRef`(필수)로 읽으므로 Secret 없으면 pod 미기동(CRIU off라도). in-process Scenario4는
      이 S3가 필요 없다(인메모리 스토어로 대체).
- [~] (CRI-O 대안 전용, 미사용 시 삭제 가능) `nodes/proxy` RBAC(`session-platform-node-checkpoint`): 에이전트
      주도 경로는 쓰지 않는다 — CRI-O 대안을 채택할 때만 필요.

**확인 명령(green이면 AC-D4 + 커서 연속성 검증 완료)**
```
CRIU_ENABLED=1 DATA_PLANE_IMAGE=<criu-포함 agent-image> \
  go test -tags=integration ./test/... -run TestScenario4_CRIUIntegrity -v
```
- **skip**: 런타임/클러스터/이미지 미준비(정상).
- **fail**: 런타임은 있으나 `execCriuEngine`(criu 호출/PTY 재부착)가 아직 안 맞음 → 조정 지점.
- **pass**: 동결 전 `MARKER`·cwd가 복원 후 그대로 재개 + 복원 전 커서로 델타 read 유효 → 완료.
> in-process Scenario4는 **인메모리 스토어**로 아카이브를 체크포인트↔복원 사이 중계하므로 S3 env가
> 필요 없다(에이전트가 아카이브를 HTTP로 테스트 프로세스에 스트리밍). **S3 업로드 경로**는 (1) 단위
> 테스트(`checkpointstore`/`criu`)와 (2) `checkpoint-s3` Secret이 주입된 **배포된 control-plane**에서 검증된다.

## 리스크 / 대안
- **in-pod criu 복원 + PTY 재부착 미검증**: 실제 criu 호출은 `data-plane`의 `execCriuEngine` 하나에
  격리했으므로, 복원 시 PTY 재부착/프로세스 reaping이 런타임에서 안 맞으면 그 파일만 조정하면 된다
  (나머지 경로·아카이브·복원모드는 유닛 테스트로 고정). 메커니즘 자체를 CRI-O로 되돌려도(대안)
  `Checkpointer` 포트 교체 하나로 국소적이다.
- **미검증 코드 재작업 위험**: 결정 ①~⑤로 API/저장/복원 타겟을 고정하고 인터페이스 경계를 좁혔다.
- **실행 중 포그라운드 프로세스/FD 캡처 온전성**: AC-D4 마커는 우선 env·cwd 위주. 실행 중 프로세스
  케이스는 트리거 정책 확정과 함께 후속 시나리오로(범위 밖, `doc-tracker.md`).

## 관련 문서
- `docs/prd/shell-workload.md` — AC-D4(보존 상태), offset·복원 설계 노트(커서 유효).
- `docs/test/shell-workload.md` 시나리오 4 / `docs/test/lifecycle.md` 시나리오 3 — 마커 왕복.
- `deploy/kind-config.yaml` — kind는 CRIU 미활성(게이트 off) 명시.
