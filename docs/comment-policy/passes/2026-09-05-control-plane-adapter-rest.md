# 2026-09-05 — `control-plane/internal/adapter/` (k8s 외 전부, 판정 508줄)

3차 패스가 `adapter/k8s/` 4파일을 등재했으므로, 이 패스로 **어댑터 계층이 닫힌다** — `criu`(5파일
215줄) · `configmap`(2파일 107줄 + `envtest` 2파일 33줄) · `agent`(3파일 83줄) ·
`checkpointstore`(2파일 70줄). **제거 156 · 유지 352**(30.7%).

**이 패스의 주된 형태는 「같은 문단이 한 파일 안에 두세 벌」이었다.** 앞선 패스들이 파일↔문서,
소스↔테스트 사이의 되풀이를 골라냈다면 여기서는 *인터페이스 doc · 구현 doc · 구현 본문 인라인*이
같은 계약을 각각 한 번씩 적는 형태가 반복됐다. 가장 큰 한 건: `container_checkpointer.go`의
「K8s-native restore에는 kubelet API가 없다 — 재개는 RestoreInto가 단 어노테이션을 런타임이
읽어 일으키고, 여기서 구동할 것은 없다」가 **`CheckpointDriver.Restore` doc · `Restore` doc ·
`kubeletDriver.Restore` 본문 세 곳에** 있었다. 계약을 소유하는 인터페이스 doc 하나만 남겼다.

**제거 156줄**

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `criu/container_checkpointer.go` 패키지 주석 (13→4) | 「2026-07-22 k3s/containerd 검증에서 dump는 되지만 containerd가 복원할 방법이 없어 왕복이 막힌다 → 미배선으로 유지, CRI-O 도입 시 checkpoint OCI 이미지 경로」 | ② `docs/criu-verification.md`의 **결정 ⑤ 근거·대안**과 「실구현 요약」의 이 파일 행(「CRI-O 대안(미배선) … 유지·테스트되지만 wired가 아님」)이 같은 말을 더 자세히 적는다. **주석이 스스로 그 문서를 가리키고 있었다** — 포인터만 남겼다 |
| `criu/container_checkpointer.go` `Restore` doc (8→5) · `kubeletDriver.Restore` 본문 (9→1) | 위의 세 벌 중 둘 | ① `CheckpointDriver.Restore` 인터페이스 doc. s3:// ref일 때 **노드 쪽이 assume-role로 아카이브를 당겨 온다**는 사실은 그 doc에 없어 남겼다 |
| `criu/container_checkpointer.go` Option 5종 (12→5) · `NewContainerCheckpointer`(2→1) · `Checkpoint` 본문 인라인(2→0) | 「기본값은 k8s.ContainerName」 · 「테스트가 CRIU 없이 오케스트레이션을 돌리게」 · 「store 없으면 node-local 경로, 있으면 durable ref」 · 「WithDriver 없으면 실 kubelet」 · 「체크포인트 엔드포인트는 파드가 뜬 노드에 있다」 | ① 각각 생성자 본문(`container: k8s.ContainerName`, `if c.driver == nil`) · `CheckpointDriver` doc · `Checkpoint` 본문의 두 분기 · `kubeletDriver` doc의 **URL 템플릿 `{node}`** 가 그대로 보여 준다 |
| `criu/agent_checkpointer.go` 패키지 주석 (12→6) · `NewAgentCheckpointer`(2→1) · `NewAgentArchiveCheckpointer`(3→2) | 「kubelet 방식을 대체한 이유(2026-07-22)」 · 「CRIU-capable 노드가 필요한 것은 셸 에이전트의 criu 호출뿐」 · 「완료된 dump와 달리 프롬프트 admission을 다시 열 수 있다」 | ② `criu-verification.md` 결정 ⑤(근거·런타임 seam) · ① 같은 파일 `AbortCheckpoint` doc이 그 대비를 **행동을 지배하는 자리에서** 적는다 |
| `criu/checkpointer.go` 패키지 주석(4→3) · `StubCheckpointer`(4→3) · `NewStubCheckpointer`(3→1) · no-op 인라인 2곳 (6→0) | 「프로덕션 셸은 agent-driven CRIU, claude-code는 파일시스템 아카이브」 · 「합성 metadata 뒤에서 live pod를 지우지 않는다」의 되풀이 · 「스텁은 격리된 lifecycle 테스트용」의 **네 벌** | ① `agent_checkpointer.go` 패키지 주석(전략 배분의 정본) · ① 같은 파일 `StubCheckpointer` doc · ① `service/manager.go` `checkpointerFor`(2차 패스가 데이터 손실 근거로 남긴 정본) |
| `criu` 테스트 2파일 (38→8) | 테스트 이름과 소스 doc이 그대로 단언하는 서술 — 「fake stands in … without a CRIU-capable runtime」 · 「durable store가 필요하다: 아카이브는 곧 회수될 파드 안에서 만들어진다」 · 「store가 있으면 durable ref를 대신 기록한다(결정 ③)」 · 「Restore는 nil/빈 체크포인트를 거부한다」 | ① `CheckpointDriver`·`AgentCheckpointer`·`WithStore`·`checkpointKey` doc이 각각 정본이다. **`fakeStore.Get` 이 kubelet 경로에서 안 쓰이는데도 있는 이유**(공유 계약)와 「driver 실패의 예(kubelet 불통·게이트 off·런타임에 CRIU 없음)」는 남겼다 |
| `agent/client.go` `HTTPClient` doc(6→3) · `Checkpoint`(4→2) · `AbortCheckpoint`(4→3) · `AwaitingApproval`(8→4) · `Stream`(3→2) · `WithPort`(2→1) | 「같은 client가 /checkpoint·/restore도 지고 IP 해석을 재사용한다」 · 「호출자가 durable storage로 흘리고 Close 해야 한다」 · 「셸 CRIU 호출자는 이 메서드를 부르지 않는다」 · 「Client 인터페이스를 넓히면 모든 fake가 바뀐다」 · 「30초 데드라인이 없다」 | ② `criu-verification.md` 실구현 요약의 이 파일 행 · ① `criu.AgentCheckpointClient` doc(계약의 정본, 「The caller closes it」 포함) · ① `AgentCheckpointer.AbortCheckpoint` doc · ③ PR #71 본문(「선택적 능력 인터페이스로 붙여 기존 fake를 하나도 고치지 않았다」) · ① `stream` 필드 doc |
| `checkpointstore/store.go` 패키지 주석(6→3) · `NewS3`(4→3) · 본문 인라인 2곳(7→1) · `Put` doc(9→7) · `Config`(2→1) | 「ambient chain + optional assume-role」의 **세 벌**(패키지 주석·`NewS3` doc·본문) · 「role 없으면 ambient 그대로」 · 「대형 아카이브는 multipart uploader로 바꿀 수 있다」 | ② `criu-verification.md` 결정 ③의 「권한(코드)」이 **SDK 심볼 이름까지**(`stscreds.NewAssumeRoleProvider` · `aws.NewCredentialsCache`) 적고, 「경계/후속」이 spool과 multipart 후속을 적는다 · ① `if cfg.RoleARN != ""` 분기. **spool이 필요한 이유**(SDK가 seekable body를 요구하고 trailing checksum은 TLS를 요구한다)는 외부 시스템 지식이라 남겼다 |
| `configmap/store.go` 패키지 주석(17→13) · `var _`(1→0) · `WithLeaseDuration`(2→1) · CAS 본문 인라인(2→0) | 「인메모리 스텁과 같은 StateStore 계약을 유지한다」 · 컴파일타임 단언 · 「(default 15s)」 · 「cm이 Get의 resourceVersion을 지니므로 409로 하나만 이긴다」 | ④ **그 인메모리 스텁은 지금 레포에 없다**(`internal/store/`는 포트 하나뿐, 유일한 구현이 이 어댑터다) — 낡아 거짓이 된 이력 서술이라 커밋 이력에 맡겼다 · ① 선언 자체 · ① `defaultLeaseDuration` doc · ① 같은 함수의 doc과 패키지 주석이 이미 두 번 말한다 |
| `configmap`·`checkpointstore` 테스트 doc (48→20) | 소스 doc의 사본과 테스트 이름 재진술 — 「Unlock은 토큰이 쥔 락만 푼다」 · 「크래시한 홀더의 락은 self-heal한다」 · 「Delete는 Lease를 유지한다」 · 「Get은 s3:// ref를 파싱한다」 | ① `Unlock`·`heldByUsOrExpired`·`Delete`·`Get` doc과 패키지 주석이 각각 정본 · ① 테스트 함수 이름. **본문 안의 「지금 상태가 idle이므로 from=active는 conflict」류 인라인과 「e2e MinIO에서 실제로 실패했다」는 관측은 남겼다** |
| `configmap/envtest/store_conflict_test.go` 테스트 doc (24→20) | 두 테스트 doc이 되풀이한 「단일 승자」 서술 | ① 같은 파일 패키지 주석이 **왜 fake clientset으로는 안 되는지**까지 포함해 소유한다. `go.mod`의 9줄(중첩 모듈이 부모 `./...`에서 빠지는 이유)은 **한 줄도 건드리지 않았다** |

**유지 352줄** — 「지울까」를 검토했다가 남긴 것들.

- **`configmap/store.go` 74줄(82에서 8만 제거)** — Lease/CAS/resourceVersion의 순서·원자성 계약이
  대부분이다. 정책이 명시적으로 보호하는 유형이고, AC-C1은 「atomic 전이」만 말할 뿐 **어느 실패가
  어느 쪽으로 해석되는지**는 말하지 않는다.
- **`criu/container_checkpointer.go`의 kubelet 지식** — `POST /api/v1/nodes/{node}/proxy/checkpoint/…`
  URL 템플릿, feature gate와 `nodes/proxy` RBAC, kubelet 응답 JSON 형태, 아카이브 파일명이
  타임스탬프를 담아 유일하다는 사실.
- **`checkpointstore/store.go`의 S3 지식** — spool이 필요한 이유(위), path-style 주소가 필요한 이유
  (S3-compatible 엔드포인트에는 버킷별 DNS가 없다), `defaultSessionName`이 CloudTrail에 보인다는 것.
- **`agent/client.go`의 pod IP 재해석** — 「복원을 건너면 pod IP가 유지되지 않고 세션은 pod 이름만
  들고 있다」. 어느 문서에도 없다.
- **`envtest` 두 파일의 존재 이유** — 「fake clientset의 object tracker는 resourceVersion 낙관적
  동시성을 강제하지 않아 진짜 CAS 레이스를 재현하지 못한다」와 「중첩 모듈이라 부모의 `./...`가
  이 트리를 컴파일조차 하지 않는다」. 둘 다 도구의 문서화되지 않은 동작이다.

**갈려서 남긴 것 하나.** `criu/checkpointer.go`의 `Checkpointer` 인터페이스에 붙은 `AC mapping:`
블록(6줄)은 1차 패스가 `session.Manager`에서 같은 형태를 남긴 것과 같은 이유로 남겼다 — **Go
메서드에서 AC로 가는 색인은 어느 문서에도 한 덩어리로 없다.**
