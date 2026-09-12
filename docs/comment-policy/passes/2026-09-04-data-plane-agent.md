# 2026-09-04 — `data-plane/cmd/agent/` (판정 976줄)

컨트롤 플레인 세 패스에 이어 **데이터 플레인 전체**. 32파일을 통째로 읽었다 — 소스 12개(729줄) ·
테스트 20개(247줄). 패키지 하나(`package main`)라서 파일 하나만 집으면 「지운 쪽의 사본」을
소유하지 못하는 것은 3차 패스와 같고, 여기서는 그 되풀이가 **소스↔테스트** 사이에도 있었다.

**왜 제거율이 24%인가**: 앞선 세 패스(12% · 25% · 32%)와 같은 대역이다. 다만 분포가 극단적으로
치우쳤다 — `main.go` 하나가 제거 237줄 중 **92줄**(그 파일의 50%)이고, 반대로
`claude_archive.go` · `credential_proxy_stream.go` · `claude_process_linux.go` ·
`claude_stream_contract_test.go`는 **한 줄도 지우지 않았다.** 아카이브 안전성·tail-safe redaction·
`/proc` 파싱처럼 코드 형태로 보이지 않는 지식만 적혀 있어서, 이 패스는 「어느 파일이 살쪘는가」가
아니라 **「어느 파일이 다른 곳을 베꼈는가」**를 골라냈다.

**제거 237줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `main.go` 패키지 주석 (33→5) | 다섯 워크로드 모드의 AC 서술 전부 — shell의 write/read/CRIU(AC-D1~D4) · claude-code의 원샷·직렬 큐·비-CRIU 아카이브(AC-E2~E5) · approval-gated의 「plugin 없음 · K3s MCP 토큰 없음 · 프록시가 파드 밖 · 세션 MCP가 유일한 표면」(AC-F6) · mcp 컨테이너 서술 · credential-proxy의 두 배치 | ② PRD 다섯 절이 전부 축자에 가깝다 · ① **같은 파일의 `switch workload`**가 다섯 모드와 기본값을 그대로 말한다 · ① `session_mcp.go` 헤더가 mcp 문단을, `credential_proxy.go`의 `credentialProxyPlacement`가 배치 문단을, 같은 파일의 `agentToolSurface`가 approval-gated 문단을 각각 다시 말한다 |
| `main.go` `defaultShellPIDFloor`(8→3) · `nsLastPIDPath`(3→0) | 「CRIU는 체크포인트 당시 pid로 복원한다 → 셸이 pid ~10에 앉아 복원 pod의 에이전트 스레드(tids ≤ ~15)와 충돌(2026-07-23 관측)」 · 「privileged pod에서만 쓰기 가능」 | ② `docs/criu-verification.md` **5차 (2026-07-23, pid 충돌)** 가 그 서술과 기본값 300·`CRIU_PID_FLOOR`·privileged 전제까지 그대로 적는다. **주석이 이미 날짜로 그 라운드를 자백하고 있었다** — 포인터만 남겼다 · ① `reserveShellPID` doc이 `ns_last_pid` 의미를 정본으로 적는다 |
| `main.go` `scrollback` doc (14→6) | 「CRIU는 셸 트리를 뜨고 에이전트를 뜨지 않으므로 scrollback이 criu 이미지에 실리지 않는다 → /checkpoint가 따로 직렬화하고 /restore가 preload한다」 · 「복원 전 커서가 그대로 유효하다」 | ① `checkpoint.go` 파일 헤더 2문단이 **축자로** 같은 말을 한다(그쪽을 정본으로 남기고 여기서 가리킨다) · ② `prd/shell-workload.md` AC-D3의 「설계 노트 (offset과 복원)」가 커서 유효성을 그대로 적는다 |
| `main.go` `checkpointing` 필드 (6→2) | criu dump가 셸을 죽여 exit watch가 `os.Exit(1)`로 아카이브를 자른다는 서술 | ① `checkpoint.go` `handleCheckpoint`의 인라인 5줄이 **실패 분기(재무장)까지 포함해** 같은 말을 한다 |
| `main.go` 라우트 주석 4곳 (12→6) | `/write`의 「본문이 PTY로 그대로 · 즉시 반환」 · `/read`의 「비파괴적」 · `/attach`의 「프레임을 버리며 붙잡고 있다」 · healthz의 세 분기 | ② AC-D2·AC-D3·AC-C3의 비블로킹 규약 · ① 각 핸들러 본문이 그대로 보여 준다. 포인터(`AC-D…`)만 남겼다 |
| `main.go` 인라인 3곳 (7→0) | 「Restore mode: no shell yet」 · 「Push the next pid up …」 · 「Claude's projected output is valid UTF-8 …」 | ① 바로 다음 줄의 `logger.Info("… restore mode; awaiting checkpoint")` · ① `reserveShellPID`/`defaultShellPIDFloor` doc · ① **UTF-8 경계 문장은 레포에 세 벌 있었다** — `claude_stream.go`의 `utf8NormalizingWriter` doc(일반 진술)과 `output_stream.go`의 `streamChunk`(셸 PTY 폴백까지 더한다)를 남기고 여기만 지웠다 |
| `checkpoint.go` 파일 헤더 (18→9) | RUNTIME SEAM 문단 통째 | ① 같은 파일의 `criuEngine` doc(「tests inject a fake so the handlers and archive plumbing run without a CRIU runtime」)과 `execCriuEngine` doc(「RUNTIME SEAM — … never in CI」)이 **각각 다시** 말한다. 세 벌 중 둘을 남겼다 |
| `checkpoint.go` `execCriuEngine`(10→7) · `Restore` 인라인(8→6) | 「detached·sibling·pidfile로 바꾼 이유: criu 종료를 셸 종료로 오인해 컨테이너가 재시작했다(2026-07-23)」의 **두 벌** | ② `docs/criu-verification.md` 5차가 그 사건과 대응을 적는다 · 플래그 **의미**(criu 외부 지식)는 남기고 사건 서술만 지웠다 |
| `claude.go` `toolSurface`(6→3) · `Tools` 필드(3→1) · `sessionMCPPermission`(2→1) | 「approval-gated는 plugin 부트스트랩이 없다 — AC-F2 허용목록에 K3s MCP도 github.com도 없기 때문」 · 「두 타입이 갈리는 유일한 지점」 · 「AC-E2 목록에 정확히 이것만 더한다」 | ② `prd/approval-gated-workload.md` AC-F6의 ✅확정 문단이 축자다 · ① 바로 아래 `toolSurface` 타입 doc · ① `loadClaudeManagedSettings`의 인라인이 「AC-E2 목록에 대한 유일한 추가」를 정본으로 적는다 |
| `claude.go` `argv` 인라인(4→2) · resume 인라인(4→2) | 「partial stream-json을 runWorker가 투영한다」 · 「permission mode는 플랫폼 정책」 · 「실패한 첫 실행은 대화를 만들지 않는다」 | ① `claudeStreamProjector` doc · ② AC-E2의 ✅구현 결정(headless 권한 정책) · ② AC-E4 축자(「첫 실행 실패·timeout은 resume 상태를 만들지 않는다」). **순서 계약**(「idle 보고 전에 persist」)은 남겼다 |
| `claude.go` `beginCheckpoint` (3→1) | 「commitCheckpoint이 …」 | ① **그런 함수는 없다** — 실제 이름은 `completeCheckpointStream`이고 그 doc이 같은 계약을 적는다. 낡아 틀린 주석이라 제거 근거가 강해진다 |
| `credential_proxy.go` `credentialProxyPlacement`(6→3) · `validateApprovalGatedClientEnv`(6→3) · `validateHelperEndpoint`(5→4) | 「동작 계약은 AC-E6 하나, 배치만 다르다: claude-code는 loopback 사이드카, approval-gated는 헬퍼 파드」 · 검증 네 항목의 열거 · 「plain http · 지정 포트 · loopback 아님」 | ② AC-F6 축자(「프록시의 동작 계약은 AC-E6을 그대로 재사용한다 … 다른 것은 배치와 바인딩뿐이다」) · ① 바로 아래 본문의 네 `if` · ① 같은 함수 본문. **왜 loopback이 거부되는가**(파드 안으로 접힌 배치)와 **왜 타입인가**(사이드카까지 열리지 않게)는 남겼다 |
| `approval_gateway.go` 파일 헤더(10→7) · `externalID`(4→2) · `await` 2문단(4→1) · `create`(3→2) | wire 규약의 엔드포인트·헤더 열거 · 「세션 절반이 두 세션을, 요청 절반이 두 호출을 가른다」 · 「AC-E2 직렬 큐 안에서 사람이 걸리는 만큼 자리를 점유한다」 · 「승인 컨텍스트에 게이트웨이 키·공급자 토큰이 없다」 | ② `docs/doc-tracker.md`의 AC-F3·F5 항목이 **엔드포인트·헤더·외부 식별자 형태까지** 적는다 · ② AC-F3의 「유일성」·「대기 위치」 불릿이 축자 · ② AC-F3의 「승인 컨텍스트에 자격 증명이나 게이트웨이 API key가 실리지 않는다(AC-F6)」. 참조 구현의 **파일 경로**(`mcp-server/src/services/gatekeeper.ts`)는 어디에도 없어 남겼다 |
| `approval_gateway.go` `now`/`sleep` 필드 (2→0) | 「테스트가 시계를 주입한다」 | ① 3차 패스가 `WithAgentPort`·`WithReadiness`에서 지운 것과 **같은 형태**. 테스트가 그 자체로 보여 준다 |
| `session_mcp_notices.go` 파일 헤더 (17→9) | 「pull을 택한 이유: 워크로드→헬퍼:8092는 이미 egress 허용목록에 있어 정책을 하나도 안 건드리지만 push는 양방향 규칙을 요구한다」 · 「피드가 메모리뿐인 것은 AC-F4의 무상태 요구」 | ② `docs/doc-tracker.md`가 **그 두 문단을 거의 그대로** 적는다(「워크로드가 헬퍼를 당긴다(pull) … push는 양쪽에 새 규칙을 요구한다」, 「피드가 메모리뿐인 것도 의도다」) |
| `session_mcp_notice_tail.go` 헤더(11→6) · Dropped 인라인(3→0) | 커서 규약 열거(`id=nextOffset`, `{offset,payloadBase64,nextOffset}`, UTF-8 경계, reset 복구) · 「조용한 구멍보다 밝힌 구멍이 낫다」 | ② AC-F3의 「대기 표시」 불릿이 **그 괄호 목록까지 축자로** 적는다 · ① `noticeFeedResponse.Dropped` doc이 같은 말을 한다 |
| `session_mcp_tools.go` 헤더(9→6) · `sessionMCPConfig`(3→1) · `toolDefinitions`(4→2) · `callTool`(6→4) | 「게이트 없으면 도구 목록이 빈다」의 **네 벌** · 「거절은 transport 오류가 아니라 도구 결과다」 | ① `session_mcp.go` 헤더가 그 불변식을 「두 파일에 걸친다」고 선언하며 소유한다 — 그쪽 하나만 남겼다 · ① `mcpToolText`/`mcpToolError` doc이 `isError` 규약의 정본 |
| `session_mcp_tools.go` `maxFetchedBodyBytes` (4→2) | 「AC-F5 공유 볼륨이 착지할 때까지 본문을 자른다」 | ② `doc-tracker.md`의 AC-F5 항목이 「그때까지 승인된 호출의 결과 본문은 100 000바이트에서 잘라 도구 응답으로 돌려준다」를 그대로 적는다. 그 항목은 2026-09-07 (3)에서 **두 축으로 갈렸고**(볼륨 위상·수명은 착지, 그 볼륨에 쓰는 축은 미착지), 이 상수를 세우고 있는 것은 뒤 축이다 — 상수를 걷어낼 때 함께 움직여야 할 문장도 그쪽이다 |
| `session_mcp.go` `sessionMCPServerVersion` (3→1) | 「1이 된 것은 승인 게이트와 첫 외부 도구가 착지했을 때이고 0은 handshake만 되던 서버였다」 | ④ 커밋 이력이 그 전이를 갖는다 |
| 테스트 20개의 doc 46줄 | 소스 doc·인라인이 이미 단언하는 서술 — `TestShellPIDFloor`↔`shellPIDFloor` · `TestCheckpointDefersRestartWhileStreaming`↔`handleCheckpoint` 인라인 · `TestApprovalGatewayRefusesIncompleteConfiguration`↔`newApprovalGateway` · `TestApprovalGatewayFailuresAreErrorsNotDecisions`↔`do` · `TestUnknownNoticeKindsRenderNothing`↔`renderApprovalNotice` · `TestNoticeTailerSurvivesAnUnreachableFeed`↔`run` · `TestRestoredManagedSettingsArePointedAtTheCurrentSessionMCP`↔`ensureClaudeManagedSettings` · `TestProviderCAWidensTrustWithoutWeakeningIt`↔`trustProviderCA` · `session_mcp_test.go`의 게이트 불변식 **세 벌** · `credential_proxy_test.go`의 `Connection` 헤더 주석↔`Rewrite` 인라인 · `approval_gated_test.go`의 loopback 인라인↔`validateCredentialProxyBindAddr` | ① 전부 **같은 레포의 소스 doc**이 정본이다. 테스트 쪽은 「무엇을 재는가」를 가리키는 한 줄로 줄이고, **소스에 없는 것**(시나리오 번호·`httptest` 내부 동작·비결정적 타이밍 규약·명령 치환이 왜 강한 단언인지)은 남겼다 |

**유지 739줄** — 「지울까」를 검토했다가 남긴 것들.

- **아카이브 안전성 전부(`claude_archive.go` 18줄, 한 줄도 안 지웠다)** — 「연쇄 심링크 이스케이프는 lexical Clean이 놓친다」, 「디렉터리 권한은 자식을 만든 뒤에 적용한다」, 「이전 라이브 상태가 온전할 때 불변식을 검증한다 — workspace/home을 조용히 재생성하면 손상이 데이터 손실이 된다」. 코드 형태로 보이지 않고 어느 AC에도 없다.
- **`credential_proxy_stream.go` 10줄 · `claude_process_linux.go` 9줄(둘 다 0 제거)** — 「overflow·전송 오류에서 redactor를 Finish하지 말 것 — 보류된 suffix가 자격 증명의 일부일 수 있다」, 「남은 허용치보다 1바이트 더 읽어 전체 버퍼링 없이 초과를 잡는다」, 「`comm`은 괄호로 싸여 있고 공백·`)`을 포함할 수 있으니 **마지막** `)` 뒤에서 자른다」. 전부 되돌리기 어려운 실수의 근거이거나 외부 포맷 지식이다.
- **`main.go`의 PTY·시그널 지식** — 「PTY slave가 controlling terminal이 되는 것이 곧 인터랙티브의 정의」, 「인터랙티브 셸은 SIGTERM을 무시하므로 SIGHUP 먼저」, `upgrader`의 「피어는 브라우저가 아니라 클러스터 안의 control plane이라 origin 게이트가 없다」.
- **`claude.go` `defaultClaudeStateDir`의 EBUSY(3줄)** — 3차 패스가 `client_orchestrator.go`의 `claudeCodeStateDir`에서 남긴 것과 **같은 사실의 두 번째 사본**이다. 갈렸다(아래).
- **`claude.go` `appendPlatformNotice`의 quota·redaction 제외 근거(5줄)** — 「invocation quota는 assistant text와 stderr에 대해 정의된 것이고 플랫폼 알림은 둘 다 아니다」, 「도구 이름과 외부 식별자로 조립되므로 redaction sink를 지나지 않는다」. AC-E2가 quota의 범위를 정하지만 **이 코드 경로가 왜 그 밖인지**는 말하지 않는다.
- **`approval_gateway.go`의 실패 의미론** — `poll`의 「모르는 중간 status는 결정이 아니라 계속 대기로 읽는다」, `do`의 「묻지 못한 것은 거절당한 것이 아니다 — 모델에 refusal로 보고해도 되는 것은 뒤쪽뿐이다」, `approvalDecision`의 「PENDING은 요청의 상태이지 기다림의 결과가 아니다」.
- **`session_mcp_tools.go` `requestIDPrefix`(4줄)** — 「JSON-RPC id를 쓰지 않는 이유: 에이전트 프로세스마다 1로 되감기므로 한 세션의 두 invocation에서 충돌한다」. 어느 문서에도 없다.
- **`output_stream.go`의 셸 PTY 폴백(4줄)** — 「셸 PTY 바이트는 임의일 수 있다. 청크 안에 경계가 없으면 그 스트림을 영영 멈추느니 바이트 경계 폴백을 유지한다」. AC-E3은 Claude 출력만 valid UTF-8이라고 말하고 셸 쪽은 말하지 않는다.
- **`main.go` `adopt`의 `sync.Once` 근거(3줄)** — 「한 pod은 평생 셸을 최대 하나 채택한다(신규 기동 **또는** 복원 한 번), 그래서 once로 충분하다」.
- **cross-module 「Keep in sync」 4건** — `helperProxyPort`/`sessionMCPPort` · `defaultAddr` · `restoreModeEnv` · MCP 컨테이너 env 3종. 3차 패스가 컨트롤 플레인 쪽 4건을 남긴 것과 같은 이유이고, **이쪽이 그 4건이 가리키는 반대편**이다.

**갈려서 남긴 것 셋.**

1. **`credential_proxy.go` `providerCACertEnv`의 7줄** — 존재 이유(공개 저장소가 모르는 CA가 발급한 private gateway)와 분류 근거(주소류이지 자격 증명이 아니다)는 **#70이 두 시간 전에 `client_orchestrator.go`의 `AnthropicCACertEnvVar`에서 판정해 8줄로 남긴 것과 같은 내용**이다. 즉 레포에 두 벌 있다. 그럼에도 남긴 이유: 컨트롤 플레인 쪽은 「어느 Secret 키를 어느 컨테이너에 넣는가」의 자리이고, **신뢰 결정을 실제로 실행하는 것은 이쪽**(`trustProviderCA`)이다. 한쪽만 읽는 독자가 반대쪽으로 가야 답을 얻는 형태를 만들지 않았다. 다음 패스가 둘 중 하나를 정본으로 정하면 그때가 제거 시점이다.
2. **`claude.go` `defaultClaudeStateDir`의 EBUSY** — 위와 같은 형태(3차 패스가 컨트롤 플레인 쪽을 남겼다). 마운트 경로를 **선언**하는 것은 저쪽이고 **rename하는 것은 이쪽**(`installClaudeState`)이라 남겼다.
3. **`checkpoint.go` `criuEngine.Restore` 인터페이스 doc(3줄)** — 본문이 하는 일을 옮겨 적는 형태로 보이지만, 인터페이스 메서드의 doc은 **구현이 아니라 계약**이고 `fakeCriuEngine`도 그 계약으로 작성됐다. 남겼다.

**관측 — 이 렌즈 밖에 낡은 주석이 하나 지목돼 있다.** `docs/test/e2e.md`가
`control-plane/cmd/control-plane/main.go`의 `k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, …)`
앞 인라인 주석(「its data plane runtime — the helper pod's session MCP — is not implemented yet」)을
**거짓이 된 주석**으로 지목하며 소관을 이 모델로
넘겼다. 이번 슬라이스의 범위(`data-plane/cmd/agent/`) 밖이라 손대지 않았다 —
`cmd/control-plane/`(주석 77줄)을 잡는 **다음 패스의 1순위**다.

## 증분 재판정 — AC-F3 유휴 예외가 더한 16줄 (2026-09-04)

이 슬라이스가 열려 있는 동안 #71이 머지돼 등재 범위에 주석 16줄을 더했고, 머지 직전 게이트가
R2로 잡았다. 유입된 **16줄만** 같은 절차로 판정했다 — **제거 8 · 유지 8**(739 → 747).

| 자리 | 판정 | 근거 |
| --- | --- | --- |
| `claude.go` `approvals` 필드 doc (2→1) | 1줄 제거 | 「claude-code에서는 비어 있다 — 게이트가 없으니까」는 `approval_wait.go`의 `observeApprovalNotices` doc이 **그 사실을 소유한 자리에서** 이미 말한다. AC-F3 포인터 한 줄만 남겼다 |
| `claude.go` `approvalWaitPath` 핸들러 doc (5→2) | 3줄 제거 | 「사람을 기다리는 중인지 답해서 컨트롤 플레인이 유휴 카운트를 붙든다」는 ② `docs/prd/approval-gated-workload.md` AC-F3의 사본이고 `approval_wait.go` 패키지 doc이 더 길게 적는다. 반면 **「복원 대기 중에도 답한다」**는 바로 아래 `GET /attach`가 `isReady()` 가드를 두는 것과의 **대비**로만 보이는 사실이라 남겼다 |
| `session_mcp_notice_tail.go` `noticeSink` doc (4→2) | 2줄 제거 | 「한 폴이 두 가지를 함께 준다 → 그래서 함께 넘긴다」는 **인터페이스가 두 메서드를 한 타입에 묶은 형태 자체**가 말한다(①). AC-E3·AC-F3 포인터는 남겼다 |
| `session_mcp_notice_tail_test.go` `recordingSink` doc (2→0) | 2줄 제거 | `emit func(string)` · `waits approvalWaits` 두 필드와 두 메서드가 그대로 말하는 선언 재진술이고, 「테스트가 양쪽을 단언할 수 있게」는 그 단언을 하는 테스트 본문이 말한다(①) |

**유지한 8줄**: 위 표의 잔여 5줄과 `run()`의 **「State before markers」 4줄 중 순서 계약 부분** —
「상태를 마커보다 먼저 넘긴다」와 그 이유(늦은 상태는 하지 말았어야 할 freeze를 만들지만, 늦은
마커는 늦은 마커일 뿐이다)는 **루프 안의 순서 계약**이라 정책이 명시적으로 보호하는 유형이다.
갈리지 않았다.

**범위 밖으로 남긴 것 — 신규 2파일.** #71이 같은 패키지에 `approval_wait.go`(48줄)와
`approval_wait_test.go`(20줄)를 새로 더했다. 등재 행의 파일 목록 밖이라 게이트는 이 68줄을
미판정 잔량으로 센다. 이 슬라이스는 **등재된 32파일의 유입분만** 재판정했고, 두 신규 파일은
as-is 지문 이동의 재감지가 다음 task로 잇는다.

## 증분 재판정 — #71이 더한 신규 2파일 (2026-09-05)

바로 위 문단이 「as-is 지문 이동의 재감지가 다음 task로 잇는다」로 넘긴 68줄이다. 그 재감지가
발화해 이 행에 편입했다 — `approval_wait.go`(48) · `approval_wait_test.go`(20), **68줄 판정,
제거 34 · 유지 34**(747 → 781).

**게이트가 이 68줄에 대해 침묵했다는 점을 먼저 적는다.** R2는 *등재된 파일 목록*을 재측정할 뿐
「이 패키지에 등재 밖 파일이 있는가」를 묻지 않는다. 그래서 rc=0 이면서도 같은 디렉터리의 신규
2파일이 미판정으로 남아 있었다 — 게이트 결함이 아니라(인구조사 카운터로는 보였다) **초록이
커버리지를 뜻하지 않는다**는 뜻이다. 같은 형태를 다음에도 만들지 않으려면, 등재 행을 만든
슬라이스 뒤에 그 경로로 들어온 신규 파일을 **다음 패스가 먼저 훑어야 한다.**

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `approval_wait.go` 패키지 주석 (17→6) | AC-F3 유휴 예외의 서술과 근거(「대기 중 동결되면 승인이 나도 실행할 파드가 없다」) · pull을 택한 이유 7줄(egress 허용목록·역방향엔 ingress 정책이 없음·NetworkPolicy 무변경) | ② `prd/approval-gated-workload.md` AC-F3의 「유휴 기준」 불릿이 **근거 문장까지 축자**다 · ③ PR #71 본문의 「방향을 pull로 고른 근거」 표가 세 방향을 각각 판정해 적는다 · ① `session_mcp_notices.go`가 같은 선택을 먼저 했다. **포인터(AC-F2·`session_mcp_notices.go`)만 남겼다** |
| `approval_wait.go` `approvalWaitPath` (3→1) | 「헬퍼 파드가 아니라 워크로드 에이전트 서버에 있다 — 헬퍼가 바로 컨트롤 플레인이 닿지 못하는 쪽이니까」 | ② AC-F2 · ③ 같은 PR 표. 위 패키지 주석에서 이미 한 번 지운 사본의 두 번째 벌이다 |
| `approval_wait.go` `approvalWaits` (8→4) | 「파생 상태이고 아무 데도 영속되지 않는다 … 헬퍼는 동결을 건너 상태를 갖지 않고(AC-F4), 복원된 세션은 새 에이전트와 빈 피드를 받는다」 | ① `session_mcp_notice_tail.go`의 `noticeTailer` doc이 **같은 문장을 축자로** 적는다(「a new agent process means a new helper pod with a feed that also starts empty (AC-F4)」). 「set이지 counter가 아닌 이유」는 남겼다 — 재전달이 언제 일어나는지는 코드 형태로 보이지 않는다 |
| `approval_wait.go` `observe` (9→6) · 미지 kind 인라인 (3→2) · `approvalWaitResponse` (4→2) | 비움의 비용 비대칭(「한 번 잘못 얼면 재접근에 복원되지만, 반대는 회수되지 않는 파드다(V2)」) · 「미지 kind를 발행하는 최신 헬퍼」 서술 · 「도구 이름·외부 식별자·게이트웨이 정보를 싣지 않는다 … 파드 경계를 넓힐 뿐」 | ③ PR #71 「판단이 갈린 지점」이 그 트레이드오프를 그대로 적는다 · ① `renderApprovalNotice` doc(주석이 스스로 「matching renderApprovalNotice」로 가리키고 있었다) · ① `session_mcp_notices.go`의 `approvalNotice` doc이 **같은 논거를 같은 낱말로**(「would only widen what crosses the pod boundary」) 적는다 |
| `approval_wait_test.go` doc 7곳 (20→7) | 테스트 이름·표의 케이스 이름·소스 doc이 그대로 말하는 서술 — 「awaiting builds the notice …」 · 「the helper pod publishes, the tailer follows, …」 · AC-F6 3줄 · 「real gate가 결정하면 …」 | ① 각 테스트 함수 이름과 서브테스트 이름(「approval closes it」·「rejection closes it」…) · ① `approval_wait.go`의 소스 doc. **「어느 쪽을 재는가」 한 줄씩만 남겼다** |

**유지 34줄** — 파일 목적 문단과 pull 포인터, 「set이지 counter가 아닌 이유」, 「미지 kind가 유휴
카운트를 붙잡지 못하게」, `observeApprovalNotices`가 claude-code에도 있는 이유(게이트도 헬퍼도
없어 아무도 발행하지 않는다), 그리고 위 표에서 한 줄로 줄인 테스트 doc 7줄.

## 증분 재판정 — 세션 MCP 등록 자리 수정 슬라이스 (2026-09-12 (2))

CLI 가 MCP 서버를 읽지 않는 파일에 등록하고 있던 결함을 고치면서 이 범위에 신규 1파일
(`claude_mcp_registration.go`)이 들어오고 `claude.go`·`approval_gated_test.go` 의 주석이 움직였다.
**93줄 판정, 제거 26 · 유지 67**(865 → 916).

**이 슬라이스의 지배 형태는 앞선 패스들과 달랐다.** 베낀 원본이 문서나 매핑 행이 아니라 **같은 PR 이 방금
만든 파일 헤더**였다 — 초안은 `claude_mcp_registration.go` 헤더가 소유한 「CLI 2.1.220 관측」을
`claude.go` 세 자리와 테스트 세 자리에 각각 한 번씩 더 적고 있었고, 여섯 곳 전부 **스스로 그 파일을
가리키면서** 내용을 함께 옮겼다(정책 본문의 「포인터는 남기고 사본은 지운다」가 그리는 바로 그 형태).
초안 137줄을 판정에서 127줄로 줄인 10줄이 그것이다.

**낡아 거짓이 된 주석 1건**도 포함한다 — `claude.go` 의 `Tools` 필드 doc 이 「session HOME 의
settings.json 에 쓰인다」였는데, 이 슬라이스가 등록을 CLI 설정으로 옮기면서 그 문장이 절반만 참이
됐다. 제거 근거가 강해지는 유형이다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `claude.go` `claudeManagedSettings.MCPServers` (4→3) | 「CLI 는 이 파일에서 MCP 서버를 읽지 않는다」 | ① `claude_mcp_registration.go` 패키지 헤더가 그 관측의 정본이다. **남긴 것은 필드가 남아 있는 이유** — 쓰이지 않는 필드가 `DisallowUnknownFields` 아래 옛 아카이브를 디코드하려고 존재한다는 것은 코드 형태로 보이지 않는다 |
| `claude.go` `ensureClaudeManagedSettings` doc (6→5) | 「표면은 두 파일에 걸치고 이것이 둘 다 쓰는 유일한 진입점이다 — 권한은 settings.json, 등록은 CLI 설정」 | ① 함수 본문이 두 호출을 그대로 보여 준다. **「한쪽만 쓰면 도구가 조용히 사라진다」는 남겼다** — 실패가 무증상이라는 것이 이 함수를 하나로 합친 이유이고, 그 사실은 코드에 나타나지 않는다 |
| `claude.go` `validateClaudeManagedSettings` doc (7→5) | 「MCP 표면의 두 반쪽이 떨어져 살아서 두 파일에 걸친다」 | ① 함수 본문의 `loadClaudeManagedSettings` + `loadClaudeMCPRegistration` 두 호출 · ① `validateClaudeToolSurface` 시그니처가 두 인자로 같은 말을 한다 |
| `claude.go` `Tools` 필드 doc (1→1, 재작성) | 「session HOME 의 settings.json 에 쓰인다」 — 이 슬라이스가 등록을 옮겨 **거짓이 됐다** | ① `ensureStateDirs` 가 어디로 가는지 보여 준다. 포인터 성격의 1줄로 다시 썼다 |
| `claude.go` `loadClaudeManagedSettings` doc (6→2) | `requireToolSurface` 분기의 존재 이유 서술 전부 | ① **그 플래그가 사라졌다** — 도구 표면 판정이 `validateClaudeToolSurface` 로 나가면서 이 doc 이 설명하던 대상 자체가 없어졌다 |
| `approval_gated_test.go` 테스트 doc 3곳 (16→6) | 「pinned CLI 는 자기 설정에서 MCP 를 읽고 settings.json 의 mcpServers 를 무시한다」 · 「CLI 가 파일을 매 실행 재작성하며 machine·account 식별자와 프로젝트 기록을 넣는다」 · 「검증은 양쪽 위치를 받고 다음 부팅이 옮긴다」 | ① 셋 다 `claude_mcp_registration.go` 헤더와 `validateClaudeToolSurface` 인라인이 소유한다. **「이 테스트가 무엇이 깨지면 걸리는가」 한 줄씩만 남겼다** |

**유지 67줄** — 대부분이 신규 파일 헤더이고, 그 헤더가 이 슬라이스의 **유일한 복원 불가능 지식**을
담는다: 핀된 CLI 2.1.220 에서 `$HOME/.claude/settings.json` 의 `mcpServers` 는 **오류 없이 무시되고**
(`claude mcp list` 가 「No MCP servers configured」, 도구 호출은 `No such tool available`),
`$HOME/.claude.json` 의 같은 블록은 연결된다는 관측. CLI 문서에 없고, 설정 파일이 읽지 않는 키를
불평 없이 받기 때문에 **신호가 「도구가 없다」 하나뿐**이다 — 이 레포 어디에도, 업스트림 어디에도
적혀 있지 않다. 같은 헤더가 CLI 가 그 파일을 소유한다는 관측(매 실행 재작성·식별자 주입)도 갖고,
그것이 「렌더링하지 않고 머지한다」는 설계의 근거다.

나머지 유지는 셋이다: `registersSessionMCP` 가 **다른 서버를 관대하게 지나치는 이유**(AC-F2 의 egress
허용목록이 이미 막으므로 복원을 깨뜨릴 값이 없다), `validateClaudeToolSurface` 의 **옛 등록 자리를
계속 받는 이유**(거절하면 지금 동결된 세션이 복원 불가가 된다), 그리고 각 테스트가 무엇을 재는지
한 줄씩.

## 증분 재판정 — 등록 URL 의 경로 누락 수정 슬라이스 (2026-09-12 (3))

바로 위 슬라이스가 등록 **자리**를 고쳤고, 이 슬라이스가 등록 **값**을 고친다. 같은 범위의
`claude.go`·`credential_proxy.go`·`approval_gated_test.go` 에 초안이 22줄을 더했다.
**22줄 판정, 제거 11 · 유지 11**(916 → 927).

**지배 형태는 바로 위 슬라이스와 같다** — 같은 PR 이 방금 만든 주석의 사본. 초안은
`sessionMCPEndpoint` doc 이 소유한 「404 가 조용하다」는 관측을 `credential_proxy.go` 의 새 가드와
새 테스트 doc 에 각각 한 번씩 더 적고 있었다. 두 번 연속 같은 형태가 나온 것은 우연이 아니라 이
저자의 습관으로 보이므로, 다음 슬라이스가 먼저 볼 자리로 적어 둔다.

**이 슬라이스는 자기 주석의 복원 경로를 스스로 하나 만들었다.** 초안이 「`SessionMCP` 는 베이스
주소이고 소비자들이 각자 경로를 붙인다」를 적고 있었는데, 같은 PR 이 `validateHelperEndpoint` 에
bare-origin 가드를 넣으면서 그 문장이 **코드로 복원 가능해졌다**(①). 판정 중에 복원 경로가 생기는
경우라 적어 둔다 — 초안 시점에는 유지 대상이었다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `claude.go` `sessionMCPEndpoint` doc (10→6) | 「`SessionMCP` 는 host·port 뿐인 베이스 주소다」 · 「이 파드의 소비자 둘이 각자 경로를 붙인다」 · 「형제 주소인 credential proxy 는 catch-all `/` 를 서빙해서 베이스를 그대로 줘도 동작한다」 | ① 같은 PR 이 넣은 `validateHelperEndpoint` 의 bare-origin 가드가 첫째를 말한다 · ① 둘째는 이 함수의 `+ sessionMCPPath` 와 `noticeTailer` 의 `+ noticesPath` 두 자리가 그대로 보여 준다 · ① 셋째는 `credential_proxy.go` 의 `mux.HandleFunc("/", …)` 한 줄이다. **남긴 것은 핀된 CLI 의 관측** — 404 가 「닿지 못한 주소」가 아니라 「도구 없는 서버」로 보고된다는 것 |
| `credential_proxy.go` 새 가드 인라인 (5→2) | 「부팅 때 거절하는 편이 첫 도구 호출의 404 보다 낫고, 그 404 를 CLI 는 도구 없는 서버로 보고한다」 | ① 바로 위 `sessionMCPEndpoint` doc 이 그 관측의 정본이다 — 같은 PR 안에서 두 번째 벌이었다. **포인터만 남겼다** |
| `approval_gated_test.go` 새 테스트 doc (7→3) | 「등록은 서버가 답하는 엔드포인트를 가리켜야 한다」 · 「이 패키지에서 둘이 어긋나도 아무것도 실패하지 않는다」 · SUT 관측의 재진술(마커가 90초 안에 한 번도 안 나왔고 CLI 가 루트로 POST 했다) | ① 첫째는 테스트 본문이 그대로 한다 · ① 둘째는 **바로 위 테스트 doc 이 이미 적는다**(「nothing else here fails when it is wrong」) · ③④ 셋째는 이 PR 본문과 커밋 메시지가 소유한다. **run 링크는 포인터라 남겼다** |

**유지 11줄** — 둘로 갈린다. `sessionMCPEndpoint` doc 6줄은 **핀된 CLI 의 문서화되지 않은 동작**이다:
등록 URL 이 틀리면 CLI 가 연결 오류가 아니라 **도구 없는 서버**를 보고하므로 세션은 계속 돌고 모델만
게이트에 닿지 못한다. 신호가 「도구가 없다」 하나뿐이라는 점에서 위 슬라이스가 남긴 관측과 같은 성질이고,
실제로 그 두 결함이 **같은 증상으로 연달아** 나타났다. 나머지 5줄은 포인터 2줄과, 새 테스트가 **왜 문자열
비교가 아니라 실제 mux 를 구동하는가** 3줄 — 등록하는 쪽과 서빙하는 쪽이 다른 파일에 있어 이 패키지에서
둘의 불일치가 공짜라는 것은 코드 형태로 보이지 않는다.

## 2026-09-12 (4) 증분 재판정 — AC-F5 후반(아카이브) 슬라이스

`claude_archive.go`·`claude_archive_test.go`·`claude.go`·`claude_test.go` 에 초안이 57줄을 더했다.
**57줄 판정, 제거 22 · 유지 35**(927 → 962).

**지배 형태는 ① 이고, 이번에는 doc 의 첫 문장에 몰려 있었다.** Go 의 doc 관례가 「식별자로 시작하는
한 줄」을 요구하다 보니 초안이 그 한 줄을 **시그니처의 산문 번역**으로 채운 자리가 셋이었다. 관례는
이름으로 시작하는 것이지 선언을 되풀이하는 것이 아니므로(정책 「유지 대상」 2번), 첫 문장을 **그 함수가
가진 이유** 쪽으로 옮겨 적고 번역은 걷었다.

**틀린 주석 1건을 여기서 걷었다.** `sharedArchiveWorkload` 의 doc 이 「a pair of agents … 그들의 두
공유 디렉터리를 반환한다」고 적었으나 실제 반환은 워크로드 하나·서버 하나·디렉터리 하나다. 초안이
쓰인 뒤 헬퍼의 모양이 바뀐 흔적이고, 아무 게이트도 이것을 잡지 못한다 — 정책이 말하는 「조용히
거짓이 되는」 형태의 교과서적 사례라 적어 둔다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `claude_archive.go` `writeClaudeArchive` doc | 「`sharedDir` 는 둘째 트리를 더한다」 | ① 시그니처와 바로 아래 `if sharedRoot != nil` 분기 |
| `claude_archive.go` `writeClaudeArchiveTree` doc | 「archiveRoot 아래로 트리 하나를 걷는다」 | ① 시그니처 |
| `claude_archive.go` `openClaudeArchiveRoot` doc | 「트리를 자기 자신으로 가둔다」 | ① `os.OpenRoot` 호출 한 줄 |
| `claude_archive.go` `installSharedVolumeContents` doc | 「검증된 트리를 최상위 엔트리 단위로 옮긴다」 | ① 시그니처와 루프 |
| `claude_archive.go` `claudeArchiveSharedRoot` doc | 「approval-gated 세션의 공유 볼륨을 나른다」 | ① 상수값 `"shared"` 와 그것을 쓰는 두 분기 |
| `claude_archive_test.go` `sharedArchiveWorkload` doc (전량) | 반환값의 산문 번역 — **게다가 틀렸다** | ① 시그니처. 틀린 사본은 복원 경로 이전에 지울 이유가 있다 |
| `claude_archive_test.go` 테스트 doc 셋 | 「설정을 끄면 승인된 파일이 조용히 사라진다」 · 「새 클레임은 비어 있으므로 이미 있는 이름은 남의 잔여다」 · 「공유 트리는 state 와 같은 탈출 검사를 받는다」 | ① 앞의 둘은 **같은 PR 이 지은** `restoreClaudeArchive` 인라인과 `installSharedVolumeContents` doc 이 정본이다 · ① 셋째는 테스트 이름과 케이스 표가 말한다 |

**유지 35줄** — 전부 레포 어디에도 없는 것이다. 큰 덩어리 넷:

1. **공유 트리를 쓰는 주체가 이 에이전트 밖에 있다.** 워커는 체크포인트 전에 drain 되지만 헬퍼 파드의
   MCP 는 그렇지 않다. 그래서 per-entry `SameFile` 검사가 방어가 아니라 **하중을 받는 검사**이고,
   지우면 걷는 도중 착지한 spill 이 반쪽 파일로 아카이브된다.
2. **마운트 포인트는 rename 으로 덮을 수 없다.** state 트리가 쓰는 「스테이징 디렉터리를 통째로
   swap」 이 `/shared` 에서 성립하지 않는 이유이고, 설치가 엔트리 단위인 것도 스테이징이 마운트
   **안**에 있는 것도 전부 여기서 나온다.
3. **설치 순서가 공유 먼저인 이유** — 그쪽이 실패하면 살아 있는 state 트리가 손대지지 않은 채로
   남는데, 둘 중 더 지킬 값이 있는 보장이다.
4. **traversal 테스트 첫 케이스의 함정** — 어휘적 `..` 는 dispatch 전에 정규화돼 공유 분기에 닿지
   못하고 「아무도 소유하지 않는 멤버」로 거절된다. 그것을 모르면 그 케이스가 공유 쪽 가드를 산다고
   오독하게 된다.

**오검출 1줄**: 게이트의 주석 정규식 `^[ \t\v\f\r]*(//|/\*|\*[^/])` 가 `*entryCount++` 를 주석으로
집는다. 8차·9차가 찾은 오검출(`tokens.css` 의 `* {`, 셸 `case` 와일드카드)과 같은 갈래다.
