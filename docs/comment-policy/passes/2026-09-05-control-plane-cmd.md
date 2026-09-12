# 2026-09-05 — `control-plane/cmd/control-plane/` (판정 77줄)

4차 패스가 「다음 패스의 1순위」로 지목한 조립 루트. **제거 19 · 유지 58.**

**지목의 근거였던 「거짓이 된 주석」을 실측으로 확정하고 지웠다.** `docs/test/e2e.md`가
`main.go`의 `WithWorkloadImage(…WorkloadTypeApprovalGated…)` 앞 인라인(「its data plane runtime — the
helper pod's session MCP — is not implemented yet」)을 낡음으로 지목했고, 같은 주장이 `dataPlaneApprovalGatedImage` 필드 doc에도 **두 번째 벌**로 있었다.
데이터 플레인에는 `session_mcp.go` · `session_mcp_tools.go` · `session_mcp_notices.go` 등 8파일이
실재하고 `main.go`가 `case workloadSessionMCP:`로 그 워크로드를 띄운다 — 주석이 틀렸다. 그리고
그 타입이 실제로 inert인 **현재의 이유**(이미지 미설정 + 필수 `secretEnv` 3종이 없으면 헬퍼 파드가
Ready에 이르지 못함)는 `docs/test/e2e.md`가 좌표까지 들어 적고 있으므로, 사본을 새로 쓰지 않고
**두 자리 모두 사실 주장을 지우고 AC 포인터만 남겼다.**

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `main.go`의 `WithWorkloadImage(…ApprovalGated…)` 인라인(4→1) · `dataPlaneApprovalGatedImage` doc(4→3) | 「헬퍼 파드의 session MCP는 아직 데이터 플레인에 없다 … 그래서 unset이 기대되는 배포 상태다」의 **두 벌** | **거짓이다**(위). 현재 사유는 ② `docs/test/e2e.md`의 F 계열 문단이 좌표까지 들어 적는다(그 문단이 인용한 줄 번호는 그 뒤 낡았다 — 실제 자리는 `client_orchestrator.go`의 게이트웨이 3종 `secretEnv(ApprovalGateway{URL,APIKey,UserID}EnvVar, …)`이고, 그 원장은 자매 모델 `tbm_session-platform-ac-e2e` 소관이다) |
| `k8s.BuildClient` 앞 인라인 (7→3) | 「같은 client가 pod orchestrator와 ConfigMap/Lease store를 함께 받친다」 · 「활성 전략은 에이전트에게 셸 CRIU 번들이나 Claude 아카이브를 만들게 해 durable store에 넣고, 비활성 전략은 회수 전에 fail closed」 | ① `configmap.NewStore` doc이 **같은 문장을**(「the same client the pod orchestrator uses」) 적는다 · ① 이 파일 패키지 주석이 이미 전략 배분과 fail-closed를 말한다 |
| claude-code 이미지 인라인 (3→1) | 「Start가 claude-code 라벨로 셸 파드를 띄우는 대신 그 타입을 거부한다」 | ① 같은 파일 `dataPlaneClaudeCodeImage` 필드 doc이 축자에 가깝게 적는다(그쪽을 정본으로 남겼다) |
| 리퍼 인라인 (5→2) · 「Drive the idle→snapshot reaper …」(1→0) | 스캔 주기·`MaxIdle`·AC-D5·AC-A3 회수의 열거 · 「종료까지 리퍼를 돌린다」 | ① `service/reaper.go`의 `IdleReaper`/`ScanOnce` doc · ① 같은 파일 `idleScanInterval` 필드 doc · ① 바로 아래 `go reaper.Run(ctx)` |
| checkpoint 설정 필드 주석 (6→3) · `buildCheckpointStore` doc (3→1) | 「checkpointS3RoleARN을 ambient 위에서 assume … e2e SUT는 MinIO를 가리키고 role을 비워 static key로 인증한다」 · 「agent-driven checkpointer는 언제나 store가 필요하다: 아카이브는 곧 회수될 파드 안에서 만들어진다」 | ① `checkpointstore.NewS3` doc · ① `Config.RoleARN` 필드 주석이 e2e static key까지 적는다 · ① `criu.AgentCheckpointer` doc이 **같은 낱말로** 적는다 |

**유지 58줄** — 조립 루트에서만 보이는 것들. 폴백 이미지에 에이전트가 없어 readiness를 통과하지
못한다는 함정, 아카이브 게이트 두 개(`CRIU_ENABLED` · `CLAUDE_CODE_ARCHIVE_ENABLED`)가 데이터
유출 경계라는 사실, `parseClaudeCodeDefaultModel`이 예약 별칭을 명시 설정으로 받지 않는 이유
(Secret 투영 실수를 감추게 된다), `claudeCodeModels`가 API 허용목록이 아니라 표시 설정이라는 구분,
그리고 `mock-exception: CRIU-GATE` 등재 블록(자매 모델 `tbm_session-platform-e2e-mock-policy`
소관이라 손대지 않았다).

## 2026-09-12 증분 재판정 — AC-F5 후반(아카이브) 슬라이스

조립 루트에 초안이 10줄을 더했고, **낡아 거짓이 된 1줄**을 교체했다.
**10줄 판정, 제거 4 · 유지 6**(65 → 70).

교체한 1줄은 체크포인트 스토어 게이트 위의 「`CRIU_ENABLED` for shell, `CLAUDE_CODE_ARCHIVE_ENABLED`
for claude-code」다 — 이제 그 게이트가 두 타입을 켜므로 열거가 거짓이 됐다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| 게이트 블록 위 (5→2) | 「이 게이트가 허용하는 것은 타입이 아니라 전략(아카이브를 `CHECKPOINT_S3_*` 에 쓰는 것)이다」 · 「두 타입 모두에 등록한다」 | ① 같은 파일 아래쪽 `claudeArchiveEnabled` config 필드 doc 이 전략 서술의 정본이다 · ① 둘째는 바로 아래 `append` 두 줄 |
| approval-gated 등록 자리 (4→2) | 「없으면 이 타입에 전략이 없고 `POST /snapshot` 은 503 이며 유휴 리퍼가 이 세션들을 영영 지나친다」 | ① `reaper.go` 의 `ErrCheckpointDisabled → continue` 분기 · ② `doc-tracker.md` 의 AC-F5 항목이 같은 사실을 소유한다 |

**유지 6줄** — 둘 다 레포 안에 복원 경로가 없다. **왜 env 이름을 바꾸지 않았는가**(이름을 고치면 그
값을 세우는 모든 오버레이와 함께 착지해야 하고 그중 일부는 이 레포 밖에 있다)와, **왜 approval-gated
등록에는 claude-code 와 달리 이미지 검사가 없는가**(설정되지 않은 타입은 애초에 스냅샷할 세션을
만들지 못한다). 후자는 코드에 **비대칭만 보이고 이유가 안 보이는** 자리라, 없으면 다음 사람이
「빠뜨린 검사」로 읽고 더한다.
