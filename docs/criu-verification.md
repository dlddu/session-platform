# CRIU 체크포인트/복원 — 현재 구현 및 검증

> 상태: **agent-driven in-pod CRIU 구현 + kind 배포 e2e 왕복 검증** — *2026-08-08 갱신*.
> `shell` snapshot은 pod 에이전트가 CRIU dump/restore를 실행하고 control plane이 archive를
> durable S3로 중계한다. `deploy/` overlay는 `CRIU_ENABLED=1`과 MinIO를 켜며 제품 snapshot
> endpoint를 호출하는 `TestDeferred_CRIUIntegrity`가 실제 dump→pod 회수→새 pod restore와 cursor
> 연속성을 단언한다. production base의 전략 게이트는 기본 off이고, 이때 snapshot 요청은 live pod를
> 보존한 채 실패한다. 남은 항목은 production S3/IAM 프로비저닝, 권한 최소화, 그리고 dump 성공 뒤
> Stop/final metadata 실패를 복구하는 shell 전용 transaction/reconcile 프로토콜이다.

## 왜 별도 항목이었나 (배경)
- 처음 검토한 K8s `ContainerCheckpoint`(kubelet) API는 alpha이고 containerd에는 해당 archive를
  새 pod로 복원하는 경로가 없어, 그 어댑터는 미배선 대안으로만 남았다.
- 현재 wired 경로는 런타임 checkpoint API 대신 pod 내부의 CRIU를 직접 실행한다. 따라서 필요한 것은
  CRIU를 포함한 이미지, 이를 허용하는 커널/보안 설정, durable archive store다.
- CRIU 게이트와 무관하게 생성/목록/전환과 `claude-code` workload는 동작한다.

## 확정된 결정 (5)

### ① 검증 클러스터 — kind + in-pod CRIU
- **선택**: CRIU syscall을 지원하는 커널 위의 kind와 privileged session pod. containerd/runc의
  checkpoint/restore 기능은 사용하지 않는다.
- **근거**: `data-plane` 이미지가 CRIU 4.2를 포함하고 agent가 직접 실행한다. GitHub-hosted runner의
  kind에서 `criu check`와 실제 snapshot/restore 왕복이 통과했다.
- **배포 경계**: production에서 privileged shell pod를 허용할지, 필요한 capability/AppArmor/procMount로
  줄일지는 별도 보안 결정이다.

### ② 컨테이너 런타임 — 별도 checkpoint 기능 불필요
- **선택**: 일반 containerd/runc 위에서 agent가 `/checkpoint`·`/restore` 요청을 받아 CRIU를 실행한다.
- **근거**: kubelet feature gate나 CRI-O checkpoint-image 복원에 의존하지 않아 kind와 production이 같은
  application code path를 사용한다.
- **대안**: `ContainerCheckpointer`는 CRI-O를 채택할 경우를 위한 미배선 코드이며 현재 manifest/RBAC에는
  연결되지 않는다.

### ③ 체크포인트 저장소 — durable S3
- **선택**: agent의 archive stream을 control plane이 **S3로 업로드**하고
  `session.Checkpoint.Ref`에 `s3://<bucket>/<prefix>/<pod>/.../checkpoint.tar`를 기록한다
  (`claude-code`는 CP-owned generation도 key에 포함). snapshot 게이트를 켰는데 S3 설정이 없으면
  control plane이 fail-fast하며 node-local fallback은 없다.
- **권한(코드)**: production은 **노드 인스턴스 프로파일(또는 IRSA)** 을 베이스 자격증명으로 쓰고,
  role ARN이 있으면 코드가 **STS AssumeRole**(`CHECKPOINT_S3_ROLE_ARN`)로 대상 역할을 위임받아 S3에 접근한다
  (`internal/adapter/checkpointstore` — aws-sdk-go-v2 `stscreds.NewAssumeRoleProvider` +
  `aws.NewCredentialsCache`). 자격증명은 최초 요청 시 지연 해석된다.
- **근거**: 노드 로컬 아카이브는 노드가 회수되면 사라지고 다른 노드로 복원할 때 접근 불가. S3는
  클러스터 전역에서 접근 가능하여 복원 pod가 어느 노드로 스케줄되어도 아카이브를 가져올 수 있다.
- **설정(env)**: `CHECKPOINT_S3_BUCKET`(설정 시 S3 활성), `CHECKPOINT_S3_ROLE_ARN`,
  `CHECKPOINT_S3_REGION`(없으면 `AWS_REGION`), `CHECKPOINT_S3_PREFIX`(기본 `checkpoints`),
  `CHECKPOINT_S3_SESSION_NAME`. 버킷은 설정됐는데 region이 없으면 기동 시 fail-fast하며 role은 optional이다
  (role 미지정은 ambient credential을 직접 사용; kind/MinIO는 test Secret의 static key 사용).
- **경계/후속**: 현재 S3 SDK 업로드는 unknown-length stream을 임시 파일에 spool한다. 대형 archive의
  multipart streaming과 object 수명주기/정리는 후속이다.

### ④ 무결성 검증법 — AC-D4 마커 왕복 (in-process Scenario4)
- **선택**: 동결 전 인메모리 마커(환경 변수 `MARKER`·작업 디렉터리)를 세팅 → 스냅샷 → 복원 →
  `echo $MARKER`·`pwd`로 왕복 확인. 여기에 **커서 연속성**(복원 전 발급 커서로 델타 read) 검증을 더한다.
- **구현**: `control-plane/test/integration_test.go`의 `TestScenario4_CRIUIntegrity`는 실 클러스터
  backend에 대해 in-process `Service.Snapshot`을 호출한다. 배포 e2e의
  `TestDeferred_CRIUIntegrity`는 제품 `POST /sessions/{id}/snapshot` endpoint로 같은 marker/cursor
  왕복을 full stack에서 단언한다.
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
- **런타임 seam(검증됨)**: 실제 criu dump/restore + PTY 재부착은 `data-plane`의 `execCriuEngine`에
  격리되며 kind 배포 e2e가 이 seam을 포함한 왕복을 실행한다. archive framing·복원모드·상태 스왑·
  orchestration은 별도 fake 기반 단위 테스트도 갖는다.

## 게이트 동작
- 환경변수 `CRIU_ENABLED`(기본 `false`).
- **off**: service가 snapshot을 unavailable로 거부하며 live pod와 metadata를 보존한다. 테스트 전용
  enabled stub만 합성 metadata로 lifecycle 골격을 검증한다.
- **on**: `criu.AgentCheckpointer`(에이전트 주도 in-pod CRIU)가 pod 에이전트의 `/checkpoint`·`/restore`를
  구동하고 아카이브를 S3로 중계한다. `main.go`가 게이트로 실 어댑터/스텁을 분기하고 세션 pod에 criu
  권한을 붙인다. standalone `TestScenario4_CRIUIntegrity`는 필요한 env/cluster가 없으면 skip하지만,
  `deploy/` e2e는 게이트·S3-compatible store·trigger를 제공하므로 실제 왕복을 단언하고 실패를 표면화한다.

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
- `control-plane/internal/adapter/checkpointstore/store.go` — **S3 스토어**(`NewS3`). ambient credential을
  사용하고 `CHECKPOINT_S3_ROLE_ARN`이 있으면 그 위에 STS AssumeRole을 적용해 `Put`/`Get`. 가짜 API로 단위 테스트.
- `control-plane/internal/adapter/k8s/client_orchestrator.go` — `RestoreInto`가 restore-target pod를 **고유
  이름**(`sess-<id>-r<suffix>`) + `DATA_PLANE_RESTORE_MODE=1` env로 생성. `WithCheckpointPrivileged`(게이트드)로
  shell session pod를 privileged로 기동한다. `Start`의 신규/restore mode 구분은 유지된다.
- `control-plane/internal/service/manager.go` — `Snapshot`/`Restore` 오케스트레이션. `Restore`가
  `RestoreInto`에 체크포인트 ref를 전달하고 커서를 리셋하지 않는다. shell scrollback은 agent 자체가
  CRIU 대상이 아니므로 CRIU images 옆 archive entry로 별도 직렬화·preload한다.
- `data-plane/cmd/agent/main.go` + `checkpoint.go` — 스왑 가능한 셸 홀더 + 복원모드 기동, `/checkpoint`(criu
  dump→tar) / `/restore`(tar→criu restore→셸 부활) 핸들러, scrollback 직렬화. dump가 셸 트리를 얼려 죽이면
  셸-종료→컨테이너-재시작 경로가 아카이브 스트리밍을 자를 수 있어, checkpoint 중엔 `checkpointing` 플래그로
  재시작을 유예(회수는 컨트롤플레인 Stop이 담당). 셸 fork 직전 `ns_last_pid`에 pid floor(기본 300)를 써서
  복원 시 pid 충돌을 방지(5차). 복원은 `--restore-detached --restore-sibling --pidfile`로 복원된 루트 태스크의
  실제 pid를 감시(3차). 실제 criu 호출은 배포 e2e로, 나머지는 가짜 엔진으로도 유닛 테스트한다.
  scrollback은 에이전트 메모리 상주라 아카이브에 함께 직렬화돼 복원 후 커서 유효(AC-D4).
- `data-plane/Dockerfile` — debian:trixie 런타임 + **criu 4.2 소스 빌드 스테이지**(`CRIU_VERSION` 고정,
  `CRIU_BIN=/usr/local/sbin/criu`, 빌드 시 `criu --version`으로 링크 검증). `ENV GODEBUG=multipathtcp=0`:
  Go 1.24가 Linux 리스너에 MPTCP를 기본 활성화하는데 CRIU는 MPTCP 소켓을 체크포인트하지 못하므로,
  에이전트 :8090 리스너(및 세션 쉘이 상속하는 환경)를 plain TCP로 고정.
- `control-plane/test/integration_test.go` — `TestScenario4_CRIUIntegrity`(마커 왕복 + 커서 연속성).
- `control-plane/test/e2e_deferred_test.go` — `TestDeferred_CRIUIntegrity`(이름은 유지되지만 deploy overlay에서
  실행되는 B2/B3/D4 실단언).

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
  S3로 중계한다. **당시** 남은 런타임 미검증 지점은 `execCriuEngine`(criu 호출 + PTY 재부착)
  하나였고, 이후 kind 배포 e2e 왕복으로 해소됐다.
- 🔧 **레이스 발견→수정됨**: 복원 pod가 동결 pod의 결정적 이름(`sess-<id>`)을 재사용해 Terminating
  잔재와 AlreadyExists 레이스 가능 → 복원 pod를 고유 이름(`sess-<id>-r<suffix>`)으로 분리(2026-07-22).

### 2차 (2026-07-23, k3s · kernel 6.12/arm64 — in-pod criu 사전 점검)
- ❌ capability만으로 불충분: `CHECKPOINT_RESTORE`+`SYS_PTRACE`는 netns 접근 EPERM. `SYS_ADMIN`+`NET_ADMIN`
  추가로 그 계열은 해소되나, non-privileged에선 containerd 기본 AppArmor의 mount 차단과
  `/proc/sys/kernel/ns_last_pid` read-only가 남음.
- ❌ CRIU 3.16.1(jammy)은 kernel 6.12/arm64의 vdso 파싱 실패로 초기화 불가(치명). 같은 노드에서
  **CRIU 4.1.1(Debian trixie)은 통과**.
- ✅ **privileged pod + CRIU 4.1.1 → `criu check` "Looks good" 완전 통과**.
- 🔁 **대응**: 베이스 이미지를 `debian:trixie`(CRIU 4.1.x)로 전환, 게이트 on 세션 pod를 **privileged**로
  기동(`WithCheckpointPrivileged` — 검증된 구성). capability 최소화(caps 4종 + AppArmor unconfined +
  `procMount` 조정)는 라운드트립 green 후의 후속 항목.

### 6차 (2026-07-27, e2e SUT 첫 CRIU-on 실행 — S3 업로드)
- ✅ **in-pod criu dump 성공**: GHA kind SUT에서 스냅샷 요청 시 에이전트가 셸 트리를 실제로 덤프
  (해당 pod가 `0/1` = 셸 동결로 healthz 503, 조기 재시작 억제도 설계대로 동작)하고 아카이브를 스트리밍.
- ❌ **S3 업로드 실패**: `PutObject ... unseekable stream is not supported without TLS and trailing checksum`.
  아카이브는 길이를 모르는 unseekable 스트림인데, aws-sdk는 요청 체크섬·Content-Length를 위해 seekable
  body가 필요하고 대안인 trailing checksum은 TLS를 요구한다(e2e MinIO는 http).
- 🔁 **대응**: `S3.Put`이 스트림을 임시 파일로 spool한 뒤 업로드. 아카이브가 매우 커지면
  bounded-part S3 multipart streaming으로 교체하는 것이 다음 수순.

### 5차 (2026-07-23, pid 충돌 — 간헐 성공의 정체)
- ❌ **복원 pid 충돌**: CRIU는 체크포인트 당시의 pid로 복원한다. 소스 pod의 셸은 에이전트 기동 직후
  떠서 항상 낮은 pid(~10)를 받는데, 복원 pod의 에이전트(PID 1)도 Go 런타임 **스레드가 tid ~10대를
  점유**하므로 `/proc/10`이 이미 차 있어 복원이 실패한다. 4차에서 성공했던 건 우연(복불복).
- 🔁 **대응**: 소스 pod에서 셸 fork **직전에** `/proc/sys/kernel/ns_last_pid`에 큰 값(기본 300,
  `CRIU_PID_FLOOR`로 조정)을 써서 셸 pid를 높게 배정. privileged pod라 쓰기 가능하며(2차에서 non-privileged일
  때 read-only로 걸렸던 그 파일), 복원 pod 에이전트의 tid는 ~15 이내라 충돌이 사라진다. 쓰기 실패는
  best-effort로 로깅만(게이트 off pod는 unprivileged라 정상적으로 실패).

### 3차 (2026-07-23, 에이전트 생명주기 레이스 2건)
- ❌ **dump 중 조기 재시작**: criu dump가 셸 트리를 얼려 죽이면 에이전트의 셸-종료 감시가 `os.Exit(1)`을
  일으켜 **아카이브 스트리밍이 잘림**. → dump 직전 `checkpointing` 플래그로 재시작 경로 유예, 스트리밍
  완료 후 컨트롤플레인의 `Stop`이 pod를 회수한다. dump가 성공하면 shell CRIU 트리는 이미 멈췄으므로
  `AbortCheckpoint`는 이를 재개하지 않는다. 따라서 upload·Stop·final metadata 저장 실패의 durable
  recovery는 아직 없는 명시적 shell lifecycle 부채다.
- ❌ **복원 직후 조기 재시작**: 포그라운드 `criu restore`는 복원 성공과 동시에 종료하는데 이를 셸 사망으로
  오인해 컨테이너 재시작 → 후속 `/write`가 connection refused. → `--restore-detached --restore-sibling
  --pidfile`로 전환해 **복원된 루트 태스크의 실제 pid**를 pidfile에서 읽어 감시/시그널 대상으로 삼음
  (sibling이라 에이전트가 부모 → reap 가능; ECHILD 시 liveness 폴링 폴백).

## CI 프로브 — kind(GitHub Actions)에서 in-pod CRIU가 되는가

에이전트 주도 방식으로 바꾸면서 **kind를 막던 이유(런타임 CRIU 미지원)가 사라졌다** — criu는 우리가
빌드해 pod 안에서 실행하므로 containerd/kubelet이 관여하지 않고, 필요한 건 커널 지원 + privileged pod뿐이다.
e2e 워크플로에 `criu check` 프로브 스텝을 추가해(비차단, `continue-on-error`) GHA 러너 커널 + kind 중첩
컨테이너에서 실제로 되는지 답을 받는다: `criu --version` → `ns_last_pid` 쓰기 가능 여부(= pid floor 가드의
전제) → `criu check`.

**프로브 결과 (2026-07-26, GHA 러너 + kind): 통과** — `criu 4.2` 실행, `ns_last_pid` 쓰기 가능
(pid floor 가드 전제 성립), `criu check` → *Looks good*. (`check --all`은 nftables 기반 locking 미지원
경고만 — 우리 워크로드는 netns 잠금을 쓰지 않는다.) 이로써 **CRIU 라운드트립을 e2e 자동 검증으로 승격**했다:

- ① **스냅샷 트리거**: 제품 `POST /api/v1/sessions/{id}/snapshot`이 동일한 workload별 전략으로
  세션을 즉시 동결하고 pod를 회수한다. UI의 수동 archive/freeze와 e2e의 결정적 트리거가 이 경로를
  함께 사용하며, 60분 idle reaper는 자동 동결 경로로 그대로 유지된다. 복원은 별도 endpoint가 필요
  없다(접근 시 자동 복원).
- ② **AWS 없는 저장소**: 클러스터 안에 **MinIO**를 띄우고 `CHECKPOINT_S3_ENDPOINT`로 가리킨다
  (`deploy/minio.yaml`). 테스트 전용 저장 백엔드를 따로 만드는 대신 **프로덕션과 동일한 S3 코드 경로**
  (`checkpointstore`)를 그대로 태우는 것이 핵심이며, 2개 replica가 같은 저장소를 보는 문제도 자연히
  해결된다(스냅샷과 복원이 서로 다른 replica로 갈 수 있음). e2e SUT는 role을 비워 두고 static key로
  인증한다(=MinIO root) — 프로덕션은 role ARN을 설정해 인스턴스 프로파일 위에서 assume-role 한다.
  즉 이 경로에서 미검증으로 남는 것은 assume-role 홉 하나뿐이다.
- ③ **검증**: `TestDeferred_CRIUIntegrity`가 HTTP만으로 전 스택을 구동 — 마커 세팅 → 스냅샷 → 접근으로
  복원 → 복원 전 커서 델타에 `frozen42`·`/tmp` 확인 + `offset=0` 전체 이력 순서 확인. 트리거가 없는
  SUT에서는 skip한다.

즉 지금까지 수동 라운드로 잡던 종류의 회귀(pid 충돌, 조기 재시작 등)가 **CI에서 자동으로** 잡힌다.

## production 게이트 on 인계

kind/MinIO 경로는 자동 검증된다. production에서 게이트를 켜려면 아래 배포 의존성을 준비한다.

**전제 체크리스트 (에이전트 주도 in-pod CRIU 기준)**
- [x] criu 바이너리를 데이터플레인 이미지에 제공 — 베이스 **debian:trixie** + **criu 4.2 소스 빌드**
      (`CRIU_VERSION` build-arg로 고정, 멀티스테이지). jammy의 CRIU 3.16.1은 kernel 6.12/arm64 vdso 파싱
      실패, noble은 criu 미패키징, trixie는 4.1.1 고정이라 소스 빌드가 필요했다. sid pin은 sid의 glibc가
      런타임(=세션 쉘 userland)에 섞이고 빌드가 움직이는 타깃이 되어 배제. 이미지는 `CRIU_BIN`을
      `/usr/local/sbin/criu`로 고정하고, 빌드 단계에서 `criu --version`으로 링크를 검증한다.
      런타임(containerd) 자체 교체는 불필요 — 에이전트가 pod 안에서 criu를 직접 실행한다.
- [x] 노드 커널 CRIU 지원 — 2026-07-23 2차에서 privileged pod의 `criu check` "Looks good"으로 검증됨.
- [x] 세션 pod 권한: `WithCheckpointPrivileged`가 `CRIU_ENABLED=1`에서 **privileged**로 자동 기동
      (2026-07-23 2차 검증 구성 — capability만으론 AppArmor·read-only `/proc/sys`에 막힘).
      ⚠️ privileged 세션 쉘 = 노드 root 수준 — capability 최소화(caps + AppArmor unconfined +
      `procMount`)는 라운드트립 green 후의 후속 항목.
- [x] `DATA_PLANE_IMAGE` + `CRIU_ENABLED=1` — *2026-07-22 검증 환경에서 수행됨(criu 미포함 이미지 → 위 항목 필요)*.
- [x] **`execCriuEngine` dump/restore + PTY 재부착** — kind 배포 e2e의
      `TestDeferred_CRIUIntegrity`가 실제 marker/cwd/cursor 왕복으로 검증한다.
- [ ] S3 저장소(결정 ③, **배포된 control-plane 실동작용**): 버킷 + `checkpoint-s3` Secret(`bucket`/`role-arn`/
      `region`/`prefix`) + IAM(노드 프로파일 → `sts:AssumeRole` → `s3:PutObject`·`GetObject`). Deployment가 이
      Secret을 `secretKeyRef`(필수)로 읽으므로 Secret 없으면 pod 미기동(CRIU off라도). in-process Scenario4는
      이 S3가 필요 없다(인메모리 스토어로 대체).
- [x] `nodes/proxy` RBAC(`session-platform-node-checkpoint`) **삭제됨**(2026-07-23) — 에이전트 주도 경로는
      쓰지 않는 broad 권한이라 매니페스트에서 제거. CRI-O 대안 채택 시 git 이력에서 재추가.
      이미 적용된 클러스터에선 Flux prune이 꺼져 있으면 수동 정리:
      `kubectl delete clusterrole,clusterrolebinding session-platform-node-checkpoint`.

**확인 명령(green이면 AC-D4 + 커서 연속성 검증 완료)**
```
CRIU_ENABLED=1 DATA_PLANE_IMAGE=<criu-포함 agent-image> \
  go test -tags=integration ./test/... -run TestScenario4_CRIUIntegrity -v
```
- **skip**: standalone integration 실행에 cluster/image/env가 미준비(정상). `deploy/` e2e에서는
  overlay가 cluster/image/store 선결조건을 제공하고 제품 snapshot endpoint를 호출해 실단언한다.
- **fail**: standalone integration은 CRIU 호출/PTY 재부착/복원 중 하나가 계약과 맞지 않음. deploy e2e는
  여기에 S3-compatible MinIO 중계까지 포함한다.
- **pass**: 동결 전 `MARKER`·cwd가 복원 후 그대로 재개 + 복원 전 커서로 델타 read 유효 → 완료.
> in-process Scenario4는 **인메모리 스토어**로 아카이브를 체크포인트↔복원 사이 중계하므로 S3 env가
> 필요 없다(에이전트가 아카이브를 HTTP로 테스트 프로세스에 스트리밍). **S3 업로드 경로**는 (1) 단위
> 테스트(`checkpointstore`/`criu`)와 (2) `checkpoint-s3` Secret이 주입된 **배포된 control-plane**에서 검증된다.

## 리스크 / 대안
- **post-dump failure recovery**: dump 성공 뒤 upload·pod Stop·final metadata 저장이 실패하면 shell을
  재개할 filesystem abort protocol이 없다. durable shell transaction/reconciler가 필요하다.
- **restore-target orphan**: CRIU restore와 workload-neutral restore 모두 새 pod의 restore가 성공한 뒤
  final aggregate CAS 전에 control plane이 hard crash하면 target pod를 orphan할 수 있다. durable
  RestoreTransaction, deterministic target 또는 orphan reconciler가 필요하다.
- **production 환경 차이**: kind/GHA 커널 경로는 검증됐지만 실제 node kernel, security profile,
  S3 AssumeRole/IAM은 대상 환경 smoke가 필요하다.
- **권한 범위**: 현재 게이트 on shell pod는 privileged다. capability/AppArmor/procMount 최소화가 후속이다.
- **실행 중 포그라운드 프로세스/FD 캡처 온전성**: AC-D4 마커는 우선 env·cwd 위주. 실행 중 프로세스
  케이스는 트리거 정책 확정과 함께 후속 시나리오로(범위 밖, `doc-tracker.md`).

## 관련 문서
- `docs/prd/shell-workload.md` — AC-D4(보존 상태), offset·복원 설계 노트(커서 유효).
- `docs/test/shell-workload.md` 시나리오 4 / `docs/test/lifecycle.md` 시나리오 3 — 마커 왕복.
- `deploy/kustomization.yaml` — kind overlay는 CRIU·MinIO를 활성화하고 제품 snapshot endpoint를 검증한다.
