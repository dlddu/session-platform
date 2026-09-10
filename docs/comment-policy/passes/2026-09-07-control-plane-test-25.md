# 2026-09-07 — `control-plane/test/` 25파일 (판정 841줄)

스캔 범위의 **마지막 미판정 경계**. 27파일 중 25파일을 통째로 읽었다 — 846줄(실 주석 841줄,
아래 「오검출」 참고). 이 행으로 8차까지가 남긴 「`control-plane/test/` 하나뿐」이 닫힌다.

**왜 25파일인가 — 경합면 2파일은 실측으로 특정해 뺐다.** 8차 패스가 이 경로를 미룬 이유는
「자매 렌즈가 매 슬라이스 새 파일을 더하는 자리라 저작 시점 지문이 머지 창 안에서 배신당한다」
였다. 그 위험은 **파일 단위로 갈린다** — 신규 파일은 어느 등재 행에도 속하지 않아 R2가 반응하지
않기 때문이다(#92가 신규 파일에 114줄을 들여왔는데 게이트가 rc=0이고 등재 8행이 지문까지 동일했던
것이 그 실측이다). 그래서 뺀 것은 **기존 파일을 실제로 움직이는 것 둘**뿐이다:

- `approval_gated_orchestrator_test.go` — 열린 PR이 이 파일을 **65 → 72줄로 움직인다**(양쪽
  리비전에서 게이트 추출기로 직접 셌다). 그 PR은 이 원장도 함께 고치고 있어, 등재하면 rebase +
  증분 재판정 의무를 얹게 된다. **이미 떠 있는 PR에 부채를 넘기지 않는다.**
- `harness_shared_test.go` — 패키지의 공유 하네스(13함수). 새 e2e 파일이 헬퍼 변형을 필요로 하면
  착지하는 곳이라, 신규 파일 저작이 진행 중인 동안은 등재하지 않는다.

반대로 `workload_type_orchestrator_test.go`와 `e2e_e6_credential_placement_test.go`는 **뺄 근거가
없어 넣었다** — 전자의 근거는 머지된 과거 선례 하나뿐이고, 후자는 자매 슬라이스가 그 안의 헬퍼를
*호출*할 뿐이며 Go 동일 패키지 재사용은 정의 파일 편집을 요구하지 않는다.

**제거율 55%.** 이 범위의 지배적 형태는 `docs/test/e2e.md` **§「AC ↔ e2e 파일 매핑」 표의
「무엇을 단언하나」 칸을 파일 헤더로 옮겨 적은 것**이다. 그 칸은 파일마다 한 문단 길이로 자세하고,
헤더는 그것을 영어로 다시 쓴 것에 가깝다 — 8차 패스가 `web/`에서 만난 것과 같은 사정이며, 여기서는
AC의 **검증 방법**까지 두 번째 출처로 겹친다(여러 헤더가 *「The AC's verification method has four
parts」* 라고 **스스로 자백한다**).

**제거 465줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| e2e 19파일의 `검증 AC:` **아래 헤더 산문** (대부분 8~50 → 1~5) | 「이 파일이 무엇을 단언하나」의 열거 — AC-A1의 두 갈래, AC-C2/C3의 세 분기표, AC-E1/F1의 네 갈래, AC-E6의 배치 주장, AC-F4의 네 항목 | ② §「AC ↔ e2e 파일 매핑」 표의 「무엇을 단언하나」 칸이 **파일마다 같은 것을 더 자세히** 적는다(AC-E6·F4 칸은 한 문단 길이다) · ② 각 `docs/test/*.md` 시나리오와 `docs/prd/*`의 「검증 방법」. **포인터(PRD·시나리오 번호)만 남겼다** |
| 「이건 저 AC의 파일이 소유한다」 류 소유권 문장 12곳 | 「cardinality는 AC-A2's file」·「scrollback은 AC-B3's」·「AC-F3/F5/F6는 각자의 파일이 산다」 | ② 매핑 표가 **AC ↔ 파일 1:1** 자체이고(규칙 1·2), AC-E6·F1·F4 칸은 「여기서 다시 사지 않는다」까지 축자로 적는다 |
| 미단언 분기의 사유 (AC-C2·C3·F4) | 「`idle`에 도달할 방법이 없다 — AC-B1 정책 미확정」·「동결·복원 갈래는 아카이브 전략이 선행」 | ② §「남은 미검증 분기 (공백은 아님)」 표가 **소유 파일·막힌 이유·선행**을 행으로 갖는다. 주석이 스스로 *「Registered as a gap in docs/test/e2e.md」* 라고 적고 있었다 |
| 대역·차단 요인 서술 (`e2e_e2` 헤더 6줄 · `e2e_provider_reachability` 헤더 11줄) | 「대역은 결정적 상수를 답한다 → AC-E4/E5는 그래서 막혀 있다」·「2026-09-04까지 base-url이 라우팅 불가였다」 | ② 충실도 허용목록의 `CLAUDE-PROVIDER` 등재 행이 **잔여 칸에 ⑴⑵로** 같은 것을 적고, 「차단 요인」 ③의 해소 경위는 §「미해소 위반」이 갖는다 |
| 테스트 함수 이름을 되풀이한 doc 주석 24곳 | 「Switching an already-active session is a no-op」(`…_ActiveTargetIsANoop`) 류 — 이름이 이미 문장인 자리 | ① 함수 이름과 바로 아래 단언·실패 메시지. 2차·7차 패스가 `manager_test.go`·`api_test.go`에서 지운 것과 같은 형태다 |
| `Scenario N (AC-…):` 헤더 4곳 (`integration_test.go`) | 「Scenario 1 (AC-A1): creating a session provisions a dedicated data plane pod」 류 | ② `docs/test/architecture.md`의 같은 번호 시나리오 · ① `TestScenario1_CreateProvisionsPod` 이름. **모델 등록 메모가 이 형태를 중복 유형 ②로 지목했던 그 자리다** |
| 시그니처 재진술 doc 12곳 | 「`envOf` returns the value of the named env var」·「`e6RequireAbsent` asserts a container was given no such entry」·「`podCount` counts session pods」 | ① 시그니처 자체. 정책 「유지 대상」 2가 *「그 1줄이 시그니처를 그대로 옮겨 적기만 하면 … 제거 후보」* 로 이 형태를 이름으로 지목한다 |
| 바로 아래 단언이 하는 말 18곳 | 「Ground truth from the cluster: exactly one pod backs the session」·「offset=0 replays everything in order」·「Both invocations answered」 | ① 다음 3~5줄과 그 실패 메시지가 같은 말을 한다 |
| `e2e_smoke_test.go` 헤더 「These run first」 | 「스모크가 먼저 돌아 깨진 배포가 여기서 잡힌다」 | **거짓이었다.** e2e는 `go test -tags=e2e ./test/...`로 돌고 Go는 파일을 알파벳 순으로 읽는데, `e2e_smoke_test.go`는 `e2e_*` 중 **마지막**이다. 정책 「충돌 시 기본 방향」대로 주석이 틀린 것으로 봤다 |

**유지 376줄** — 「지울까」를 검토했다가 남긴 것들. 이 범위는 **유지 비중이 45%로 앞선 패스보다
높다**. e2e·통합 테스트의 주석은 「무엇을 단언하나」(문서가 갖는다)보다 **「왜 이 단언이 무언가를
증명하나」**(어디에도 없다)에 치우쳐 있기 때문이다.

- **음성 단언의 대조군 근거 9종** — 「같은 프로브가 placeholder는 찾으므로 0이 유의미하다」
  (`e2e_e6`·`e2e_f4`), 「대역이 401을 내므로 마커가 오면 주입이 일어난 것」
  (`e2e_provider_reachability`), 「control-plane pod의 exec 실패는 세션 pod에서 성공하는 같은
  exec가 대조군」(`e2e_a1`), 「배경 잡이 실제로 출력했는지를 **샘플링 뒤에** 확인한다」(`e2e_d5`).
  **없으면 그 테스트가 vacuous한지 알 수 없다.**
- **마커가 트리에 유일하다는 전역 사실 3종** — `providerReplyMarker`·`e2e` 프롬프트 마커·
  `deploy/plugin-marketplace-git.yaml` 픽스처. 한 파일만 읽어서는 확인할 수 없다(8차 패스가
  `PROVIDER_REPLY`를 남긴 것과 같은 근거).
- **에코와 실행을 가르는 기법** — `$((40+1))`·`$D4MARK`·`$(pwd)`가 PTY 에코에는 전개되지 않은
  채로 실린다는 사실(`e2e_d2`·`e2e_d4`·`integration_test`). 이것이 「stdin에 주입됐다」와
  「되돌아왔다」를 가르는 유일한 근거다.
- **온클러스터 관측·외부 시스템 제약** — CRIU 특권이 capability set만으로는 AppArmor와 읽기 전용
  `/proc/sys`에 막힌다(2026-07-23) · 복원 pod 이름 경합과 그 해소(2026-07-22) · `:latest`를
  Always로 당기지 않으면 read가 에이전트 404를 낸다 · 파드 종료 유예 기본 30초 · kindnet은
  NetworkPolicy를 집행하지 않는다 · fake clientset에는 kubelet도 UID도 없다 · client-go 리액터의
  `handled=false` 폴스루.
- **왜 상수를 import하지 않고 적어 두는가** 3곳(`e2e_f1`의 라벨 쌍 · `e2e_f4`의 503 메시지 ·
  `e2e_e6`의 컨테이너 이름) — *「import하면 어떤 rename에도 구성상 동의하게 된다」*. 지우면 그
  단언이 왜 중복으로 보이는지 설명할 길이 사라진다.
- **프로브 설계의 함정**(`e2e_e2`의 `claudeArgvProbe` 17줄) — 커맨드 치환이 포크한 서브셸이 부모의
  argv를 물려받아 `$$` 제외만으로는 부족하다는 것, 그리고 **센티널을 대문자 `E2E_`로 적으면
  충실도 허용목록의 seam 토큰이 된다**는 경고. 후자는 다른 게이트와의 상호작용이라 특히 남긴다.
- **갈린 판정 3건** — ⑴ `e2e_c1`의 「envtest가 hermetic single-winner를 소유한다」는 분업 문장:
  매핑 표에 없고 파일 이름(`store_conflict_test.go`)에서 *추론*은 되지만 확인은 아니라, 포인터
  한 줄로 줄여 남겼다. ⑵ 「독립 재조회로 응답이 아닌 상태를 확인한다」류 4곳은 **지웠다** —
  변이 뒤의 두 번째 GET이라는 코드 모양이 그 의도를 그대로 보여준다. ⑶ `client_orchestrator_test.go`의
  「fake는 pod spec을, e2e는 런타임을 산다」는 분업은 `e2e_d1`에도 있어 **한 벌만 남겼다**.

**오검출 5줄 — 등재하되 판정하지 않았다.** `e2e_e2_prompt_invocation_test.go`의 Go raw string
안에 있는 셸 `case` 와일드카드(`*claude-argv-probe-self*)` · `*--include-partial-messages*)` ·
`*)` ×2 · `*claude)`)가 지문 정규식의 `\*[^/]` 가지에 걸린다. `data-plane/Dockerfile`의 `*) echo`,
`web/src/design/tokens.css`의 `* {`와 **같은 사각지대**이고 소관은 이 문서가 아니라 모델 정의
(`asIs.versionScript`)와 게이트 `COMMENT_RE`의 개정이다. 파일은 등재하고 이 5줄만 계산에서 뺐다.

**등재하지 않은 파일 4개.** `control-plane/test/approval_gated_orchestrator_test.go`(경합) ·
`control-plane/test/harness_shared_test.go`(경합) · `web/src/design/README.md`(마크다운 볼드
접두 6줄, 실 주석 0) · `data-plane/Dockerfile`(셸 `case` 1줄, 실 주석 0). 앞의 둘은 **다음 패스의
1순위**이고, 뒤의 둘은 위와 같은 정의 개정 사안이다.
