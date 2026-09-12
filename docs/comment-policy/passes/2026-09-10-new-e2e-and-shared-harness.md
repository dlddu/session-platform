# 2026-09-10 — 신규 e2e 1파일 + 공유 하네스 (판정 130줄)

자매 축(`tbm_session-platform-scenario-e2e`)의 슬라이스가 저작한
`e2e_state_api_6_stream_contract_test.go`(76줄)와, **10·11차가 두 번 연속 유예한**
`harness_shared_test.go`(54줄)를 판정했다. 130줄을 한 줄도 빠짐없이 읽었다.

**왜 하네스를 이제 등재하는가 — 유예 사유를 실측으로 반증했다.** 10·11차가 적은 사유는
「자매 축의 **다음** 저작이 이 공유 하네스에 착지한다」였다. `git log --follow`로 재니 이 파일을
건드린 커밋은 **생성(#23)과 판정 축 개명(#99, `+5/−4`) 둘뿐**이고, 그 뒤의 저작 슬라이스
**둘**(#105가 2파일, #108이 1파일)은 하네스를 **한 줄도** 건드리지 않았다. 즉 유예의 전제였던
「저작은 하네스에 착지한다」가 최근 두 슬라이스에서 거짓이었다. 게다가 등재는 그 파일을 **잠그지
않는다** — R2가 하는 일은 이후 변경을 재판정 대상으로 세우는 것이고, 그것은 등재된 130파일 전부가
지는 같은 의무다. 지금 이 파일을 건드리는 **열린 PR도 0건**이라, 등재가 깨는 남의 유예가 없다.

**제거율 63%** — 9~11차(73%·69%)와 같은 대역이고, 신규 e2e 쪽(76 → 24, **68%**)은 11차와
거의 같다. 지배 형태도 같다: `docs/test/e2e.md` §「시나리오 ↔ e2e 파일 매핑」의 그 파일 행을
헤더로 옮겨 적은 것. **11차가 「매핑 행이 자라 경계까지 삼켰다」고 적은 그 성질이 이번에도
그대로였다** — 헤더의 「경계」 문단 셋(claude-code #4와의 분담 · read의 `lastAccess` 소유 ·
`idle` 갈래 범위 밖)이 매핑 행에 **어순까지 같게** 들어 있다.

**하네스 쪽은 형태가 달랐다.** 여기 있던 것은 매핑 행의 사본이 아니라 **문서의 절 자체**를 옮겨
적은 것이다 — 실행 명령(§「빠른 실행」) · SUT 도달 방식(§「SUT 도달 방식」) · 매칭 단위 규칙
(§「매핑 규칙」 규칙 2) · 「이 파일은 왜 매칭 단위가 아닌가」(§「매칭 단위 밖」의 **이 파일 전용
행**). 넷 다 정본이 `docs/test/e2e.md`에 있고, 하네스는 그것을 영어로 한 벌 더 갖고 있었다.

**제거 82줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| `e2e_state_api_6…`의 파일 헤더 (22 → 4) | 「AC-E3, asserted on the deployed SUT」 · 「이 파일이 사는 것은 passive stream의 상태 계약과 커서 계약이다」 · claude-code #4와의 경계 6줄 · read가 `lastAccess`를 갱신한다는 것은 시나리오 2가 산다 3줄 · `idle` 갈래 범위 밖 3줄 | ② 이 파일의 매핑 행이 **다섯을 전부** 적는다 — 「**passive live output stream 의 상태 계약과 커서 계약**을 배포 SUT 에서」로 시작해 「4는 claude-code 세션의 라이브 왕복 … 6은 상태별 계약과 커서 오류 갈래다 — 여기서는 shell PTY 바이트를 실어 나르므로 UTF-8 경계를 사지 않는다」 · 「read 가 `lastAccess` 를 갱신한다는 것은 `state-api.md#시나리오 2` 의 파일이 이미 사므로 인용만 한다」 · 「**`idle` 갈래는 이 파일에 없다** — … §「남은 미검증 분기」에 등재했다」까지 **어순이 같다**. 「같은 선행이다」도 그 §의 행이 `〃`로 적는다 |
| 상수 블록의 「imported 하지 않은 이유」 (3 → 2) | 「Written out rather than imported so that rewording either fails this file instead of travelling into it」 | ① **정본이 `e2e_f1_workload_type_test.go`의 상수 블록 doc에 한 벌** 있고, 자매 둘(`e2e_architecture_2_1…`·`e2e_state_api_5…`)은 이미 「for the reason e2e_f1 gives」 **포인터만** 쓴다. 이 파일만 세 번째 전문 사본을 갖고 있었다 — 11차가 세운 한 벌 규약으로 포인터로 내렸다 |
| 마커를 산술로 쓴 이유 (4 → 0) | 「Markers are written as arithmetic so the PTY's echo of the command line does not itself contain the token … The "beta must not be replayed" assertion below depends on that distinction」 | ② 매핑 행이 **기법과 그 용도를 함께** 적는다 — 「(마커를 `$((…))` 산술로 써서 PTY 의 명령행 에코가 토큰을 담지 않게 만든 뒤 판정한다)」가 `Last-Event-ID` 절 안에 있다. *11차가 `a5ProbeCommand`의 같은 지식을 남긴 것과 갈린 이유는 하나 — 그때는 매핑 행이 결론만 적었고, 지금은 행이 기법을 삼켰다* |
| `s6Frame`·`next`·`nextEvent`·`s6Output`·`s6ActiveWithOutput` doc (11 → 0) | 「one SSE frame」 · 「A frame ends at a blank line」 · 「returns the next non-comment frame and how many comment frames it skipped」 · 「checks the three things that make its cursor usable」 · 「brings a fresh session to "active, holding a pod, with a settled marker"」 | ① 전부 시그니처·본문 재진술이고 **비공개 식별자**라 Go 관례상 doc 의무도 없다 · ② `s6Output`이 세는 셋은 매핑 행이 「`id` = `data.nextOffset` 이고 `{offset,payloadBase64,nextOffset}` 이며 decoded byte 길이가 커서 이동폭과 정확히 같고」로 적고, ① 바로 아래 세 `t.Fatalf`가 같은 말을 한다(한 줄은 「a reconnecting client resumes from the id, so the two must be one cursor」까지 적는다) |
| `s6Settled` doc (5 → 1) | 「polls until two consecutive full reads agree, so that "the stream sent nothing more" is measured against a quiet shell rather than one still flushing」 | ① **자매 `e2e_state_api_5_wire_validation_test.go`의 `a5Settled` doc이 같은 문장을 갖는다**(11차가 「정온 대기의 이유」로 남긴 그 한 벌). 두 벌째라 포인터로 내렸다. 「하네스가 아니라 로컬에 두는 이유」도 ② §「매칭 단위 밖」이 하네스를 「공용 하네스(HTTP DTO·헬퍼·kube 클라이언트)만 담는다」로 규정한다 |
| 테스트 doc 4곳 (9 → 0) | 「The active branch: … leaves the session exactly as it found it」 · 「The cursor error branches: … explicit reset rather than a silent wait」 · 「The snapshot branch: unlike read/write/switch, the stream does not restore」 · 「The keepalive branch: a heartbeat is neither output nor activity」 | ② 매핑 행이 네 갈래를 **`/`로 나눠 각각** 적는다 — 「왕복 전후로 상태·pod·`lastAccess` 가 셋 다 불변」 · 「조용한 대기가 아니라 **`reset`**」 · 「read/write/switch 와 달리 **복원하지 않는다**」 · 「keepalive 는 output 도 activity 도 아니지만 … **뒤이은 `read(0)` 은 일반 Read API 의미**」 · ① 네 함수 이름이 이미 그 문장이다 |
| 대조군 근거 사본 1곳 (2 → 0) | 「The 200 above is the control that keeps these 400s from being vacuous: the same route on the same session answers a well-formed cursor」 | ② 매핑 행이 괄호로 적는다 — 「(같은 세션·같은 route 가 정상 커서에 200 을 주는 대조군을 같은 케이스 안에 둔다)」. 11차의 「대조군 근거 사본 5곳」과 같은 형태다 |
| keepalive 관측 설계 (2 → 0) | 「Opening at the end of the record leaves the agent with nothing to send, so the only frame it can write next is the heartbeat」 | ① **바로 아래 실패 메시지가 같은 말을 한다** — 「want a keepalive comment — **a settled shell must not produce output events**」 |
| 하네스 패키지 doc (7 → 4) | 실행 명령(`make e2e-up && go test -tags=e2e ./test/...`) · 「Unlike integration_test.go (which mounts the handlers in-process)」 · `E2E_BASE_URL` 기본값 `http://localhost:8080` · 「see deploy/ + scripts/e2e」 | ② `docs/test/e2e.md` §「빠른 실행」이 **그 명령 두 줄을 그대로** 담고 `E2E_BASE_URL`(기본 `http://localhost:8080`)까지 적는다 · ② 문서 첫 문단이 「`make test-integration`이 핸들러를 **인프로세스**로 띄워 검증한다면」으로 대비를 세운다 · ② §「매칭 단위 밖」이 `integration_test.go`를 「빌드 태그 `integration` — 인프로세스 통합」으로 적는다. **블랙박스 성격만 남겼다**(패키지 주석은 유지 대상 2) |
| 하네스 `LAYOUT` 블록 (7 → 0) | 「one test scenario per file」 · 「Every `e2e_*_test.go` … declares exactly one scenario in its header」 · 「that declaration is the machine-checkable scenario↔file mapping」 · 「THIS file is deliberately NOT named `e2e_*`: it holds the shared harness only, so it is not a matching unit」 | ② §「매핑 규칙」 **규칙 2**(「파일 → 시나리오 유일: 매칭 단위 파일은 실재하는 시나리오 정확히 1개만 선언한다」)와 매칭 단위 정의(`control-plane/test/e2e_*_test.go`) · 「매핑의 SSOT는 각 매칭 단위 파일 헤더의 `// 검증 시나리오:` 선언이다 … `check-scenario-mapping.sh`가 셋의 일치를 기계적으로 강제한다」 · ② §「매칭 단위 밖」에 **이 파일 전용 행**이 있다 — 「공용 하네스(HTTP DTO·헬퍼·kube 클라이언트)만 담는다. 파일명이 `e2e_`로 시작하지 않아 매칭 단위가 아니다」 |
| 하네스 `snapshotSession` doc (7 → 2) | 「the same one manual archiving uses」 · 「Only the *automatic* idle->snapshot trigger policy is still open (AC-B1, **`service/session.go` TODO(policy)**)」 · 「gives this suite a deterministic way to reach the snapshot state without waiting out the idle window」 · 「reports ok=false when the SUT predates the endpoint」 | 🔴 **그 좌표는 이미 썩어 있었다** — `service/session.go`라는 파일은 없고 `TODO(policy)`의 정본은 `internal/session/session.go`의 `session.MaxIdle`이다(1차 패스가 doc-tracker의 앵커라 보존한 그 블록). 나머지는 ② §「등재된 seam」의 `TRIG` 항목이 **더 정확히** 적는다 — 「*(현재 0건 — 과거의 `SNAPSHOT-TRIG`는 수동 아카이브 기능이 들어오면서 제품 endpoint로 승격돼 seam 자체가 사라졌다.)*」 · ② 문서 도입부가 「제품 snapshot endpoint(`POST /api/v1/sessions/{id}/snapshot`)를 통해」와 「실제 시간 경계 시드는 skip이다」를 적는다 · ① `ok=false`는 본문의 `StatusNotFound` 분기가 말한다 |
| 하네스 `sessionNamespace` doc (3 → 0) | 「where the deployed control plane provisions its data plane pods — the same namespace it runs in」 · 「Overridable for clusters that place the control plane elsewhere」 | ① **`k8s/deployment.yaml`의 주석이 정본이다** — 「provisions session pods in this same namespace, resolving the namespace from the mounted service account」 · ① overridable은 바로 아래 `os.Getenv` 분기 |
| 하네스 `do`·`getSession`·`writeShell`·`readShellAt` doc (5 → 0) | 「performs a request against the SUT and returns the response plus the fully-read body」 · 「reads a session back from the API」 · 「posts a payload to the session's write endpoint」 · 「reads the session's shell output after offset」 | ① 전부 시그니처 재진술이고 비공개 식별자다. `do`의 「failing the test on transport errors」는 본문의 `t.Fatalf` 셋이 말한다 |
| 하네스 `kubeClient`·`getPodEventually`·`uniqueName`·`eventuallyShellRead`·`client`·`session`·`ptyShellProbe` doc의 재진술 절반 (17 → 9) | 「builds a client … from the ambient kubeconfig (kind writes one) or the in-cluster config」 · 「It also returns the rest config so callers can open exec streams」 · 「fetches a pod, tolerating brief API eventual-consistency」 · 「derives a collision-free session name from the test name」 · 「polls read at offset until ok(payload) holds」 · 「Creating a session provisions a real pod and waits for it to report Ready」 · 「mirrors the JSON the API emits」 · 「prints "comm tty" for every process … whose stdin is a PTY slave」 | ① 각각 바로 아래 본문·시그니처가 그대로 말한다(`clientcmd` → `rest.InClusterConfig` 폴백 · 반환 타입 `(kubernetes.Interface, *rest.Config, bool)` · 재시도 루프 · `t.Name()` + `UnixNano` · `readlink "$d/fd/0"`와 `case … /dev/pts/*`) · ② create가 pod Ready까지 기다린다는 것은 `docs/test/e2e.md` 도입부가 적는다(「create는 pod Ready에 더해 **쉘 도달(Reach…)** 까지 확인한 뒤에야 `active`를 반환한다」) · ① `session` DTO를 로컬 선언한 이유는 위 `e2e_f1` 한 벌 규약 |

**유지 48줄** — 「지울까」를 검토했다가 남긴 것들. 성격은 11차와 같다: **「무엇을 단언하나」는
매핑 행이 갖지만 「이 단언이 왜 vacuous하지 않은가」와 「왜 이 방식이어야 하나」는 어디에도 없다.**

- **두 표면의 인코딩 비대칭 1종 (5줄)** — `read`는 바이트를 **JSON 문자열에 담아** 돌려주므로
  (에이전트 `main.go`) 비-UTF-8 PTY 바이트가 U+FFFD로 바뀌어 stream의 raw 바이트와 byte-exact
  비교가 성립하지 않는다. 매핑 행은 「offset 0 부터 이어 붙인 바이트가 … `read(0)` 의 payload 를
  **바이트로 재현**함」이라는 **결과**를 적지만, 두 표면이 같은 바이트를 다른 인코딩으로 낸다는
  사실은 적지 않는다. 가드가 단언을 조용히 약화시키지 않고 **조건을 로그로 남기는** 이유도 여기 있다.
- **`bufio.Scanner` 토큰 상한 함정 1종 (3줄)** — output 프레임이 `outputStreamChunkBytes`(64 KiB)를
  base64로 실으면 data 줄이 스캐너 기본 상한을 넘어 **계약 실패가 파싱 실패로 위장한다.**
  `s.sc.Buffer(…)` 호출은 상한을 올렸다는 것만 보여 주고 무엇을 막는지는 말하지 않는다.
- **왜 공용 `client`를 쓰지 않는가 1종 (3줄)** — 고정 90초 타임아웃은 **테스트가 기다리는 것과
  무관한 지점에서** 라이브 스트림을 끊는다. 11차의 「왜 기존 헬퍼로 안 되는가 2종」과 같은 형태로,
  코드만 보면 「비슷한 게 하나 더 있다」로만 읽힌다.
- **`EventSource`가 두 커서를 함께 보내는 이유 1종 (2줄)** — 네이티브 재연결은 **원래 URL을
  질의 문자열째** 다시 던지면서 마지막 id를 헤더로 얹는다. PRD(`state-api.md`)는 「`Last-Event-ID`가
  있으면 query보다 우선한다」는 **규칙**을 적지만, 왜 클라이언트가 둘을 동시에 보내는 상황이
  생기는지는 적지 않는다 — 규칙의 전제이고, 복원 경로 넷 어디에도 없다.
- **프로브가 자기를 세지 않는 이유 1종 (3줄)** — `ptyShellProbe`는 TTY 없이 exec되므로 자신도
  명령 치환 서브셸도 `/dev/pts/*`에 걸리지 않는다. **이것이 없으면 매 실행이 최소 1건을 보고한다** —
  11차가 `a5ProbeCommand`에서 남긴 것과 같은, 음성 단언을 가능하게 만드는 설계다.
- **`pods/exec`의 권한 출처 1종 (3줄)** — 이 helper의 exec은 **러너의 kubeconfig**가 허가한 것이지
  SUT의 ServiceAccount가 아니다(`k8s/rbac.yaml`에 `pods/exec`이 없다). 이 구분이 없으면 하네스가
  「제어면도 exec할 수 있다」의 증거로 읽힌다.
- **suite 전역 단언 규약 1종 (3줄)** — 셸 출력 타이밍이 비결정적이라 **모든** 출력 단언이
  containment + eventually다. 한 헬퍼의 동작이 아니라 스위트 전체의 규약이고 문서에 없다.
- **패키지 주석 (4줄)** — Go 관례의 유지 대상. 블랙박스 성격만 남기고 나머지는 `docs/test/e2e.md`
  포인터로 접었다.
- **나머지 (22줄)** — `client` 90초 예산의 근거(pod pull·스케줄) · `uniqueName`이 필요한 이유
  (SUT 상태가 실행 간 유지된다) · `getPodEventually`가 재시도하는 것이 **kube API의 짧은 최종
  일관성이지 기동 중인 pod가 아니라는** 구분 · `kubeClient`의 `ok=false`가 **자격 없는 클러스터를
  가리키는 실행**을 위한 것이라는 점 · 상수·DTO를 import하지 않는 이유의 포인터 3곳 ·
  `snapshotSession`이 제품 endpoint를 쓰는 근거 포인터 · 여는 comment 프레임을 소비하는 것이
  **SSE 프레이밍으로 파싱됐다는 증거**라는 점.
- **갈린 판정 1건 — 남겼다 (4줄).** 신규 파일 헤더에 남긴 「커서 계약 중 둘(`Last-Event-ID` 우선 ·
  past-end reset)을 `claude-code-workload.md#시나리오 4`의 저작 대기 행도 자기 몫으로 열거한다」.
  **두 행을 나란히 놓으면 겹침이 보이므로 문면상 제거 후보**이고, 같은 문단의 나머지(무엇을
  단언하나 · 왜 UTF-8 경계를 안 사나)는 실제로 지웠다. 그런데 **그 겹침을 한 문장으로 말하는 곳은
  어느 행에도 없다** — 시나리오 6의 행은 「6은 상태별 계약과 커서 오류 갈래다」로 경계를 긋되
  겹친다는 사실은 적지 않고, 시나리오 4의 행은 자기 몫만 적는다. 두 행의 **교집합을 독자가 직접
  구성해야** 답이 나오므로 정책 「짚히면 제거, 추론이 필요하면 유지」로 남기고 갈렸다는 사실을
  여기 적는다. 시나리오 4의 파일이 저작되는 날 그 파일과 함께 재판정 대상이다.

**부수 변경 — 「제품 코드 무변경」을 측정했고, 딱 한 줄이 예외다.** 두 파일에서 판정 대상 주석을
걷어낸 나머지(빈 줄 제외, 우측 공백 정규화)가 부모와 비교해 `harness_shared_test.go`는 **완전히
동일**(207줄 불변)하고, `e2e_state_api_6…`는 **358줄 불변이되 한 줄의 공백이 다르다** —
`s6Heartbeat = …`가 `s6Heartbeat  = …`로 한 칸 밀렸다. 상수들 사이의 주석을 걷어내면서 gofmt의
정렬 그룹이 합쳐진 것이고, **내 선택이 아니라 gofmt가 강제하는 형태**다(정렬을 되돌린 사본을
`gofmt -l`에 넣으면 그 파일이 다시 잡힌다 — 실제로 확인했다). 10차 패스가 만난 것과 같은 형태다.
`gofmt -l`은 두 파일 모두 빈 출력이고 `go vet ./...`·`go vet -tags=e2e ./test/`는 rc=0이다.
**단언은 하나도 더하거나 빼지 않았다.**

**자매 게이트를 건드리지 않았음도 쟀다.** `check-scenario-mapping.sh`는 이 슬라이스 전후로
**같은 한 줄**을 낸다 — 「시나리오 37개 = 매칭 파일 23 + 예외 3 + 구현 대기 0 + 저작 대기 11 +
공백 0 (매칭 단위 26개, 비-시나리오 3개)」. 신규 파일의 `// 검증 시나리오:` 선언 줄은 **판정
대상이 아니고**(지시어) 손대지 않았다. `check-fidelity-allowlist.py`·`check-render-fidelity.py`도
rc=0이다 — 다만 초안에서 `snapshotSession` 주석에 `test-only`라는 낱말을 쓰자 fidelity 게이트가
**미등재 seam으로 잡았다**. 그 넷(`E2E_[A-Z0-9_]+`·`.route(`·`mock-exception:`·`test-only`)은
스캔 토큰이므로 산문에 우연히 들이지 말 것.

**이 행으로 남는 미판정은 `shared_volume_test.go` 하나다.** 열린 PR이 이 원장에 소유를 살아 있는
문장으로 선언해 두었고, 그 PR이 이 파일에 **+52줄을 이미 스테이징**하고 있어 지금 등재하면 그
PR을 R2로 즉시 빨갛게 만든다 — 11차까지의 유예 사유가 이쪽에서는 **그대로 살아 있다.**
`web/src/design/README.md`·`data-plane/Dockerfile`의 오탐 7줄은 9~11차와 같은 정의 개정 사안이다.
