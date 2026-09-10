# 2026-09-10 — 자매 축이 들인 신규 e2e 1파일 (판정 80줄)

`tbm_session-platform-scenario-e2e`의 슬라이스(#110)가 저작한
`control-plane/test/e2e_claude_code_6_archive_freeze_test.go`를 판정했다. 80줄을 한 줄도
빠짐없이 읽었다.

**왜 지금인가 — 유예 사유가 예고된 그대로 소멸했다.** 12차 패스(#109)는 이 파일을 「자기 모델의
열린 PR 이 처분되면 범위가 확정된다」는 이유로 유예하며 **유예를 죽이는 사건을 「#109 머지면
80줄」로 못박아 뒀다.** #109 가 머지되고 그 task 가 닫히며 그 사건이 그대로 일어났고, #109 는
이 80줄을 한 줄도 판정하지 않았다(12차 행의 범위는 `e2e_state_api_6_stream_contract_test.go`
+ `harness_shared_test.go`다). 판정 시점에 이 모델의 비-터미널 task 도, 이 모델이 소유한 열린
PR 도 없었다 — **소유자 0**이다.

**등재 밖은 게이트가 보지 않는다.** R2 는 등재된 범위만 재측정하므로, 등재된 적 없는 파일은
주석이 몇 줄이 쌓이든 조용하다. 게다가 이 파일에 남는 유일한 재감지 경로는 **자매 축의 다음
슬라이스**인데, 그것은 판정 대상(분모)만 키우고 판정 완료(분자)는 그대로 두는 방향이라 비율은
**초록인 채로 내려간다.** 11차가 「이 잔여에는 유예 사유가 존재한 적이 없다」로 적은 것과 같은
성질이고, 여기서는 유예가 **있었다가 만료된** 형태다.

**제거율 83%** — 이 원장에서 가장 높다. 이유는 단순하다: 80줄 중 **35줄이 파일 헤더 하나**이고,
그 헤더가 `docs/test/e2e.md`의 매핑 행과 §「남은 미검증 분기」 두 행을 거의 문장 단위로 옮겨
적은 것이었다. 9·10·11·12차가 이름 붙인 바로 그 지배 형태이며, 11차가 관측한 「매핑 행이 그
뒤로 자라 경계까지 삼켰다」가 여기서는 한 단계 더 나아가 있다 — **매핑 행 쪽이 주석보다 더
자세하다.** 예외 등재 행은 주석이 「`manager_test.go`가 순서를 소유한다」고만 적은 자리에서
**그 테스트 함수 세 개의 이름까지** 적는다.

**제거 66줄** — 「이미 말하는 곳」이 제거 근거다.

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| 파일 헤더의 AC 재진술 (3 → 0) | 「`claude-code-workload.md` AC-E5 — and with it the `claude-code` type path of AC-B1/B2/B3, which the shell files only ever drive through CRIU. Driven against the deployed SUT」 | ② 매핑 행의 **AC 칸이 글자 그대로** 「AC-E5 (AC-B1/B2/B3의 `claude-code` 경로)」이고, 「배포 SUT 에서」는 그 행의 첫 문장이다 · ① 남긴 `// 검증 시나리오:` 선언이 그 행으로 가는 기계 판독 색인이다(`check-scenario-mapping.sh`가 강제한다) |
| 「What this file deliberately does NOT assert, and why」 3항목 (23 → 0) | ⓐ 동결 전 대화를 참조하는 프롬프트의 **의미** 판정을 안 사는 이유 10줄(상수 응답 대역 · `deploy/e2e-anthropic-fake.yaml`의 「여기 응답은 결정적 상수」 · 시나리오 5와 같은 벽 · 「사는 것은 그 의미가 타고 갈 배선」) ⓑ durable `preparing`→`committing` 순서를 안 사는 이유 7줄 ⓒ 60분 유휴 트리거를 안 사는 이유 4줄 | ② §「남은 미검증 분기」에 **이 파일 전용 행 둘**이 있다. ⓐ 행은 대역 파일 이름·시나리오 5와의 동일성·해제 조건(provider opt-in smoke)·「사는 것은 배선이고 그쪽은 전부 산다」를 **같은 말로** 적고, ⓑ 행은 「한 API 호출의 in-flight 위상 둘이라 밖에서 관측하려면 전이와 경쟁해야 한다」에 더해 **주석에 없는 것까지** 적는다 — 소유 테스트 **함수 세 개의 이름**과 「제품 표면이 위상을 노출하게 되면 이 행이 소유 파일로 돌아온다」 · ⓒ 는 `lifecycle.md#시나리오 1`의 예외 행이 소유하고, 대체 트리거는 **이 파일의 시나리오 자신**이 실행 단계에 적는다(「유휴 한계 도달(또는 스냅샷 직접 호출)」) |
| 헤더의 「이웃과의 경계」 문단 (6 → 0) | 「architecture.md#시나리오 3's file owns pod reclaim as such and lifecycle.md#시나리오 2·3's files own restore-into-a-new-pod and cursor integrity — all three for *shell* sessions … What is exclusive here is the other strategy」 | ② 매핑 행의 **마지막 문장**이 같은 셋을 같은 순서로 적고 결론까지 같다 — 「…전부 **shell(=CRIU) 경로**로 각자의 파일이 이미 사므로 다시 사지 않는다 — 이 파일이 배타적으로 사는 것은 같은 계약이 **다른 전략**으로 성립하는가다」 |
| 첫 테스트의 doc (9 → 0) | 「The scenario's first expected result is a negative one … Watching for the absence of a dump while the pod is being torn down is a race, so this buys the negative structurally instead: CRIU dumps a process tree from inside the pod and the platform grants that pod the privilege … A claude-code pod never receives it … and the shell session created alongside proves the gate really is on in this SUT」 | ② 매핑 행이 **같은 논증을 같은 구조로** 적는다: 「해체 중인 파드를 훔쳐보는 경쟁 대신 **구조로** 산다: CRIU 는 파드 안에서 프로세스 트리를 뜨고 플랫폼은 그 특권을 `shell` 파드에만 준다(`k8s.WithCheckpointPrivileged`, 배포의 CRIU 게이트 배선) … 대조군이 그 부재를 「게이트가 꺼져서」가 아니라 **이 타입의 결정**으로 못박는다」 · ① 함수 이름이 이미 문장이다(`TestClaudeCodeFreeze_ArchivedTypeNeverCarriesTheCRIUPrivilege`) |
| 둘째 테스트의 doc (2 → 0) | 「The round trip. One freeze, one restore, and every surface the scenario names that a constant-reply provider still lets us observe」 | ① 함수 이름이 세 갈래를 그대로 나열한다(`…ArchiveRoundTripsWorkspaceHistoryAndResume`) · ② 「왕복 세 갈래가 새 파드로 건너감」이 매핑 행의 소제목이고, 「상수 응답이 남기는 것」은 예외 행 ⓐ 가 갖는다 |
| 갈래 표지 · 대조군 표지 4곳 (4 → 0) | 「Control first: the type whose freeze *is* CRIU」 · 「(1) the workspace tree crossed the archive」 · 「(b) offset=0 holds pre- and post-freeze output, in that order」 · 「(a) the pre-freeze cursor still points at exactly the new output」 | ② 매핑 행이 대조군을 적고(「같은 SUT 에 세운 shell 세션이 **특권을 받는다**는 대조군」), (a)·(b)는 **시나리오의 기대 결과가 같은 letter로** 적는다(「(a) `cursorBefore` 델타 read가 유효하게 동작 … (b) `offset=0`에 동결 전·후 출력이 순서대로 모두 포함」) · ① 각 표지 바로 아래 단언과 그 실패 메시지가 같은 말을 한다 |
| 「양성 절반」 문단 (2 → 0) | 「The positive half: the thing the archive is taken from is a mounted tree, present in the very pod that has no way to dump a process image」 | ② 매핑 행의 둘째 갈래가 그대로다 — 「아카이브가 뜰 대상이 실재함 — 그 파드가 `/session` 에 볼륨을 마운트하고 있음」 · ① 바로 아래 `t.Fatalf`가 「the filesystem archive has nothing to capture」로 같은 말을 한다 |
| 프로브 자기검증 문단 (2 → 0) | 「Reading it back before the freeze is what makes the post-restore read an assertion rather than a hopeful cat: the probe itself is known to work」 | ② 매핑 행이 **괄호로 같은 것을** 적는다 — 「(그 프로브가 동결 **전에** 동작한다는 것을 먼저 확인해 음성이 아닌 양성 결과를 자기검증한다)」 · ① 바로 아래 `t.Fatalf`가 「the probe cannot attest to anything after the freeze if it does not work before it」이다 |
| `--continue` 근거 문단 (3 → 0) | 「(2) the resume flag crossed it too. `--continue` on the first invocation of a *new* pod can only come from state that was archived: a fresh pod starts a fresh conversation」 | ② 매핑 행이 어순까지 같다 — 「복원된 파드의 **첫** invocation argv 가 `--continue` 를 단다 — 새 파드는 새 대화로 시작하므로 그 플래그의 출처는 아카이브뿐이다」 · ① 바로 아래 `t.Fatalf`가 「the conversation state is archived with the session and resumed after restore」로 반복한다 |
| 바이트 순서 문단 (3 → 0) | 「Ordering, byte for byte: the delta is the tail of the whole history. This is what 「동결 전·후 출력이 순서대로」 means when both replies are the same constant string and counting them cannot tell which came first」 | ② 매핑 행이 **같은 논증을** 적는다 — 「**델타가 그 꼬리와 바이트 동일** — 두 응답이 같은 상수 문자열이라 개수로는 순서를 못 가르므로 순서는 바이트로 산다」 · ① `strings.HasSuffix`와 그 실패 메시지(「post-freeze output must sit after the archived output, not interleaved with it」) |
| 비공개 헬퍼의 선언 재진술 3곳 (5 → 0) | 「`containerSpec` returns the named container of a pod, or nil」 · 「`execOK` runs one /bin/sh -c command in the session container and fails the test with the stderr if it does not succeed」 · 「`claudeArgvProbeRun` is a probe attached to a pod's process table; `invocations()` blocks for its result and returns the distinct argv lines it saw, in order」 | ① 셋 다 **시그니처와 본문이 그대로 말한다** — 반환 타입과 `return nil`, `[]string{"/bin/sh", "-c", script}`와 `t.Fatalf(… errOut)`, `<-p.done`과 직전 원소 비교(`observed[len(observed)-1] != argv`). 셋 다 **비공개 식별자**라 Go 관례상의 doc 의무도 없다 |
| 마커를 exec 로 심는 이유 (4 → 2) | 「A file in the workspace stands in for the scenario's 「touch marker.txt」 prompt … What the scenario asks of it is unchanged — that it is still there after the freeze」 | ② 대역의 성질은 예외 행 ⓐ 가 갖고(상수 한 줄 → 도구 호출이 만들어지지 않는다), 「동결 전 생성 파일이 남아 있음」은 **시나리오의 기대 결과** 문장이다. **포인터는 남겼다** — 아래 유지 참조 |
| 상수 doc 의 후반 (4 → 2) | 「Both are asserted here as the *wire* the archive is taken from: the volume mounts at the parent, and the archived tree is rooted at the state dir」 | ② 매핑 행의 「그 파드가 `/session` 에 볼륨을 마운트하고 있음」 · ① 바로 그 마운트를 도는 루프와 `t.Fatalf`. **앞 절반은 남겼다** — 아래 유지 참조 |

**유지 14줄** — 「지울까」를 검토했다가 남긴 것들.

| 위치 | 남긴 것 | 왜 복원되지 않나 |
| --- | --- | --- |
| 파일 헤더 (3줄) | 매핑 행과 §「남은 미검증 분기」로 가는 **포인터** | 운영 규칙 그대로다 — 사본은 지우고 포인터는 남긴다. 이 파일에서 사라진 35줄이 전부 그 두 자리로 가므로, 길을 지우면 다음 독자가 같은 사본을 다시 쓴다 |
| 상수 블록 doc (2줄) | 두 경로가 플랫폼의 무엇에 대응하는가 — `CLAUDE_CODE_STATE_DIR`, 그리고 볼륨이 **상태 루트의 부모**에 마운트된다는 것 | 리터럴 `"/session"`·`"/session/state/workspace"`만 보고는 어느 것이 컨트롤 플레인이 주는 env 이고 어느 것이 그 안의 작업 디렉터리인지 알 수 없다. cross-module 좌표라 이 레포의 어느 문서도 한 덩어리로 갖지 않는다 |
| 마커를 exec 로 심는 자리 (2줄) | 「프롬프트가 아니라 exec 로 심는다」와 그 이유가 **예외 등재에 있다**는 포인터 | 코드만 보면 `printf > marker` 는 시나리오가 말하는 「touch marker.txt 프롬프트」와 다른 일로 읽힌다. 대체가 **의도된 것**이라는 사실은 코드에 없고, 그 근거는 다른 문서에 있다 |
| read 로 복원을 트리거하는 이유 (3줄) | 「복원과 다음 invocation 을 별개 사건으로 두어야 argv 프로브가 **프롬프트가 돌기 전에** 붙는다」 | write 로 복원해도 통과하는 것처럼 보이는 형태라 다음 편집이 조용히 바꾸기 쉽다. 바꾸면 프로브가 첫 invocation 을 놓쳐 `--continue` 단언이 **거짓 음성**이 된다. 순서 계약이고 코드 형태에는 보이지 않는다 |
| 두 read 사이 `time.Sleep` (2줄) | 「응답이 들어오면 버퍼가 가라앉는다 — 아래 두 read 가 같은 커서를 보도록 invocation 의 꼬리 프레임에 시간을 준다」 | 근거 없는 `sleep` 은 플레이크 땜질로 읽혀 다음 사람이 지운다. 지우면 `full.NextOffset != delta.NextOffset` 로 간헐 실패한다. 왜 필요한지는 어디에도 없다 |
| `waitPodReclaimed` (2줄) | 「기본 30초 termination grace 를 지나서까지 폴링한다」 | 90초라는 상수만으로는 그 수가 무엇을 넘기려는 것인지 알 수 없다. **k8s 런타임의 기본값**이라 이 레포의 코드·문서 어디에도 없다 |

**코드 동작은 변하지 않았다.** 주석 줄을 걷어낸 뒤의 바이트 대조로 확인했다 — 비-주석·비-공백
263줄이 판정 전후 동일하다.

## 다음 패스의 1순위

이 행으로 `control-plane/test/`에 남는 미판정은 **`shared_volume_test.go` 하나**이고, 그것은
`tbm_session-platform-docs-impl`의 열린 PR 이 **살아 있는 소유 선언**을 갖는다(10·11·12차가
같은 이유로 남겼다 — 편입은 파일 목록 관리가 아니라 판정 책임의 주장이라 남의 선언 위에 쓰지
않는다). 그 선언이 풀리면(머지되거나 닫히면) 그때가 이 파일의 차례다.

스캔 범위에 남는 나머지는 오탐 7줄뿐이다 — `web/src/design/README.md`(마크다운 볼드 접두 6줄,
실 주석 0)·`data-plane/Dockerfile`(셸 `case` 1줄, 실 주석 0). 9차 패스부터 같은 자리에 있고,
고치는 자리가 지문 패턴이라 **모델 정의 개정 사안**이다.
