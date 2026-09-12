# 2026-09-08 — `control-plane/test/`의 나머지 2파일 (판정 249줄)

9차 패스가 **「다음 패스의 1순위」로 이름까지 찍어 남긴** `approval_gated_orchestrator_test.go`와,
그 뒤 새로 들어와 어느 등재 행에도 속하지 않던 `e2e_f6_credential_split_test.go`를 함께 판정했다.
249줄을 한 줄도 빠짐없이 읽었다.

**왜 지금인가 — 9차가 적어 둔 유예 사유가 소멸했다.** 그때의 사유는 「열린 PR이 이 파일을
65 → 72줄로 움직이고 그 PR이 이 원장도 함께 고치고 있어, 등재하면 rebase + 증분 재판정 의무를
얹게 된다」였다. **그 PR은 머지됐고** 이 경로에 열린 PR은 없다. 다른 파일은 반대 방향의
사정이다 — 신규 파일은 어느 등재 행에도 속하지 않아 R2가 반응하지 않으므로, 등재하기 전까지는
**몇 줄이 쌓이든 게이트가 조용하다.** 두 사정 모두 「지금 등재한다」로 모인다.

**제거율 73%** — 앞선 아홉 패스 중 가장 높다. 이 범위의 지배적 형태는 9차 패스가 이미 이름 붙인
그것, `docs/test/e2e.md` §「시나리오 ↔ e2e 파일 매핑」의 **「무엇을 검증하나」 칸을 파일 헤더로
옮겨 적은 것**인데 여기서는 정도가 더 심하다. `e2e_f6_credential_split_test.go`의 헤더 **67줄**이
그 파일의 매핑 행 하나와 §「남은 미검증 분기 (공백은 아님)」 네 행을 영어로 다시 쓴 것이었다.
그 매핑 행은 한 문단이 아니라 **한 화면 길이**로, 헤더가 적은 7개 항목·5개 공백·소유권 분업을
전부 더 자세히 적는다.

**제거 183줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `e2e_f6_credential_split_test.go`의 파일 헤더 (67 → 8) | 「이 파일이 무엇을 사는가」 7항목 · 「무엇이 일부러 빠졌고 왜인가」 5항목 · 「다시 사지 않는 이웃」 4항목 | ② §「시나리오 ↔ e2e 파일 매핑」의 이 파일 행이 셋을 **전부, 더 자세히** 적는다 · ② §「남은 미검증 분기 (공백은 아님)」의 네 행이 각 공백의 **선행·소관·왜 vacuous한가**까지 갖는다. 주석이 스스로 *「all five are registered in docs/test/e2e.md …」* 라고 **자백**하고 있었다 — 정책 「포인터는 남기고 사본은 지운다」 그대로 포인터만 남겼다 |
| `approval_gated_orchestrator_test.go`의 파일 헤더 (8 → 2) | 「fake가 살 수 없는 것은 두 헬퍼 컨테이너의 런타임 동작과 egress 정책의 실제 집행이다」 분업 · 「AC-F1의 타입 계약은 API 계층이 산다」 | ② 매핑 표와 §「매칭 단위 밖」 · **같은 분업 문장이 `client_orchestrator_test.go`에 이미 있다** — 9차 패스가 「한 벌만 남긴다」로 판정한 그 자리다. ⚠️ 덧붙여 마지막 문장 「Neither is implemented in this slice」는 **낡아 거짓**이었다(AC-F3 승인 게이트는 그 뒤 머지됐다) |
| const 블록의 doc 7묶음 17줄 → 0 | 「The gateway triple, named as the helper pod's MCP container gets it」·「…and the Secret keys behind them」·「`modelEnvVar` and the label the control plane runs under」 | ① 상수 이름과 그 값이 같은 말을 한다(`gatewayURLKey = "url"`). 정책 「유지 대상」 2가 이 형태를 이름으로 지목하고, 전부 **비공개 식별자**라 Go 관례상의 doc 의무도 없다 |
| 시그니처 재진술 **앞머리** 9곳 | 「`container` returns a named container of either pod」·「`secret` reads one of the platform Secrets from the cluster」·「`mustEnv` returns a container's environment entry」 | ① 시그니처 자체. **뒤에 붙어 있던 「왜」 문장은 남겼다** — 지운 것은 각 doc의 첫 문장뿐이다 |
| 바로 아래 단언이 하는 말 11곳 | 「Required, all three, and out of the gateway's own Secret」·「Its neighbour in the same pod … are given none of it」·「The workload container must carry no Secret projection at all」·「Same provisioning round: the names differ only in the role letter」 | ① 다음 2~6줄과 그 실패 메시지가 같은 말을 한다 |
| 테스트 함수 이름을 되풀이한 doc 6곳 | 「Regression guard for the two existing types」(`…_ExistingTypesUnchanged`) · 「The workload pod … must not carry the credential proxy sidecar」(`…_WorkloadPodHasNoCredentialSidecar`) · 「A concrete model … outranks the Secret default」(`…_ConcreteModelBeatsThePlatformDefault`) | ① 함수 이름이 이미 문장이다. 9차 패스가 24곳에서 지운 것과 같은 형태 |
| AC 축자 6곳 | 「AC-F6: the gateway triple reaches the MCP container only, the provider credentials reach the proxy container only…」·「AC-F1/AC-F4: an approval-gated session is provisioned as a workload pod *and* a session-scoped helper pod…」 | ② `docs/prd/approval-gated-workload.md`의 해당 AC. **AC 번호(포인터)만 남기고 본문은 지웠다** |
| 대조군 근거의 **사본** 2곳 | 워크로드 프로브 자리의 「its own placeholder is the control…」 · 헤더의 「with the startup line and a Secret-resolved value in it as controls」 | ① 같은 근거가 `f6ReachProbe`·`f6ControlPlaneLog`의 **정의 자리에** 있다. 9차 패스의 「한 벌만 남긴다」 |
| 이력 서술 2곳 | 「that is exactly how this assertion failed the first time it ran」·「the type's whole failure mode until now was a pod spec whose containers the agent refused」 | ③ PR · ④ 커밋 메시지. 정책이 복원 경로로 이름 붙인 바로 그 둘이다 |

**유지 66줄** — 「지울까」를 검토했다가 남긴 것들. 9차 패스와 같은 이유로 유지 비중이 높다
(**「무엇을 단언하나」는 문서가 갖지만 「왜 이 단언이 무언가를 증명하나」는 어디에도 없다**).

- **음성 단언의 대조군 근거 3종** — `f6ReachProbe`의 `control`/`leak` 대비(control이 맞지 않으면
  `leak=0`은 「/proc을 못 읽었다」는 뜻일 뿐이다) · 「각 보유자가 자기 시크릿을 **실제로 해소했다**」
  (빈 변수는 어느 환경에도 없으므로 이것 없이는 음성 결과가 무의미하다) · control-plane 로그의
  **두 번째** 대조군(Secret에서 해소한 값 하나가 실제로 echo돼 있어야 네 부재가 무언가를 말한다).
  **없으면 그 테스트가 vacuous한지 알 수 없다.**
- **왜 이 헬퍼가 자매와 다른가** — `f6RequireSecretRef`가 Secret **이름을 파라미터로** 받는 이유.
  AC-E6의 `e6RequireSecretRef`는 그 타입에 Secret이 하나뿐이라 상수로 들고 있어도 옳지만,
  **두 Secret이 두 컨테이너로 갈린다는 이 AC의 주장을 문장으로 만들 수 없다.** 코드만 보면
  「파라미터가 하나 더 있다」로만 읽힌다.
- **키와 이름을 가르는 이유** — `f6NoSecretRefTo`가 변수 이름이 아니라 **Secret 키**로 보는 이유
  (같은 키가 다른 변수 이름으로 도착할 수 있다). 자매 `e6RequireAbsent`와 **왜 둘 다 필요한지**가
  여기서만 나온다.
- **미래 편집자를 향한 규칙 2종** — 「비교하는 시크릿 값은 전부 클러스터에서 읽는다, 이 파일은
  자격 증명 사본을 갖지 않는다」와 「값은 보간하지 않고 위치 인자로 넘긴다」. 둘 다 현재 코드를
  읽어 **확인은** 되지만 **다음 편집이 어길 수 있는 규칙**이라, 지우면 규칙이 어디에도 남지 않는다
  (정책 「애매하면 남긴다」).
- **비용·계약 사실 3종** — 세션 생성이 **두 파드가 Ready가 되어야** 반환한다(그래서 테스트마다
  하나만 만들어 모든 단언을 건다) · `ok=false`는 클러스터가 없을 때이고 그때는 **실패가 아니라
  스킵**이다 · `mustEnv`가 부재를 실패로 바꾸는 이유(없는 항목이 **빈 값으로 읽힌다**).
- **외부 시스템의 동작 2종** — HTTP readiness probe는 **kubelet이 노드에서** 건다(그래서 AC-F2의
  ingress 정책이 막을 바로 그 호출자다) · 데이터 플레인은 **선언된 placement가 승인하지 않는
  bind를 거부한다**(헬퍼용 파드 네트워크 bind를 연 것이 claude-code 사이드카에 우연히 닿을 수 없는
  이유). 둘 다 이 레포의 다른 곳에 서술이 없다.
- **테스트 설계의 순서 함정** — 이미지 미설정은 **헬퍼 파드 스펙에서 먼저** 실패해 아무것도
  만들어지지 않고, 유효하지 않은 모델은 **헬퍼가 Ready가 된 뒤** 워크로드 스펙에서 실패한다.
  두 갈래의 **순서**가 그 테스트의 요점인데 코드 모양으로는 구분이 안 된다.
- **갈린 판정 1건** — `approval_gated_orchestrator_test.go`의 `AC-F6's ✅ 2026-09-03 decision.`
  한 줄. 그 결정의 **내용**(이 타입은 plugin 부트스트랩을 두지 않는다)은 ②가 갖지만, **날짜가 붙은
  결정 포인터**는 그 자리에서 「누락이 아니라 결정」임을 말해 준다. 같은 결정을 서술한 3줄이
  `e2e_f6` 쪽에도 있었는데 **그쪽은 지웠다** — 한 벌만 남긴다.

**부수 변경 하나 — gofmt.** const 블록에서 주석을 걷어내자 `modelEnvVar`·`controlPlaneLabel`·
`controlPlaneStartupLine`이 하나의 정렬 그룹으로 합쳐져 `gofmt`가 공백을 다시 맞췄다. 주석을 뺀
바이트는 **공백 정규화 후 부모와 완전히 동일**하다(두 파일 모두, 비-주석 줄 수 805줄 불변).

**등재하지 않은 파일 2개 — 둘 다 다음 패스의 1순위다.** `harness_shared_test.go`(패키지 공유
하네스. 시나리오 축의 후속 저작이 헬퍼 변형을 필요로 하면 착지하는 자리다) ·
`shared_volume_test.go`(열린 PR이 이 원장에 「다음 패스의 1순위로 남긴다」는 소유를 **살아 있는
문장으로** 선언해 두었다 — 편입은 파일 목록 관리가 아니라 **판정 책임의 주장**이라 남의 선언 위에
쓰지 않는다). `web/src/design/README.md`·`data-plane/Dockerfile`의 오탐 7줄은 9차 패스와 같은
정의 개정 사안으로 남는다.
