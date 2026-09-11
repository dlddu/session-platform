# 2026-09-04 — `control-plane/internal/adapter/k8s/` (판정 473줄)

레포 최대 밀집 구간. 4파일 전부를 통째로 읽었다 — `client_orchestrator.go`(327) ·
`orchestrator.go`(86) · `network_policy.go`(53) · `orchestrator_test.go`(7).

**왜 이 범위인가**: 이 패키지의 주석은 AC-A2·A3·E1·E6·F2·F4·F6이 세 번의 PR(#61 계열 ·
#64 · #62)에 걸쳐 착지하면서 **PRD 문장이 그때마다 코드로 한 번씩 더 복사된** 자리다. 그래서
제거율이 앞선 두 패스(12% · 25%)보다 높은 **32%**다 — 판정이 엄해진 것이 아니라 같은 문장이
여러 벌 있었다. 포트(`orchestrator.go`)와 구현(`client_orchestrator.go`)이 **한 패키지 안에서
서로를 되풀이**하므로 파일 하나만 집으면 「지운 쪽의 사본」을 소유하지 못한다 — 패키지 경계로
잡은 이유다.

**제거 153줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `client_orchestrator.go` 파일 헤더 (7→1) | 「세션당 전용 파드를 몰고(AC-A1/A2), stop에서 회수하고(AC-A3), 워크로드 에이전트에 닿음을 증명(AC-D1/E1)」 · 「워크로드는 data plane 이미지의 entrypoint가 시작한다」 · 「main은 BuildClient로 client/namespace를 만든다」 | ① 같은 파일 `ClientOrchestrator` 타입 doc이 첫 절을 그대로 다시 말한다 · ① `buildPod`의 「No command override」가 둘째를 · ① `NewClientOrchestrator` doc이 셋째를 · ① `orchestrator.go`의 패키지 주석이 「포트·구현·스텁이 어디 있는지」를 이미 말한다(포인터만 남겼다) |
| `client_orchestrator.go` 자격 증명 분리 4곳 (25→8) | Claude 런타임 const 블록(7), approval-gated const 블록(6), `buildPod`의 approval-gated case(8→3), `claudeCredentialProxy`(4→1)·`credentialProxyContainer`(6→3) — **같은 성질을 네 번** | ② `prd/claude-code-workload.md` AC-E6(*「주 컨테이너에는 실제 공급자 자격 증명을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다」*)과 `prd/approval-gated-workload.md` AC-F6의 두 불릿(*「게이트웨이 URL·API key와 …userId → 헬퍼 파드의 MCP 컨테이너에만」*, *「공급자 base-url·auth-token → …credential-proxy 컨테이너에만」*) · ① 바로 아래 `secretEnv(...)` 목록이 어느 키가 어느 컨테이너로 가는지 그 자체로 보여 준다 |
| `WithCheckpointPrivileged`(8→4) · `buildPod` privileged 인라인(5→2) | 2026-07-23 검증의 세부(CHECKPOINT_RESTORE+SYS_PTRACE의 netns EPERM, containerd AppArmor의 mount 차단, read-only `ns_last_pid`, privileged에서 `criu check` 완전 통과, 최소화 후속) | ② `docs/criu-verification.md` 「2차 (2026-07-23 …)」가 그 다섯을 그대로 적는다. **인라인 쪽은 이미 「see WithCheckpointPrivileged」라고 자백하면서 내용을 또 옮겼다** — 포인터만 남겼다 |
| `AnnotationRestoreCheckpoint` (8→4) | CRI-O `io.kubernetes.cri-o.restore` 어노테이션·체크포인트 OCI 이미지 매핑 서술 · 「export한 이유는 그 런타임 매핑이 복원 계약의 일부라서」 | ② `docs/criu-verification.md`의 「CRI-O 대안(미배선)」 항목들 — **주석이 스스로 그 문서를 인용하고 있었다.** 포인터와 「미배선」 사실만 남겼다 · ① export 여부는 선언이 말한다 |
| `orchestrator.go` 포트 메서드 doc 3곳 (11→3) | `Start`/`Stop`/`RestoreInto` doc의 AC 해설과 「variadic이라 whole set을 넘긴다」·「unique name이라 terminating pod와 충돌하지 않는다」 | ① 같은 doc 블록 바로 위의 `AC mapping:` 색인(유지) · ① 시그니처가 가변인자임을 말한다 · ① `restorePodName` doc이 이름 충돌 회피를 정본으로 적는다 |
| `client_orchestrator.go` `Start`(4→1) · `RestoreInto`(6→3) · `Stop`(5→4) | 포트 doc이 이미 말한 계약의 재진술 | ① `orchestrator.go`의 `PodOrchestrator` 인터페이스 doc — **포트와 구현이 같은 패키지에 있어 한 번만 말하면 된다** |
| `var _ PodOrchestrator = …` (1→0) | `compile-time assertion that ClientOrchestrator satisfies the port.` | ① 그 선언 자체가 컴파일타임 단언이다. **직전 패스가 `service.Service`에서 지운 것과 같은 형태** |
| `workloadImages` 필드 주석 (3→0) · `WithWorkloadImage`(6→2) · `WithImage`(2→1) | 「기본 타입은 `image`로 폴백, 미설정 타입은 거부」의 **3중 서술** · 「alpine 폴백은 readiness probe를 통과하지 못한다」 사본 | ① `imageFor` doc이 그 규칙의 정본이다(유지) · ① `defaultDataPlaneImage` doc이 alpine 함정의 정본이다 — `WithImage`는 이미 「see defaultDataPlaneImage」라 적고 있었다 |
| `SessionMCPContainerName` (3→2) | 「이 슬라이스는 컨테이너만 프로비저닝하고 **게이트 자체는 아직 구현되지 않았다**」 | ② `docs/doc-tracker.md`(2026-09-04) — *「승인 게이트가 착지했다 … `tools/call`이 승인 게이트웨이에 요청을 만들어 … APPROVED일 때만 실제 외부 GET을 한다」*. **#64가 머지되며 거짓이 된 주석**이다 |
| `ContainerName` (3→2) | 「shell 파드는 이것뿐, Claude 파드는 격리된 credential-proxy 사이드카를 더한다」 | ① 같은 파일 `buildPod`의 `switch workloadType` 세 case · 게다가 approval-gated가 생기며 **불완전해졌다**(그 타입은 사이드카 대신 헬퍼 파드다) |
| K3s MCP·마켓플레이스 URL const 블록 (7→3) | 「플러그인 부트스트랩이 닿는 두 엔드포인트」 서술과 「조직 엔드포인트에 못 닿는 환경(kind e2e SUT)이 같은 코드 경로를 인클러스터 대역으로 돌릴 수 있다」 | ② `prd/claude-code-workload.md` AC-E6이 부트스트랩 왕복을, `docs/test/e2e.md`의 `PLUGIN-CRED` 행이 **그 대역 치환을 통째로** 적는다 · ① `optionalSecretEnv` 호출이 optional임을 말한다 |
| `agentAttachPath`(3→1) · `Reach`(4→3) · 「Opening the stream is the proof」(1→0) · readiness probe 인라인(2→0) | attach가 무엇인지·healthz가 왜 readiness인지의 재진술 | ① `Reach` doc과 `agentHealthzPath` doc이 각각 정본이다 |
| `helperPodSpec`(10→6) · `SessionIDEnvVar`(6→3) · `ApprovalGateway*` 트리오(3→1) · `SessionMCPURLEnvVar`(3→2) · `HelperCredentialProxyContainerName`(3→2) | 「보조 파드라 AC-A2를 깨지 않는다」·「상태를 갖지 않아 동결 시 버려진다」·「`{세션ID}:{요청ID}`이고 게이트웨이가 중복을 거부한다」·「자격 증명이 아니라 파드가 이미 라벨로 다는 값」·「MCP가 파드 밖 유일한 도구 표면」 | ② AC-F4·AC-F3·AC-F6 본문이 전부 축자에 가깝다 · ② `doc-tracker.md`가 `SESSION_ID`를 *「자격 증명이 아니라 파드가 이미 라벨로 다는 값」* 으로 그대로 적는다 |
| `network_policy.go` 파일 헤더 (20→12) | 「PRD는 이 성질을 워크로드 파드가 무엇에 닿을 수 있는지로 적는다: kube-dns와 자기 세션 헬퍼 파드, 그 밖은 전부 차단」 · 「집행 여부는 CNI의 몫이고 kindnet은 NetworkPolicy를 구현하지 않는다」 | ② AC-F2 본문이 허용·차단 목록을 그대로 적는다 · ② `doc-tracker.md`의 열린 항목이 집행 미검증을 적고, **주석이 이미 그 문서를 가리키고 있었다** — 「오브젝트만 세운다」는 사실과 포인터만 남겼다 |
| `orchestrator.go` `SessionPods`(7→3) · `SetAuxiliaryPods`(8→6) · `RunningCount`(5→3) · `SessionsCount`(3→1) · `StubOrchestrator.Start` 인라인(3→0) · 부분 회수 인라인(2→0) | 「shell·claude-code는 보조 파드가 없고 approval-gated는 정확히 하나」 · 「보조 파드가 함께 기동·회수·복원된다」 · 「RunningCount가 예전에 세션 수를 셌다」 | ② AC-A2의 보조 파드 절이 타입별 개수를 적는다 · ② `doc-tracker.md`의 해소 항목이 스텁 노브가 단정하는 네 가지를 그대로 열거한다 · ④ RunningCount의 과거 의미는 커밋 이력이 갖는다 |
| `orchestrator_test.go` 테스트 doc 2곳 (7→3) | 「이름이 서로 달라 각각 회수할 수 있고 All이 워크로드 파드를 앞에 둔다」 · 「워크로드 파드만 멈추면 완전 회수로 보이면 안 된다」 | ① 바로 아래 본문의 단언과 실패 메시지 — 특히 *「(the auxiliary pod leaked)」* 가 같은 말을 한다 |
| 그 밖 doc 축약 10여 곳 | `WithAgentPort`·`WithReadiness`의 「테스트가 주입한다」, `NewClientOrchestrator`의 「fake clientset」, `BuildClient`의 in-cluster/kubeconfig 분기 서술, `provision`·`startSet`·`buildPod`의 checkpointRef 분기 재진술, `hardenedSecurityContext`·`helperPodName`·`helperRestorePodName`의 본문 재진술, `claudeCredentialsSecret` 관련 옵션 doc | ① 전부 같은 파일의 본문·시그니처·다른 doc이 말한다 |

**유지 320줄** — 「지울까」를 검토했다가 남긴 것들.

- **`PodOrchestrator`의 `AC mapping:` 색인(5줄)** — 직전 패스가 `session.Manager`의 같은 블록을
  남긴 것과 **같은 이유**: 개별 대응은 ②로 복원되지만 **Go 메서드에서 AC로 가는 방향의 색인은
  어느 문서에도 한 덩어리로 존재하지 않는다.** 다만 각 항목에 붙어 있던 괄호 해설은 AC 재진술이라
  지우고 **색인만** 남겼다 — 그것이 이 블록이 유일하게 갖는 값이다.
- **`restorePodName`의 이름 충돌 레이스(8줄)** — 「동결이 지운 파드가 아직 Terminating일 때
  같은 이름을 재사용하면 create가 AlreadyExists로 진다」. 되돌리기 어려운 실수의 근거이고
  어느 문서에도 없다. `restoreSuffix`의 63자 DNS 라벨 산술(2줄)도 같은 성격이다.
- **`pullPolicyForImage`(7줄 + 파싱 인라인 3줄)** — 「같은 태그의 낡은 캐시를 노드가 내주어
  새 control plane이 그 라우트가 없던 옛 에이전트에 `/read`를 걸었다」는 **관측된 실패**와,
  레지스트리 포트 콜론을 태그 구분자로 오인하지 않기 위한 파싱 근거.
- **헬퍼 파드 MCP의 exec probe 이유(6줄)** — 「HTTP probe는 파드가 아니라 kubelet에서 출발하는데
  AC-F2의 ingress 정책은 그 세션 워크로드 파드만 허용한다. 정책을 집행하는 CNI 아래에서는
  probe가 경계가 거절하도록 설계된 바로 그 호출자가 되어 헬퍼 파드가 영영 Ready에 못 간다.」
  AC-F2는 이 상호작용을 말하지 않는다.
- **`network_policy.go`의 NetworkPolicy 의미론(약 15줄)** — 「peer에 namespace selector가 없으면
  이 네임스페이스 밖 파드는 매치될 수 없고, 남의 세션 헬퍼를 배제하는 것은 selector의 세션 id다」,
  「정책은 additive라 복원 중 두 라운드의 쌍이 무해하게 겹친다」, `ownerReferenceTo`의
  「`BlockOwnerDeletion`을 일부러 끄는 이유는 pods/finalizers 업데이트 권한이 정책 회수 값어치보다
  넓은 부여이기 때문」. 전부 코드 형태로는 보이지 않는다.
- **cross-module 「Keep in sync with data-plane/cmd/agent」 4건** — `AgentPort`·`SessionIDEnvVar`·
  `restoreModeEnvVar`·`CredentialProxyPlacementEnvVar`. 컴파일러가 잡지 못하는 계약이라 포인터가
  유일한 방어다. `CredentialProxyPlacementEnvVar`의 「바인드 주소에서 추론하지 않고 선언한다 —
  선언이 없으면 제한적인 쪽」도 data plane 쪽 동작이라 남겼다.
- **`defaultDataPlaneImage`(6줄)** — alpine 폴백이 readiness probe를 통과할 수 없다는 함정.
  `claudeCodeStateDir`의 「활성 마운트 지점 rename은 EBUSY」(3줄), `BuildClient`의 「deferred
  kubeconfig 로더는 in-cluster 네임스페이스를 읽지 않는다」(3줄)도 같은 성격의 외부 시스템 지식이다.

**갈려서 남긴 것 셋.**

1. **`LabelPodRole`의 selector 함정(4줄)** — 「모든 파드가 세션 id를 달고 있어, `LabelSessionID`만
   보는 selector는 헬퍼 파드를 그 세션의 두 번째 워크로드 파드로 센다」. AC-A2의 1:1 성질로
   복원된다고 볼 수도 있으나, **성질과 그 성질을 selector로 옮길 때의 함정은 다르다.** 남겼다.
2. **`helperPodSpec`의 「Kubernetes 신원을 주지 않는다」(2줄)** — `automount=false`는 ①로 보이지만,
   *왜*(안에 API server가 필요한 것이 없고, 이 파드가 플랫폼의 외부 비밀 **둘**을 쥐고 있다)는
   어느 문서에도 없다. AC-F6은 비밀 배치만 말하고 신원 부재는 말하지 않는다.
3. **`startSet`의 보조 파드 선행 기동(5줄)** — AC-F4는 「복원된 워크로드 파드에 그 복원의 헬퍼 파드
   주소가 주입된다」를 말하지만, **그래서 기동 순서가 강제된다**는 것은 말하지 않는다. 순서 계약은
   유지 대상이다.

**지문 사각지대 — 이 범위에서 실측했다.** 줄 끝 주석 **7건**(`ClientOrchestrator`의 필드 4개,
`StubOrchestrator`의 맵 3개)이 판정 지문에 보이지 않는다. 반대로 직전 패스가 `internal/service/`에서
5줄 보고한 **오검출**(포인터 역참조·임베드 필드)은 이 범위에 **0건**이다. 둘 다 정의가 유예한
후속 항목이며, 지금은 **게이트가 지문을 강제하므로 사각지대를 넓히는 변경은 지문을 바꾸지 못한다** —
패턴을 넓히려면 게이트·모델 정의·이 문서를 한 번에 고쳐야 한다(그 세 곳이 글자 그대로 같아야 한다는
요구를 스크립트 상단에 못박아 두었다).

## 재판정 — #66이 더한 14줄 (게이트의 첫 양성)

이 패스를 실은 PR(#69)이 머지되기 5분 전에 형제 PR #66이 같은 범위에 주석 **14줄**을 더했다.
게이트는 머지 직후 main에서 즉시 R2로 그 사실을 보고했다(등재 320 != 실측 334). **이것이 이
게이트를 세운 이유 그 자체다** — 게이트가 없었다면 그 14줄은 「판정 완료」로 표시된 범위 안에
조용히 들어앉아, 어느 행에도 속하지 않은 채 영원히 미판정으로 남았을 증분이다. 아래는 그
증분에 대한 재판정이고, 이 행의 줄 수·지문 갱신이 곧 그 기록이다.

**제거 6줄**

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `client_orchestrator.go` `AnthropicCACertEnvVar` doc (9→8) | 「Secret 키가 optional이라 생략하면 프록시는 시스템 풀에 남고 `k8s/`는 그대로다」 | ① 같은 파일 `optionalSecretEnv`(선언 자체가 `Optional=true`를 세운다) · ① **바로 아래 이웃 블록**이 optional 키 일반에 대해 *「absent, the entrypoint keeps its built-in defaults, so k8s/ needs no change」* 로 이미 말한다 |
| `credentialProxyContainer` 인라인 (5→0) | 「private gateway일 때만 존재한다」 · 「자격 증명과 같은 컨테이너에 실려 두 배치가 이 한 함수에서 같이 받는다」 · 「tool-running 컨테이너는 loopback 주소 말고는 공급자에 대해 아무것도 모른다(AC-E6/AC-F6)」 | ① 바로 위 `AnthropicCACertEnvVar` doc(private gateway 근거, 유지했다) · ① **그 함수 자신의 doc** *「builds the provider proxy in either of its two placements. One behaviour contract (AC-E6) for both; only the placement and bind address differ (AC-F6)」* · ② AC-E6 *「주 컨테이너에는 실제 공급자 자격 증명을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다」* + ① `claudeCredentialProxy` doc |

**유지 8줄** — `AnthropicCACertEnvVar` doc의 나머지.

- **존재 이유**(「공개 저장소가 모르는 CA가 발급한 private gateway를 시스템 루트에 *더해* 신뢰한다」)는
  상수 이름이 말하지 않고 어느 AC에도 없다. AC-E6은 `base-url`·`auth-token`·`model`만 다루고
  `ca-cert`를 언급하지 않는다.
- **분류 근거**(「주소류 설정이지 자격 증명이 아니다 — CA 인증서는 구성상 공개된 값이다」)는
  이 파일의 credential/address 이분법이 왜 이쪽으로 갈렸는지를 말한다. 어느 문서에도 없어 남겼다.
  **「Like the two bootstrap URLs below」의 상호참조는 rebase 후 다시 확인했다** — 이 패스가 그
  이웃 블록을 7줄 → 3줄로 줄였지만 블록과 「optional 키」 성질은 남아 있어 참조가 성립한다.
- **포인터**(`docs/test/e2e.md`의 `CLAUDE-PROVIDER`)는 SSOT로 가는 길이라 남긴다.
- **「Keep in sync with data-plane/cmd/agent (providerCACertEnv)」** — 컴파일러가 잡지 못하는
  cross-module 계약. 이 패스가 같은 형태 4건을 남긴 것과 같은 이유다.
