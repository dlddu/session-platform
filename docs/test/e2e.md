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
아래 다섯 블록과 코드를 **양방향으로** 대조해 강제한다(등재 · 미해소 위반 · 차단 요인 · 회계 ·
집계) — `make check-fidelity`(= `make lint`에 포함), 그리고 모든 PR에서 도는 `ci.yml`의
`fidelity` 잡.

### 등재된 seam

<!-- fidelity:registry -->
| CODE | 카테고리 | e2e에서 실제로 구동되는 구간 | 치환으로 검증되지 않는 잔여 |
| --- | --- | --- | --- |
| `CRIU-GATE` | `GATE` | e2e SUT는 항상 `deploy/` 오버레이로 뜨고(`scripts/e2e/up.sh`의 `kubectl apply -k deploy/`) 거기서 게이트가 **ON**이라 실 CRIU 경로를 탄다 — 에이전트가 pod 안에서 쉘 트리를 dump/restore하고, 아카이브는 인클러스터 MinIO를 향해 **프로덕션과 같은 S3 코드 경로**로 오간다. 동결은 제품 endpoint(`POST /api/v1/sessions/{id}/snapshot`)로 유발되고, 왕복 전체가 AC-B2/B3/D4 전용 파일의 실 단언이다. | 프로덕션 base(`k8s/`, `CRIU_ENABLED: "false"`)의 쉘 동결·복원은 **no-op 스텁이고 어떤 e2e도 그 경로를 타지 않는다**. 검증된 런타임이 서고 base가 on으로 올라가기 전까지, 프로덕션 구성에서 쉘 세션 동결이 상태를 실제로 보존하는지는 미검증이다. |
| `PLUGIN-CRED` | `EXT` | claude-code 세션 pod는 agent 를 띄우기 전에 `data-plane/entrypoint.sh` 가 K3s MCP 에서 마켓플레이스 읽기 토큰을 받아 플러그인을 설치한다. e2e SUT 에서는 그 **자격 발급 한 지점만** 인클러스터 대역(`deploy/e2e-k3s-mcp-fake.yaml`)으로 바꾸고 나머지는 전부 실물이다 — 토큰 요청은 pod 안의 **실 curl·실 jq**가 프로덕션과 같은 JSON-RPC 왕복으로 보내고, 받은 토큰은 같은 `http.<url>.extraheader` 로 좁혀져 **실 git 클론**(인클러스터 원격, 아래 비-seam)에 쓰이며, `claude plugin marketplace add`·`claude plugin install` 은 실 Claude CLI 가 실행한다. 대역이 bearer 를 검사해 틀리면 401 을 내므로 인증 단계도 그대로 밟는다. 이 경로가 서야 pod 가 Ready 에 이르고, `control-plane/test/e2e_e1_workload_type_test.go` 가 그 위에서 claude-code 세션 생성과 pod 안 `claude --version` 실행을 단언하고, **인클러스터 픽스처에만 있는 마커 문자열이 그 pod의 플러그인 캐시에 있음**을 확인해 *어느 원격이 응답했는지*까지 못 박는다. | **프로덕션의 실제 자격 발급 왕복은 미검증**이다 — 실 K3s MCP 의 GitHub App 설치 토큰 발급, 그 토큰의 스코프·만료, github.com 에 대한 인증 클론은 어떤 e2e 도 밟지 않는다. 대역이 돌려주는 것은 형태만 같은 가짜 토큰이라, 발급이 실패하거나 권한이 모자랄 때의 동작도 여기서는 드러나지 않는다. |
| `DELETE-CONFLICT-ERR` | `NET` | `web/e2e/journeys/session-deletion.spec.ts`의 스냅샷 삭제 테스트는 **실 SUT 위에서** 돈다 — 세션을 `POST /api/v1/sessions`로 실제로 만들고 제품 `POST /api/v1/sessions/{id}/snapshot`으로 진짜 `snapshot` 상태(pod 회수 포함)까지 얼린 뒤, `/restore/{id}` 렌더·세션 목록·단건 조회·**재시도 DELETE(204)** 가 모두 배포된 control-plane의 실 응답이다. 인터셉트가 만드는 것은 **첫 DELETE 한 번의 409 응답뿐**이고, 나머지 요청은 같은 핸들러에서 `route.continue()`로 그대로 흘려보낸다. | 409를 낳는 **진짜 경합** — 다른 라이프사이클 연산이 세션 Lease를 쥔 상태에서 들어온 DELETE(`service.Terminate`의 `store.Lock` → `session.ErrConflict`) — 자체는 브라우저 e2e에서 검증되지 않는다. 주입은 UI의 실패 표시·재시도 경로만 확인하고, 서버가 그 상태에서 실제로 409를 내는지는 control-plane 단위 테스트와 envtest의 CAS/Lease 충돌 케이스가 담당한다. |
| `CLAUDE-PROVIDER` | `EXT` | claude-code 세션의 프롬프트 왕복에서 **응답을 만드는 주체 하나만** 인클러스터 대역(`deploy/e2e-anthropic-fake.yaml`)으로 바꾸고 나머지는 전부 실물이다. 요청은 주 컨테이너가 오케스트레이터가 주입한 loopback `ANTHROPIC_BASE_URL`로 보내고, credential-proxy 사이드카가 **프로덕션 코드 그대로** 헤더 허용목록을 적용하고 호출자가 붙인 `Authorization`을 버린 뒤 자기 Secret 환경의 플랫폼 토큰을 주입하며, 대역의 인증서를 **실제로 검증한다** — 사설 발급자를 신뢰하는 근거는 플랫폼 Secret의 optional `ca-cert` 키 하나뿐이고 시스템 루트는 대체되지 않는다. 대역은 bearer가 틀리면 401을 내므로 주입이 실제로 일어났는지가 대역 쪽에서도 확인된다. `control-plane/test/e2e_provider_reachability_test.go`가 배포 SUT의 세션 pod 안에서 이 왕복을 **버퍼드·SSE 두 형태**로 단언하고, 위조 `Authorization`이 대체되는 것까지 확인한다. | **실 provider의 계약은 미검증**이다 — 스트리밍 타이밍·오류 응답·토큰 회계·모델 라우팅은 대역이 결정적 상수로 답하므로 드러나지 않는다. 특히 **이미지에 핀된 실 Claude CLI(2.1.220)의 프롬프트 루프가 이 표면으로 만족되는지는 이 등재가 단언하지 않는다** — 그 프로토콜의 정본은 이 레포 소스에 없다(외부 바이너리). AC-E2~E6 전용 파일을 세우는 슬라이스가 그 계약을 먼저 실물로 확인해야 하고, 그때 이 행의 '구동 구간'이 넓어진다. |
<!-- /fidelity:registry -->

### 미해소 위반 (승인된 예외가 아니다)

여기 있는 치환은 **정책 위반이며 제거 대상**이다. 등재의 목적은 정당화가 아니라 회계다 —
개수에 상한이 걸려 있어(`상한` 집계) 새 위반이 들어오면 CI가 막고, 줄어들 때만 상한을 함께
내린다.

남은 한 파일은 `page` 라우팅으로 **control-plane API 표면 전체**(`**/api/v1/**`)를 가로채 세션
목록·단건·SSE 스트림·config를 손으로 지은 픽스처로 응답한다. 그중 일부(config 실패, 응답 보류로
만드는 중간 UI 상태, past-end cursor `reset` 이벤트)는 `NET` 자격을 만족하지만, 그것들이 **API
전체를 삼키는 인터셉트 안에** 들어 있어 행 단위로 승인할 수 없다. 실 SUT가 낼 수 있는
데이터(세션 목록·상태·pod)까지 함께 위조되기 때문이다.

제거가 한 슬라이스에서 끝나지 않은 이유는 두 스위트가 모두 **claude-code 세션**을 검증 대상으로
삼기 때문이었다. 그 선행은 2026-09-04에 **모두 풀렸다** — 세션 pod가 SUT에서 서고(`PLUGIN-CRED`
등재 + 인클러스터 마켓플레이스 원격), 프롬프트에 답하는 provider도 인클러스터에 있다
(`CLAUDE-PROVIDER` 등재). 그래서 아래 「차단 요인」 표는 비었다.

**남은 것은 선행이 아니라 재작성 노동이고, 래칫은 실제로 줄였을 때만 내린다** — "곧 줄어든다"로
상한을 미리 내리면 그때부터 게이트가 붉은 채로 산다. 그래서 상한은 위반을 **실제로 지운 그
커밋에서만** 함께 내려간다(6 → 4 → 2).

**j6가 남은 이유는 노동량이 아니라 설계 논점이다.** `installAgentApi`가 덮는 12개 테스트 중 절반은
`/api/v1/config`가 **테스트마다 다른 모델 카탈로그**(2개짜리 목록 · 빈 목록 · 503)를 내야 성립한다.
배포된 SUT 하나는 그 세 상태를 동시에 낼 수 없으므로, 실패·지연만 좁혀 `NET`으로 등재하는 것으로는
닫히지 않고 **파일을 나누는 설계**가 먼저다. 그 판단이 서기 전까지 두 행은 여기 남는다.

*해소된 것*: ① `web/e2e/journeys/session-deletion.spec.ts`는 여기서 빠졌다. 세션 생성과 동결을 제품 API로
돌려 실 SUT 위에서 돌게 만들고, 남은 인터셉트를 첫 DELETE의 409 응답 하나로 좁혀
`DELETE-CONFLICT-ERR`(`NET`)로 **승인 등재**했다. 그만큼 상한도 6에서 4로 내려갔다.
② `web/e2e/journeys/manual-archive.spec.ts`는 2026-09-04에 빠졌다 — 이쪽은 **좁히지 않고 전량
제거**했다. claude-code 세션을 제품 API로 실제로 만들고(대상을 shell로 바꾸지 않았다),
`ws-archive-session` 클릭이 제품 아카이브 경로(`CLAUDE_CODE_ARCHIVE_ENABLED`는 base에서 이미 on,
`NewAgentArchiveCheckpointer`)를 타게 한 뒤, 얼어붙은 상태와 회수된 pod를 배포 SUT에 **다시 물어**
확인한다. 중간 `Archiving…` 상태도 지연 주입 없이 관측된다 — 실 아카이브가 워크스페이스를 MinIO로
올리고 pod를 회수한 **뒤에야** 응답하기 때문이다. 예고돼 있던 `NET`(LAT) 등재는 **쓰지 않았다**:
정책이 "예외는 늘지 않는 방향으로만"이라 등재 seam을 늘리지 않고 닫는 쪽이 낫다. 상한은 4에서 2로
내려갔다.

<!-- fidelity:violations -->
| 파일 | 토큰 | 무엇을 위조하는가 | 제거 경로 (선결조건) |
| --- | --- | --- | --- |
| `web/e2e/journeys/j6-agent-prompt-loop.spec.ts` | `.route(` | `installAgentApi`가 12개 테스트 전부에 `/api/v1/**` 핸들러를 설치해 세션·config·SSE 스트림을 위조한다. | **선결조건은 없다** — 세션 pod가 SUT에서 서고(`PLUGIN-CRED`), 프롬프트에 답하는 provider도 인클러스터에 있다(`CLAUDE-PROVIDER`). 남은 것은 재작성 노동뿐이다: **claude-code 세션을 대상으로 유지한 채** 실 SUT 위에서 다시 쓴다. 재작성에 앞서 실 Claude CLI가 그 대역 표면으로 실제 프롬프트 루프를 도는지부터 확인해야 한다 — `CLAUDE-PROVIDER` 등재 행이 그 계약을 미검증으로 남겨 두었다. |
| `web/e2e/journeys/j6-agent-prompt-loop.spec.ts` | `route.fulfill(` | 위와 같은 핸들러의 응답 생성부 — JSON·`text/event-stream` 본문을 직접 짓는다. | 위와 같다. 재작성 뒤에도 실 SUT가 요청 시점에 낼 수 없는 주입(config 503, 응답 보류, past-end cursor `reset` 재생)은 남으므로, 그것들은 `session-deletion` 선례대로 **행 단위로 좁혀 `NET` 승인 등재**로 닫는다 — 전량 제거가 목표가 아니다. |
<!-- /fidelity:violations -->

### 차단 요인 (seam이 아니다 — 치환조차 없는 공백의 원인)

위 「미해소 위반」이 *치환이 있어서* 생긴 drift라면, 여기 있는 것은 *치환조차 없어서* e2e가 그
경로를 **아예 밟지 못하는** 이유다. claude-code 워크로드 AC(AC-E1~E6)가 「AC 검증 범위」에서
통째로 공백이던 근거가 이 표였다 — 세 행이 차례로 나가면서 AC-E1이 먼저 공백을 벗었고, 남은
E2~E6은 이제 **전용 파일 저작**만 남아 있다(원인이 아니라 순서다). seam이 아니므로 코드에
`mock-exception:` 마커도, 스캔 토큰도 남지
않는다 — R5는 이것들에 대해 영원히 침묵한다. 그래서 리터럴 단위로 대조하는 R10을 따로 둔다:
원인이 고쳐지면 선언한 리터럴이 사라져 게이트가 빨개지고, 그때 '해소 시' 칸이 예고한 등재 또는
실배포를 반드시 함께 반영해야 한다.

**원장은 종착지가 아니라 경유지다.** 판정 기준은 정의의 `EXT` 배제 조항 하나다: *"kind에
실물로 배포 가능하면 EXT를 쓸 수 없다"*(MinIO 선례). 그래서 같은 부트스트랩 안에서도 **자격
발급**과 **호스팅**의 판정이 갈리고, 판정에 따라 나가는 문이 다르다 — `EXT`면 대역을 세워
**등재 표로**, `없음`이면 실물을 배포해 **등재 없이** 나간다. 어느 쪽이든 여기 남아 있는 동안은
아직 seam이 아니다.

*나간 행*: ①**K3s MCP 자격 발급**은 대역을 세워 `PLUGIN-CRED`(EXT)로 **등재**했고,
②**마켓플레이스 호스팅**은 인클러스터 git 원격(`deploy/plugin-marketplace-git.yaml`)을 실물로
배포해 **등재 없이** 해소했다. 둘이 함께 풀리며 claude-code 세션 pod가 처음으로 Ready에 이르고,
AC-E1이 공백에서 전용 파일을 갖게 됐다. ③**제공자 도달**은 2026-09-04에 인클러스터 대역
(`deploy/e2e-anthropic-fake.yaml`)을 세워 `CLAUDE-PROVIDER`(EXT)로 **등재**하며 나갔다 — 판정이
예고한 그대로다. 그 해소에는 딸린 조건이 하나 있었다: 프록시가 upstream 스킴을 **https로
강제**하므로(`parseCredentialProxyUpstream`) 대역을 세우는 것만으로는 부족했고, 사설 발급자를
신뢰할 길이 필요했다. 그 길은 플랫폼 Secret의 **optional `ca-cert` 키**로 냈다 — `k3s-mcp-url`과
같은 형태의 주소류 optional 키이고, 키가 없으면 프록시는 시스템 풀 그대로라 `k8s/` base는 한 줄도
바뀌지 않는다. **https 요구를 낮추는 길은 택하지 않았다**: 프로덕션 보안 게이트를 테스트 편의로
내리는 것이라 해소가 아니라 새 위반이 됐을 것이다.

**표는 지금 비어 있다.** 비어 있다는 것이 이 표의 목적이 달성됐다는 뜻은 아니다 — 새로 발견되는
"치환조차 없는 공백"의 원인은 여기에 다시 쌓인다.

<!-- fidelity:blockers -->
| 무엇이 막는가 | 코드 위치 | 리터럴 | 판정 | 해소 시 |
| --- | --- | --- | --- | --- |
<!-- /fidelity:blockers -->

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
| `web/e2e/journeys/session-deletion.spec.ts` | `.route(` | `DELETE-CONFLICT-ERR` | 스냅샷 삭제 테스트가 첫 DELETE만 가로채려고 그 세션 URL에 거는 핸들러. |
| `web/e2e/journeys/session-deletion.spec.ts` | `route.fulfill(` | `DELETE-CONFLICT-ERR` | 주입되는 단 하나의 응답 — 409 `session state changed concurrently`(실서버 메시지와 동일). |
| `web/e2e/journeys/session-deletion.spec.ts` | `route.continue(` | `DELETE-CONFLICT-ERR` | 같은 핸들러의 통과 분기. 단건 GET도, **재시도 DELETE도** 실 control-plane이 응답하게 한다 — 이 줄이 없으면 승인 범위가 409 하나를 넘어선다. |
| `deploy/e2e-k3s-mcp-fake.yaml` | `test-only` | `PLUGIN-CRED` | 조직 K3s MCP 대역. 자격 발급만 흉내 내는 치환이라 MinIO(실물 S3 구현)와 달리 승인된 모킹 예외로 등재한다. |
| `deploy/e2e-anthropic-fake.yaml` | `test-only` | `CLAUDE-PROVIDER` | 인클러스터 provider 대역. MinIO가 S3의 **실물 구현**이라 EXT를 못 쓰는 것과 반대로, 여기에는 세울 실물이 없다 — 그 서비스의 본체가 모델 자체다. 그래서 승인된 모킹 예외로 등재한다. TLS는 진짜다(사설 CA 서명): 프록시가 https를 강제하므로 대역도 인증서를 내야 하고, 그 덕분에 검증 경로가 실물로 남는다. |
| `deploy/plugin-marketplace-git.yaml` | `test-only` | `—` | 인클러스터 git 원격. MinIO와 같은 성격의 **실물 배포**라 치환이 아니다 — `claude plugin marketplace add` 가 프로덕션과 같은 코드 경로로 클론한다. 담기는 마켓플레이스 문서는 픽스처 데이터이고, 정의상 시드·fixture 는 모킹이 아니다. |
| `.github/workflows/e2e.yml` | `test-only` | `—` | CRIU 프로브 주석이 "이제 test 전용 트리거가 없다"는 사실을 밝히는 서술. 치환이 아니다. |
| `Makefile` | `CRIU_ENABLED` | `—` | `make test-integration` 설명 주석. 인프로세스 통합 하네스(`//go:build integration`)는 e2e SUT 경로가 아니라 이 정책의 범위 밖이다. |
| `control-plane/test/harness_shared_test.go` | `E2E_SESSION_NAMESPACE` | `—` | 어서션용 kube 클라이언트가 볼 namespace 지정(기본 `default`). `E2E_BASE_URL`과 같은 성격의 배선이라 SUT 충실도를 낮추지 않는다. 지문에서 아예 빼는 편이 맞지만 그것은 정합성 모델 definition 개정 사안이라, 여기서는 비-seam으로 회계만 맞춘다. |
| `deploy/minio.yaml` | `test-only` | `—` | MinIO가 test 전용 저장소가 **아님**을 밝히는 서술이다 — 프로덕션과 같은 S3 코드 경로를 타는 실 배포라 `EXT` 대상이 아니다. |
| `web/e2e/journeys/deferred.spec.ts` | `test-only` | `—` | deferred skip 사유 산문. 인터셉트가 아니다. |
<!-- /fidelity:ledger -->

### 집계

<!-- fidelity:summary -->
- 등재 seam **4**개 — GATE **1** · TRIG **0** · EXT **2** · NET **1**
- 코드 마커 지점 **6** / 마커 파일 **6**
- 지문 회계 행 **17** — 등재 귀속 9 · 미해소 위반 **2** · 비-seam **6**
- web e2e 인터셉트 **5**건 (승인 3 · 위반 2, 상한 **2**)
- 차단 요인 **0**건 (seam이 아니다 — 셋 다 등재 또는 실배포로 나갔다)
<!-- /fidelity:summary -->

이 숫자들도 체커가 실제와 대조한다(R8) — 표만 고치고 집계를 잊으면 실패한다. 상한을 넘는
인터셉트가 들어오면 R9가 막는다.

> **AC 검증 범위**: 바로 위 「집계」는 **모킹 충실도**의 것이고, AC ↔ e2e 1:1 커버리지는
> 별개다. 그쪽 정본은 아래 「AC ↔ e2e 파일 매핑」·「AC 예외 목록」과, 그 뒤 「집계」의
> `ac-summary` 블록(총계 · 예외 · AC 매칭 파일 · 공백 목록)이다.
>
> **그 숫자를 여기로 옮겨 적지 말 것.** `scripts/e2e/check-ac-mapping.sh`는 `ac-summary`
> 마커 블록만 파싱하므로, 블록 **밖**에 복사된 집계는 얼마나 틀려도 CI가 잡지 못한다 —
> 규칙 6이 요구하는 일치는 그 사본에는 미치지 않는다. 실제로 #23이 여기 남긴 `AC 21개`
> 사본은 #46이 AC를 21 → 27로 늘린 뒤에도 게이트 초록 아래에서 낡은 채로 남아 있었다.
>
> 남은 미검증 분기는 `idle` 상태에 도달할 방법이 없어서 생긴 것뿐이며(read/write/switch의
> idle 경로), AC-B1의 트리거 정책과 함께 풀린다 — §「남은 미검증 분기」 참고.

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
| AC-E1 | `control-plane/test/e2e_e1_workload_type_test.go` | go | `workloadType=claude-code` 세션이 SUT에서 서고 그 pod에서 Claude CLI가 실제로 실행됨 / 필드 생략은 shell / 잘못된 값은 pod 생성 전 400 / 타입·모델은 생성 후 불변 |
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
| `control-plane/test/e2e_provider_reachability_test.go` | go | 배포 SUT의 claude-code 세션 pod가 credential-proxy를 거쳐 인클러스터 provider에 도달하는지 — 버퍼드·SSE 두 응답 형태와, 호출자가 붙인 `Authorization`이 플랫폼 토큰으로 대체되는 것 |
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
- AC 총계: 27
- 예외: 1
- AC 매칭 파일: 15
- 공백: 11 — AC-E2 AC-E3 AC-E4 AC-E5 AC-E6 AC-F1 AC-F2 AC-F3 AC-F4 AC-F5 AC-F6
<!-- ac-summary:end -->

**공백**은 전용 파일도 예외 등재도 아직 없는 AC다. AC-E2~E6(claude-code 워크로드)와
AC-F1~F6(approval-gated 워크로드)가 여기 남는다.

> **AC-E1은 2026-09-04에 공백에서 나왔다.** 이 AC를 막고 있던 것은 구현이 아니라 **SUT에서
> claude-code 세션이 서지 않는다**는 사실이었다 — pod가 agent를 띄우기 전에 조직 K3s MCP와
> 비공개 마켓플레이스에 도달해야 했고 kind 안에서는 둘 다 닿지 않아 컨테이너가 죽었다.
> 오버레이가 그 둘을 인클러스터로 세우면서(하나는 `PLUGIN-CRED`로 등재한 대역, 하나는 실물
> git 원격) 세션이 서고, AC-E1의 검증 방법 네 갈래가 모두 배포 SUT 위에서 단언 가능해졌다.
> **나머지 E 계열(E2~E6)의 선행도 같은 날 풀렸다.** 필요한 것은 **프롬프트에 대한 응답**이었고,
> 인클러스터 provider 대역을 세워 `CLAUDE-PROVIDER`(EXT)로 등재하면서 「차단 요인」 표가 비었다.
> 그래서 E2~E6이 아직 공백인 이유는 이제 **원인이 아니라 순서**다 — 전용 파일을 저작하는 일이
> 남아 있고, 그 슬라이스는 먼저 실 Claude CLI가 그 대역으로 프롬프트 루프를 도는지 확인해야 한다
> (등재 행이 그 계약을 미검증으로 남겨 두었다). 그때까지는 j6의 인터셉트가 「미해소 위반」에
> 그대로 남는다 — `manual-archive`는 2026-09-04에 실 SUT 재작성으로 거기서 빠졌다.

> **왜 이번에도 E 계열을 범위 밖에 두는가** *(2026-08-30 재판정)*. 원래 근거였던 "구현 전무"는
> #24 *Implement Claude Code workload end to end* 로 **더 이상 성립하지 않는다**. 그럼에도 범위
> 밖으로 두는 이유는 둘이다. ① `web/e2e/journeys/j6-agent-prompt-loop.spec.ts`가 E1~E6를 **한
> 파일에서 묶어** 검증해(654줄), 규칙 2(파일당 AC 1개)를 만족시키려면 AC별 6분할이라는 별도
> 슬라이스가 필요하다. ② j6와 `manual-archive`는 위 「미해소 위반」 원장에 올라 있는
> **승인되지 않은 네트워크 인터셉트**를 쓴다 — 지금 이들을 AC 전용 파일로 승격하면 "모킹으로
> 검증된 AC"를 1:1 정본에 박아 넣게 된다. 실 SUT 전환이 먼저이고 그 작업은 이미 진행 중이다
> (`manual-archive`는 PR #42). 그 전환이 끝난 뒤 E 계열 전용 파일을 신설하거나, 외부 LLM
> 자격증명·비결정 산출물처럼 곤란한 것은 예외로 등재해 이 표를 0으로 만든다.

> **왜 F 계열은 「예외」가 아니라 「공백」인가** *(2026-09-03 판정 · **2026-09-04 선행 재판정**)*.
> #46이 `approval-gated` 워크로드 타입을 문서로 신설하며 AC가 **21 → 27**로 늘었다(AC-F1~F6).
> 여섯 건 모두 전용 파일도 예외 등재도 없이 공백에 놓는다. 두 갈래를 다 검토했고 **지금은 어느
> 쪽도 성립하지 않는다**.
>
> - **전용 파일 신설 불가 — e2e SUT에서 이 타입의 세션이 아직 서지 않는다.** 최초 판정의 근거는
>   "구현이 0줄"이었고 그것은 **더 이상 사실이 아니다**: #48이 pod 참조를 집합으로 바꿨고, 이어진
>   슬라이스가 타입 축(AC-F1의 제어 평면 절반)과 **헬퍼 파드 쌍 프로비저닝·컨테이너별 자격 증명
>   분리**(AC-F4/F6의 pod 스펙 절반)를 착지시켰다.
>
>   **✅ 2026-09-03 판정이 「유일한 선행」으로 지목했던 「헬퍼 파드의 세션 MCP 워크로드가 data plane
>   에이전트에 아직 없다」는 해소됐다** — `data-plane/cmd/agent/main.go`에
>   `workloadSessionMCP = "mcp"` 상수가 있고 `main`의 `case workloadSessionMCP:`가 그 워크로드를
>   실제로 서브한다(#60이 헬퍼 파드
>   쌍을 기동시키고, #64가 그 MCP 컨테이너에 도구 표면과 승인 게이트웨이 왕복을 넣었다).
>   **그 문장을 근거로 남겨 두면 이 원장은 「선행이 풀렸으니 지금 저작해도 된다」고 읽힌다** — 그러나
>   세션은 여전히 서지 않는다. 실제로 남아 있는 선행은 아래 둘이고, 둘 다 **SUT 오버레이 쪽**이다.
>
>   **선행 ⓐ — `DATA_PLANE_APPROVAL_GATED_IMAGE`가 어디에도 설정돼 있지 않다.** base
>   `k8s/deployment.yaml`이 선언하는 이미지 env는 `DATA_PLANE_IMAGE`(`:52`)와
>   `DATA_PLANE_CLAUDE_CODE_IMAGE`(`:62`) **둘뿐**이고 이 env는 **선언 자체가 없다**. e2e 오버레이
>   `deploy/kustomization.yaml`의 control-plane patch도 `env/1`·`env/2`·`env/3`만 replace한다.
>   따라서 제어면은 `control-plane/cmd/control-plane/main.go:268`의 기본값 `""`를 쓰고,
>   `WithWorkloadImage`가 빈 값을 no-op으로 버리며
>   (`control-plane/internal/adapter/k8s/client_orchestrator.go:264-274`) `imageFor`가 shell 아닌
>   미설정 타입에 에러를 돌린다(`:555-563`). 즉 `workloadType=approval-gated` 생성 요청은 **타입
>   검증을 통과한 뒤**(400이 아니다) **프로비저닝에서 500으로 실패하고, 파드가 하나도 서지 않는다.**
>
>   **선행 ⓑ — 게이트웨이 Secret이 없고, 그 참조는 optional이 아니다.**
>   `git grep 'approval-gateway' -- deploy k8s`가 **0 hits**다 — `approval-gateway-credentials`
>   매니페스트가 오버레이에도 base에도 없다. 그런데 헬퍼 파드의 MCP 컨테이너는 게이트웨이 3종을
>   **`secretEnv`**로 투영한다(`client_orchestrator.go:780-782`, 정의 `:887-895`). 이 자리는
>   `Optional`을 세우지 않으므로(세우는 쪽은 같은 파일의 `optionalSecretEnv` `:897-902`이고 여기
>   쓰이지 않았다) Secret이 없으면 kubelet이 컨테이너 구성 단계에서 실패해 **헬퍼 파드가 Ready에
>   이르지 못한다.**
>   ⚠️ `doc-tracker.md`의 「게이트웨이 3종 env가 없으면 컨테이너는 뜨되 **도구 목록이 빈다**」와
>   겹쳐 읽어 "이미지만 채우면 선다"로 결론 내지 말 것 — 그 관용은 **agent 프로세스 층위**의 것이고
>   (`data-plane/cmd/agent/main.go`의 `case workloadSessionMCP:`), **필수 SecretKeyRef가 걸린 이
>   배포에서는 프로세스가
>   시작조차 못 해 그 경로에 도달하지 않는다.**
>
>   📌 같은 낡음이 `control-plane/cmd/control-plane/main.go:79-82`의 Go 주석에도 있다("its data
>   plane runtime — the helper pod's session MCP — is not implemented yet"). **코드 주석은 이 렌즈의
>   산출물이 아니라 손대지 않았다** — 지목만 남긴다(소관: `tbm_session-platform-docs-impl` ·
>   `tbm_session-platform-comment-redundancy`).
>
>   현재 F 계열의 절반을 지키는 것은 `control-plane/test/approval_gated_orchestrator_test.go`
>   (fake clientset · pod 스펙 단언)와 `internal/api/workload_type_test.go`(타입 허용값·400 거부)다
>   — e2e가 아니므로 이 표의 매칭 파일로는 세지 않는다.
> - **규칙 4 예외 등재도 부적격 — 예외로 올릴 이유가 아니라 순서를 기다리는 것이다.** 예외는 사유와
>   함께 **대체 검증 수단**을 요구하는데(AC-B1이 `reaper_test.go`를 갖는 것처럼), F 계열의 대체
>   수단은 아직 고르지 않다 — *2026-09-04 갱신*: 승인 게이트는 `data-plane/cmd/agent/`의
>   `approval_gateway_test.go`·`session_mcp_gate_test.go`·`session_mcp_test.go`·
>   `approval_gated_test.go`가, egress 정책 **오브젝트**(존재·셀렉터·동반 회수)는
>   `control-plane/test/approval_gated_network_policy_test.go`가 덮게 됐다(#60·#64). 그러나
>   **AC-F5의 공유 볼륨은 구현 자체가 없고**
>   (`control-plane/internal/adapter/k8s/client_orchestrator.go:685`), 정책의 **실집행**은 여전히
>   어느 테스트도 보지 않는다. `approval-gated-workload.md`가 AC-F2·F4를 "코드 단언이
>   아니라 **실클러스터 확인**"으로 적은 것은 사실이지만, 그 문장이 가리키는 것은 참조 구현
>   (`dlddu/pure-agent`)이 **클러스터에 배선된 적 없다는 미검증 전제**이지 "자동화가 원리적으로
>   곤란하다"가 아니다. 지금 예외로 올리면 구현이 착지한 뒤에도 1:1 원장에서 **영구 면제**되는 거짓
>   예외가 박힌다. 공백은 원장에 보이고 집계가 세지만, 예외는 보이지 않는다.
>
> **승격 조건(다음 감지가 이 논점을 다시 열지 않도록 못박는다)** *(2026-09-04 재작성 — 해제 조건을
> 위 선행 ⓐ·ⓑ 기준으로 옮긴다. 이전 판은 「data plane MCP 워크로드」 하나를 걸었고 그것은 이미
> 발화했다)*.
> ① **AC-F1·F3·F5·F6** → 전용 파일 신설. **해제 조건은 ⓐ와 ⓑ가 함께 충족되는 것이다** — SUT
> 오버레이가 `DATA_PLANE_APPROVAL_GATED_IMAGE`를 채우고, 같은 네임스페이스에
> `approval-gateway-credentials`(`url`·`api-key`·`user-id`)가 있어 헬퍼 파드가 Ready에 이르는 것.
> 그 둘이 서면 F1(타입 허용값·400 거부·불변성)은 AC-E1과 같은 모양이라 Go 스위트이고,
> F3(승인 대기 중 write 즉시 반환·in-band 마커·`lastAccess` 갱신)·F5(공유 볼륨 왕복)·
> F6(컨테이너별 Secret 분리)도 API·pod 스펙 단언이므로 Go 스위트다. **이 해제는 e2e 하네스
> (오버레이) 작업이라 이 렌즈의 산출물이 아니다** — 이 루프가 내는 것은 e2e 파일과 등재 문서뿐이다.
>
> ⚠️ **AC-F1을 「400 거부 갈래만」으로 먼저 끊지 말 것.** 타입 검증은 pod 생성 전에 끝나므로 그
> 갈래만은 이미지 없이도 관측된다 — 그래서 매번 "F1만은 지금 되지 않나"라는 물음이 다시 올라온다.
> 답은 아니오다: AC-F1의 검증 방법(`../prd/approval-gated-workload.md`)은 **첫 갈래가 「세션을
> 생성하면 워크로드 파드와 세션 전용 헬퍼 파드(컨테이너 둘)가 함께 기동되고 세션 조회 응답의 타입이
> `approval-gated`임을 확인한다」**이고, 그것이 정확히 ⓐ·ⓑ에 막힌다. 반쪽만 단언하는 파일은 규칙 1이
> 요구하는 **그 AC의 주검증**이 아니다. 선례가 같은 말을 한다 —
> `control-plane/test/e2e_e1_workload_type_test.go`는 헤더에 "네 갈래를 **전부** 단언한다"고 적고
> 실제로 파드 기동 갈래를 갖는데, 그 갈래는 부트스트랩 두 대역이 선 뒤에야(#62) 도달 가능해졌다.
>
> ⚠️ **AC-F5에는 선행이 하나 더 있다.** 공유 RWX 볼륨은 아직 구현되지 않았다
> (`control-plane/internal/adapter/k8s/client_orchestrator.go:685`, `../doc-tracker.md`의 같은 항목).
> ⓐ·ⓑ가 풀려도 F5 전용 파일은 그 구현이 착지한 뒤다 — 구현은 `tbm_session-platform-docs-impl`의
> 몫이고, 그때까지 F5는 규칙 7의 성격(순서를 기다리는 공백)을 그대로 갖는다.
> ② **AC-F2·F4** → 그때 비로소 규칙 4 예외의 근거가 선다. F2(egress 차단)는 **현재 e2e 하네스에서
> 검증할 수 없다** — `deploy/kind-config.yaml`이 `disableDefaultCNI`를 켜지 않아 클러스터가 기본
> kindnet으로 뜨고, kindnet은 **NetworkPolicy를 집행하지 않는다**. 따라서 정책 오브젝트의 존재·셀렉터·
> 동반 삭제까지는 Go e2e로 단언할 수 있어도 "실제로 막히는가"는 정책 집행 CNI가 있는 오버레이나 실
> 클러스터가 선결이다(`approval-gated-workload.md` 시나리오 2의 사전 조건이 같은 말을 한다). F4는
> 두 파드의 동반 회수라 `e2e_a3_pod_reclaim_test.go`와 같은 모양으로 자동화 가능하므로, **예외가 아니라
> 전용 파일**이 기본값이고 실클러스터 항목(PID 네임스페이스 분리 등)만 남으면 그때 갈라 등재한다.

> **AC-A2/A3 개정이 기존 전용 파일을 무효화하지 않는다** *(2026-09-03 판정)*. #46은 AC-A2의 제목을
> "세션당 전용 Pod" → "세션당 전용 **워크로드** Pod"로 좁히고 **보조 파드** 조항을 신설했으며, AC-A3의
> 검증 방법을 "해당 세션의 data plane pod(워크로드 파드와, **있다면 보조 파드**)"로 넓혔다. 게이트는 AC
> 식별자만 대조하므로 이 종류의 어긋남을 잡아 주지 않는다 — 그래서 여기에 판정을 남긴다.
>
> **결론: 두 파일은 개정된 문구 기준으로도 그대로 유효하며, 이번에 코드는 한 줄도 고치지 않았다.**
> 근거는 두 파일이 pod를 **라벨로 목록 조회하지 않는다**는 점이다. `e2e_a2_dedicated_pod_test.go`는 생성
> 응답이 돌려준 `s.Pod` **이름 하나**를 확인하고 고유성도 `s.Pod` 집합으로 세며, `e2e_a3_pod_reclaim_test.go`도
> `s.Pod` 하나의 삭제만 단언한다. 즉 둘 다 처음부터 "그 세션의 **워크로드** 파드"만 보고 있었고, 이는 개정된
> A2가 1:1을 요구하는 바로 그 대상이다. 두 파일이 만드는 세션은 `shell`이고 그 타입은 보조 파드를
> 만들지 않으므로, A3의 "있다면" 절은 이 두 파일에서 여전히 공허참이다.
>
> **갱신 (구현 착지 후)**: 보조 파드를 만드는 타입은 이제 코드에 **있다** — `approval-gated`가 세션마다
> 헬퍼 파드를 함께 띄운다(AC-F4의 프로비저닝 절반). 다만 그 타입의 세션은 배포된 SUT에서 아직 서지
> 않으므로(위 F 계열 판정 참고) 두 파일이 지금 보는 것은 변함없이 `shell` 세션 하나뿐이다. 착지한 코드는
> **역할 라벨**(`session-platform.dev/pod-role: workload|helper`)을 모든 세션 파드에 붙여, 이후 라벨 기반
> 조회를 쓰는 e2e가 보조 파드를 워크로드 파드로 세지 않도록 셀렉터를 좁힐 수단을 미리 마련해 두었다.
>
> **재검토 트리거**: `approval-gated` 세션이 SUT에서 실제로 서는 순간 둘 다 손대야 한다 —
> A3는 **두 파드의 동반 회수**를 단언해야 하고(그 단언의 소유자는 AC-F4 전용 파일이 될 수도 있다), A2는
> 라벨 기반 조회를 쓰게 되면 위 역할 라벨로 셀렉터를 좁혀야 한다. 그때까지는 개정 전 형태 그대로가 옳다.

## 남은 미검증 분기 (공백은 아님)

전용 파일은 있으나 그 안에서 아직 단언하지 못하는 경로. 전부 **`idle` 상태에 도달할 방법이
없다**는 한 가지 이유이며, AC-B1의 트리거 정책과 함께 풀린다.

| 경로 | 소유 파일 | 막힌 이유 |
| --- | --- | --- |
| read `idle->active->read` | `e2e_c2_read_branches_test.go` | idle 진입 트리거 없음(AC-B1 정책 미확정) |
| write `idle->active->write` | `e2e_c3_write_branches_test.go` | 〃 |
| switch의 idle 대상 승격 | `e2e_c4_session_switch_test.go` | 〃 |
