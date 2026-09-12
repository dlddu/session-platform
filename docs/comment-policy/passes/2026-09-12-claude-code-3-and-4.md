# 2026-09-12 — 자매 축이 09-11 에 들인 신규 e2e 2파일 (판정 175줄)

`tbm_session-platform-scenario-e2e` 의 슬라이스 둘이 저작한
`control-plane/test/e2e_claude_code_3_serial_queue_test.go`(103줄)와
`control-plane/test/e2e_claude_code_4_stream_reconnect_test.go`(72줄)를 판정했다.
175줄을 한 줄도 빠짐없이 읽었다.

**왜 지금인가 — 이 모델에서 처음으로 소유자가 0이 됐다.** 두 파일은 09-11 에 각각 #113·#111 로
들어온 뒤 어느 등재 행에도 속한 적이 없다. 13차 패스(#117)는 같은 날 들어온 **다른** 파일
(`e2e_claude_code_6_archive_freeze_test.go`)을 판정하고 닫혔고, 그 task 가 `succeeded` 로 가면서
이 모델의 비-터미널 task 도 이 모델을 소유자로 지목하는 열린 PR 도 **0건**이 됐다. 12·13차가
「유예 사유가 소멸하면」을 조건으로 남긴 형태와 달리, 이 둘에는 **유예 사유가 존재한 적이 없다**
(11차가 같은 말을 적은 자리와 같은 성질이다) — 단지 앞 슬라이스들이 매번 다른 파일을 집었을 뿐이다.

**게이트는 이 잔여를 구조적으로 못 본다.** R2 는 **등재된 범위만** 재측정하므로 등재된 적 없는
파일은 주석이 몇 줄 쌓이든 rc=0 이다. 실제로 이 175줄이 서 있는 동안 `check_comment_policy.py` 와
CI 전 잡은 초록이었다. 게다가 분모(판정 대상)는 자매 축이 새 e2e 를 들일 때마다 커지므로,
미루면 비율은 **초록인 채로 내려간다.**

**제거율 81%**(142/175). 지배 형태는 9~13차가 이름 붙인 바로 그것 — `docs/test/e2e.md` 매핑 행의
사본 — 이고, 여기서는 13차가 관측한 「매핑 행 쪽이 주석보다 더 자세하다」가 **두 파일 모두에서**
성립한다. 두 헤더 합계 66줄 중 **59줄**이 매핑 행·저작 슬라이스 노트·PR 본문 셋 중 하나 이상에
거의 문장 단위로 남아 있었다. 새로 관측된 형태가 하나 있다: **파일 헤더가 자기 PR 을 현재형으로
가리킨다**(「until this PR the SUT could not produce one」) — 머지되는 순간 그 지시어는 가리킬 곳을
잃는다. 낡음은 이 모델의 판정 표면이 아니지만 **제거 근거를 강화한다**는 정책 문면 그대로다.

**제거 142줄** — 「이미 말하는 곳」이 제거 근거다. 좌표는 심볼·행 이름·절 이름으로 적는다.

## `e2e_claude_code_3_serial_queue_test.go` (103 → 16, 제거 87)

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| 파일 헤더 (45 → 4) | ⓐ AC-E2 와 「배포 SUT 의 `CLAUDE-PROVIDER` 대역에 대고」 ⓑ 시나리오가 무엇에 관한 것인가(두 write · 겹치지 않음 · 순서 누적) ⓒ **창이 저절로 생기지 않는다**는 실측 8줄(200000 룬 · 600013 바이트 · 64 델타 · 버퍼 600148 · 「크기로는 시간을 못 산다」) ⓓ 델타 간격을 넣은 경위와 `64×150ms` ≈ 9.6초 대 1초 미만 ⓔ 창을 만든 효과 둘(전제를 측정으로 세운다 · 순서 단언이 공허하지 않다, 90배·20배) ⓕ 「What this file deliberately does NOT assert」 4항목 | ② 매핑 행의 **AC 칸이 글자 그대로** 「AC-E2 (직렬 큐·출력 순서)」이고 첫 문장이 「…를 배포 SUT 에서」다. ⓒ·ⓓ·ⓔ 는 그 행이 **같은 수치로 같은 순서로** 적는다(「이 SUT 에서 그 창은 저절로 생기지 않는다 … 버퍼 600148 = 두 응답 모두 완료 … 크기로는 시간을 못 산다 ⇒ … 선택 넷째 칸 … 최소 9.6초 … 둘째가 **90배 짧고 20배 빠르므로** … 이 순서 단언은 **공허하지 않다**」). ⓕ 4항목도 같은 행이 소유한다 — 「**비블로킹 반환 자체는 이 파일에 없다**」가 `claude_test.go` 의 `TestClaudeWriteIsNonBlockingAndSerial` 을 **함수 이름까지** 적고, 「시나리오 2 와의 경계」가 exact argv·원샷 수명을, 「시나리오 4 와의 경계」가 커서 표면을 적는다. 응답의 *의미* 잔여는 충실도 절 `CLAUDE-PROVIDER` 행이 갖는다 · ③ 같은 내용이 #113 본문의 「그 창 위에서 사는 것」 절과 표에 있다 · ① 남긴 `// 검증 시나리오:` 선언이 그 행으로 가는 기계 판독 색인이다(`check-scenario-mapping.sh` 가 강제한다). **포인터는 남겼다** — 아래 유지 참조 |
| 상수 블록 doc 의 앞뒤 (4 → 2) | 「The directive pair and the replies it produces」 · 「MAX_DELTAS and MAX_DELAY_MS in `deploy/e2e-anthropic-fake.yaml` are the ceilings on the first one's duration」 | ① 앞 문장은 바로 아래 상수 여덟 개가 그대로 말한다 · ① 상한 포인터는 **같은 파일의 실패 메시지**가 이미 적는다(`cc3IssueOverlapped` 의 `t.Fatalf` — 「raise cc3SlowDelayMs (MAX_DELAY_MS in deploy/e2e-anthropic-fake.yaml is the ceiling)」) · ② 대역 헤더가 「최대 응답」과 상한을 소유한다. **가운데 절반은 남겼다** — 아래 유지 참조 |
| 「64 deltas, 150ms apart … ~9.6s」 (2 → 0) | 첫 프롬프트의 지시자가 만드는 하한 | ① 바로 아래 `cc3SlowFloorMs = cc3SlowDeltas * cc3SlowDelayMs` 가 **그 곱을 코드로 적는다** · ② 매핑 행의 「`64×150ms` = **최소 9.6초**」 |
| `cc3SlowBytes` 산식 주석 (1 → 0) | 「len("ccthree-slow:") + 3 bytes per filler rune (U+AC00)」 | ① 바로 위 `cc3SlowPrefix = "ccthree-slow:"` 를 세면 13 이다 · ② 대역의 `FILLER` 상수와 본문 합성(`found[1] + ":" + FILLER.repeat(runes)`)이 `deploy/e2e-anthropic-fake.yaml` 에 있다 |
| 「No fourth field」 (1 → 0) | 둘째 지시자에 넷째 칸이 없다는 것과 그래서 가장 빠르다는 것 | ① 바로 아래 리터럴 `"e2e-reply:ccthree-fast:40:1"` 이 칸 셋뿐이다 · ② 대역 헤더의 「생략하면 0」 · ② 매핑 행의 「둘째가 **1초 미만**」 |
| `cc3Terminators` doc (3 → 1) | 「closes a message with a newline when the assistant text does not already end in one」 · 「two invocations run here」 | ① `claude_stream.go` 의 `finishMessage`/`messageNewline` 이 그 규칙 자체다(포인터가 이미 그것을 가리킨다 — 운영 규칙대로 **사본만** 걷었다) · ① 「둘」은 상수값 `2` 와 이 파일이 보내는 프롬프트 수가 말한다 |
| `cc3IssueOverlapped` doc 전체 (10 → 0) | ⓐ 선언 재진술 3줄(「sends the slow prompt and then the fast one … returns the buffer as it stood the moment the second write returned」) ⓑ 「전제이지 형식이 아니다」 6줄 | ⓐ ① 본문이 `writePromptOK`(slow) → `writePromptOK`(fast) → `readShellAt` → `return` 그대로이고, **비공개 식별자**라 Go 관례상 doc 의무도 없다 · ⓑ ② 매핑 행이 괄호까지 같은 말을 한다 — 「전제를 가정이 아니라 **측정**으로 세운다(전제가 깨지면 테스트가 그렇게 말하며 실패한다 — **조용히 통과하지 않는다**)」 · ① 바로 아래 `t.Fatalf` 가 같은 논증을 7줄로 다시 적는다 |
| 「The negative half of the same moment」 (2 → 0) | 둘째 프롬프트가 첫 것 **옆에서** 시작되면 안 된다는 것 | ① 바로 아래 `t.Fatalf` 가 글자 그대로다 — 「it must queue behind the running invocation, **not run beside it** (AC-E2)」 |
| `cc3Settled` doc 의 선언 재진술 (6 → 3) | 「waits for both replies to land and for the buffer to stop moving」 · 「the slow reply arrives as 64 deltas」 | ① 함수 본문이 그 둘을 그대로 한다(`eventuallyClaudeOutput` + 정지 대기 루프), 비공개 식별자 · ① 「64 델타」는 `cc3SlowDeltas` 상수다. **경쟁 계약 쪽은 남겼다** — 아래 유지 참조 |
| 첫 테스트 doc (3 → 0) | 「The headline: 두 응답이 write 순서대로 놓인다 — 90배 크고 20배 느린 것이 먼저」 | ① 함수 이름이 이미 문장이다(`TestClaudeSerialQueue_ReadZeroAccumulatesInWriteOrder`) · ② 매핑 행이 같은 두 비율을 같은 말로 적는다 |
| 둘째 테스트 doc (8 → 0) | ⓐ 「같은 겹침을 파드 안에서 본다 — CLI 는 여전히 두 번 동시에 돌지 않는다」 ⓑ 시나리오 2 의 파일과 갈리는 이유 5줄 | ⓐ ① 함수 이름(`…InvocationsNeverRunConcurrently`) · ② 매핑 행의 「파드 안 프로세스 테이블에서 동시 `claude` 수가 **1 을 넘지 않고**」 · ⓑ ② 같은 행의 「시나리오 2 와의 경계」 문단이 **그 파일 헤더의 문구를 인용하면서** 같은 결론에 닿는다 — 「그 파일 헤더가 「invocation 이 write 왕복보다 느리지 않다」고 적어 **겹칠 창이 비어 있을 수 있으므로**, 여기서는 창을 만든 뒤에 센다」 |
| 프로브 계수 단위 문단 (7 → 0) | 「한 줄은 *변화*마다 나온다 → 세는 단위는 상승 에지가 아니라 서로 다른 argv」 + 50ms 표본 안의 직렬 인계 실측 | ① **프로브 상수 자신의 doc** 이 첫 문장을 소유한다(`e2e_e2_prompt_invocation_test.go` 의 `claudeArgvProbe` — 「Prints one line per *change*: "<count>\t<argv>"」), 같은 패키지다 · ② 매핑 행의 ⚠️ 문단이 `1 <argv A>` → `1 <argv B>` 모양과 「에지로 세면 직렬 인계를 invocation 1 회로 오독한다(실측)」까지 적는다 · ③ #113 본문이 그 프로브 출력 전문을 싣는다 |
| 「The same ordering … one layer down」 (3 → 0) | 프로세스 순서가 버퍼 순서보다 강한 증거라는 것 | ② 매핑 행의 마지막 절이 어순까지 같다 — 「버퍼 순서는 원리상 실행 **뒤**의 무엇인가가 정렬했을 수도 있지만 프로세스 순서는 그럴 수 없다」 · ③ #113 본문의 같은 문장 |

## `e2e_claude_code_4_stream_reconnect_test.go` (72 → 17, 제거 55)

| 위치 | 제거한 것 | 이미 말하는 곳 (복원 경로) |
| --- | --- | --- |
| 파일 헤더 (21 → 3) | ⓐ AC-E3 와 「배포 SUT 의 `CLAUDE-PROVIDER` 대역에 대고」 ⓑ 헤드라인이 왜 저작만으로는 안 됐는가 8줄(상수 ASCII 32바이트 · 64 KiB 미도달 · **ASCII 는 어디서 잘라도 code-point 경계** · 대역이 이제 지시자로 응답을 빚는다) ⓒ 「does NOT assert」 2항목(응답의 *의미* · 16 MiB truncation 마커) | ② 매핑 행의 AC 칸이 「AC-E3 (live stream·재접속·read reconcile)」이고 첫 문장이 「…를 배포 SUT 에서」다 · ⓑ 는 ② 저작 슬라이스 4 노트가 **원인까지 같은 말로** 적고(「공급자 대역이 프롬프트와 무관한 상수 ASCII 32바이트를 델타 하나로 낸다는 성질 때문에 **저작해도 살 수 없는** 것이었다. 그래서 파일을 쓰기 전에 대역을 넓혔다」), ③ #111 본문의 표가 「**ASCII 는 어디서 잘라도 code-point 경계**라 룬을 무시하는 청커와 존중하는 청커가 구별되지 않는다」로 같은 논증을 적는다 · ⓒ 첫 항목은 매핑 행의 「**응답의 *의미*는 이 파일에 없다** — … 대화 연속성은 `claude-code-workload.md#시나리오 5` 의 예외가 계속 소유한다」, 둘째 항목은 **`claude-code-workload.md#시나리오 8` 의 저작 대기 행**이 통째로 소유한다(같은 벽을 이름까지 적고 「이 파일은 (b)~(e) … truncation 마커의 live append」를 자기 몫으로 선언한다). **포인터는 남겼다** — 아래 유지 참조. ⓑ 의 「until this PR」 은 머지된 지금 가리킬 PR 이 없다 |
| 상수 블록 doc 의 앞줄 (3 → 2) | 「The directives this file sends to the stand-in, and the replies they produce」 | ① 바로 아래 상수 열 개가 그대로 말한다. **뒷문장은 남겼다** — 아래 유지 참조 |
| `cc4AlphaBytes` 산식 주석 (1 → 0) | 「len("alpha:") + 3 bytes per rune」 | ① 바로 위 `cc4AlphaPrefix = "alpha:"` · ② 대역의 `FILLER` 와 본문 합성 |
| 「The small shapes」 표제 (3 → 2) | 표제 한 조각 | ① 바로 아래 상수 이름들이 작은 모양임을 말한다(`cc4SmallRunes`). **경쟁 계약 쪽은 남겼다** — 아래 유지 참조 |
| `cc4Filler` doc (1 → 0) | 「The filler rune the stand-in repeats (U+AC00), three bytes wide」 | ① 바로 아래 리터럴이 `"가"` 다 · ② 대역의 `FILLER` 상수가 같은 문자다 |
| `cc4FirstChunkEnd` doc (7 → 1) | 65535 의 산술 유도 6줄(「룬은 6, 9, 12 … 에서 시작 · 65536 은 그 룬의 둘째 바이트 · `scrollback.streamChunk` 가 마지막 완전한 룬까지 물러선다 · ASCII 였다면 둘 다 65536 이라 아무것도 사지 못한다」) | ② 매핑 행이 **같은 유도를 같은 숫자로** 적는다(「64 KiB(65536)가 아니라 **65535** 에서 끊긴다 — 그 바이트가 룬의 시작이고 65536 은 **연속 바이트**임을 … (ASCII 본문이면 둘 다 65536 이라 이 단언은 아무것도 사지 않는다)」) · ③ #111 본문이 `65535 - 6 = 65529` 까지 적고 `validUTF8PrefixAtMost` 로 물러선다는 것도 적는다 · ① 아래 두 단언(`buf[cc4FirstChunkEnd]&0xC0` · `buf[cc4ChunkLimit]&0xC0`)과 그 실패 메시지가 같은 유도를 실행으로 옮긴 것이다. **포인터 1줄은 남겼다** |
| `cc4MessageTerminator` doc (3 → 1) | 「newline when the assistant text does not already end in one」 · 「an invocation contributes its reply plus at most that one byte」 | ① `claude_stream.go` 의 `finishMessage` 가 규칙 자체다(포인터는 남겼다) · ① 상수값 `1` 과 바로 아래 `extra > cc4MessageTerminator` 검사가 「at most one」을 말한다 |
| `cc4Settled` doc 의 선언 재진술 (8 → 4) | 「writes one prompt and waits for its whole reply to land *and stop growing*, returning the final buffer」 · 「The reply arrives as a dozen deltas」 | ① 본문이 그대로다(`writePromptOK` → `eventuallyClaudeOutput` → 정지 대기 루프 → `return`), 비공개 식별자 · ① 「a dozen deltas」는 지시자 상수 `"e2e-reply:alpha:25000:12"` 의 넷째 필드다. **경쟁 계약 쪽은 남겼다** — 아래 유지 참조 |
| 첫 테스트 doc (2 → 0) | 「The whole scenario in one place: 청크될 만큼 크고, 플랫폼이 골라야 했던 경계에서 잘리고, 정확히 한 번 투영된다」 | ① 함수 이름(`TestClaudeStream_ChunkCutsBackOffToACodePointBoundary`) · ② 매핑 행의 헤드라인 문장이 세 갈래를 그대로 나열한다 |
| 「The reply arrived intact」 (2 → 1) | 「The reply arrived intact」 · 「truncated … body upstream」 | ① 바로 아래 `bytes.Equal` 과 그 실패 메시지가 「intact」를 말한다 · truncation 은 위 ⓒ 대로 시나리오 8 의 행이 소유한다. **재인코딩 배제 근거는 남겼다** — 아래 유지 참조 |
| 「One invocation, one projection」 (3 → 0) | `result` 레코드가 델타를 다시 투영하지 않는다는 것 | ② 매핑 행의 마지막 갈래가 글자 그대로다 — 「응답 라벨이 버퍼에 **정확히 1회** — raw stream-json 의 final/result 레코드가 이미 부분 델타로 낸 텍스트를 다시 투영하지 않는다는 것을 계수로 산다」 · ② PRD AC-E3 의 「raw JSONL과 delta를 중복하는 최종 result는 누적하지 않는다」 · ① 바로 아래 `t.Fatalf` 가 같은 말을 한다 |
| 「Non-vacuity, proved from the bytes themselves」 (2 → 0) | 한계선이 문자 중간에 떨어지므로 백오프가 no-op 이 아니라는 것 | ② 매핑 행이 **결론 단어까지 같다** — 「한계선이 실제로 문자 중간에 떨어졌음을, 즉 백오프가 **일어난 결정**이었음을 증명한 뒤에 그 숫자를 단언한다」 · ① 두 단언의 실패 메시지가 「the boundary assertion below would be meaningless」 · 「otherwise this test proves nothing about the back-off」 |
| 둘째 테스트 doc (2 → 0) | 「query 가 뭐라 하든 마지막으로 본 id 에서 재개하고, 이미 가진 바이트는 다시 오지 않는다」 | ① 함수 이름이 두 절을 그대로 나열한다(`…LastEventIDBeatsQueryAndResumesWithoutReplay`) · ② 매핑 행의 같은 갈래 |
| 「The query asks for the very beginning」 (2 → 0) | 헤더가 query 를 이겨야 하는 이유 | ② PRD AC-E3 의 「재연결 요청에 `Last-Event-ID`가 있으면 query `offset`보다 우선한다」 · ① 바로 아래 `t.Fatalf` 가 「Last-Event-ID must beat the query cursor」 |
| 「Everything the resumed stream delivers」 (2 → 0) | 이후 오는 것은 새 바이트뿐이라는 것 | ② 매핑 행의 「그 뒤 도착한 두 번째 프롬프트의 응답이 정확히 1회, 앞선 응답의 머리 바이트는 **재전송되지 않음**」 · ① 아래 두 단언과 그 실패 메시지 |
| `mustPayload` doc (2 → 0) | 「decodes an output frame, failing on a reset — the caller here is only ever positioned inside the buffer」 | ① 본문이 두 줄이고 `s6Output` 이 reset 에서 실패시킨다 · ① 호출자가 `d1.NextOffset` 에서 재개하므로 버퍼 안이라는 것이 코드에 보인다 · **비공개 식별자**라 doc 의무 없음 |
| 셋째 테스트 doc (2 → 0) | 「끝을 넘는 커서는 데이터가 아니라 신호로 답하고, 답하는 것은 접근이 아니다」 | ① 함수 이름(`…PastEndResetsWithoutTouchingLastAccess`) · ② 매핑 행의 「현재 길이를 넘는 커서는 payload 없는 **`reset`** 이고 … 그 신호 자체는 `lastAccess` 를 바꾸지 않음」 |
| 넷째 테스트 doc (3 → 0) | 「read(0) 은 디코더를 못 믿게 된 SPA 의 복구 경로 — 전체 이력을 순서대로, 그 커서는 비어야 한다」 | ② PRD AC-E3 가 그 복구 절차를 문장으로 적는다(「브라우저는 미완성 decoder state를 버리고 `POST /read`의 `offset=0` 전체 이력으로 콘솔을 교체한 뒤 …」) · ② 매핑 행의 「`read(0)` 이 두 응답을 **쓴 순서대로** 담고 마지막 커서 read 는 **빈 델타**」 · ① 함수 이름 |

**유지 33줄** — 「지울까」를 검토했다가 남긴 것들.

| 위치 | 남긴 것 | 왜 복원되지 않나 |
| --- | --- | --- |
| `#3` 파일 헤더 (4줄) | 매핑 행과 대역 헤더로 가는 **포인터** 둘 | 운영 규칙 그대로다 — 사본은 지우고 포인터는 남긴다. 이 파일에서 사라진 41줄이 전부 그 두 자리로 가므로, 길을 지우면 다음 독자가 같은 사본을 다시 쓴다. 지시자 문법의 정본이 레포의 **다른 디렉터리**(`deploy/`)에 있다는 것은 특히 찾기 어렵다 |
| `#4` 파일 헤더 (3줄) | 매핑 행과 **저작 슬라이스 노트**로 가는 포인터 | 같은 이유. 「대역을 먼저 넓혀야 했다」의 정본이 매핑 행이 아니라 그 아래 슬라이스 노트에 있어, 행만 읽으면 닿지 않는다 |
| `#3` 상수 블록 doc (2줄) | 바이트 수를 **서로에게서 유도하지 않고 각각 적어 둔** 이유 | 코드만 보면 `13 + 3*cc3SlowRunes` 는 `len(cc3SlowPrefix) + …` 로 「정리」하고 싶은 형태다. 그렇게 하면 대역과 이 파일이 **서로에게 동의하며** 함께 틀릴 수 있다는 것이 이 주석이 막는 것이고, 그 의도는 레포 어디에도 없다 |
| `#4` 상수 블록 doc (2줄) | 같은 계약(크기를 계산하지 않고 적는다) | 같은 함정이 `6 + 3*cc4AlphaRunes` 에 그대로 있다. **자매 파일의 같은 주석은 복원 경로가 아니다** — 넷 중 어디에도 해당하지 않고, 한쪽을 읽는 사람이 다른 쪽을 보지 않는다 |
| `#3` `cc3Terminators` (1줄) | `finishMessage` 로 가는 cross-module 포인터 | 상수값 `2` 만 보고는 이 수가 **투영기의 규칙**에서 나온다는 것을 알 수 없다. data-plane 쪽 심볼이라 control-plane 테스트를 읽는 사람의 시야 밖이다 |
| `#4` `cc4MessageTerminator` (1줄) | 같은 포인터 | 같은 이유 |
| `#4` `cc4ChunkLimit` doc (3줄) | 「import 하지 않고 **베껴 둔** 이유 — 저쪽이 바뀌면 이 파일이 깨지라고」 | 값이 같으므로 다음 편집이 「중복이니 import 하자」로 읽기 쉽다. 그렇게 하면 한계가 바뀔 때 이 테스트가 **조용히 따라가** 아무것도 사지 않게 된다. 의도적 복제라는 사실은 코드 형태에 보이지 않는다 |
| `#4` `cc4FirstChunkEnd` (1줄) | 「유도는 매핑 행에 있다」는 포인터 | 65535 는 그 자체로 마법의 수다. 포인터가 없으면 다음 사람이 64 KiB 오타로 보고 「고친다」 |
| `#3` `cc3Settled` (3줄) | 종결 개행이 마지막 델타 **뒤에** 온다는 것과, 그래서 바이트 임계에서 멈춘 대기가 아직 자라는 버퍼를 돌려준다는 것 | 근거 없는 정지 대기 루프는 플레이크 땜질로 읽혀 다음 사람이 지운다. 지우면 위치 단언이 간헐 실패한다. 이 순서 계약은 코드 형태에 보이지 않고 매핑 행도 「settle 된」이라고만 적는다 |
| `#4` `cc4Settled` (4줄) | 같은 계약 + 「고정 버퍼를 라이브 스트림과 비교하므로 움직이면 계약이 경쟁이 된다」 | 같은 이유. 이 파일 쪽은 비교 상대가 SSE 스트림이라 위험이 한 겹 더 있다 |
| `#4` `cc4SmallRunes` 옆 (2줄) | 작은 응답의 바이트 수가 **도달 가능하기만 한 임계**여선 안 되는 이유 | 40 룬이라는 작은 수는 「아무 값이나」로 읽힌다. 그것이 **대기 조건의 하한**으로 쓰이고 있고 느슨하면 아래 커서가 꼬리와 경쟁한다는 것은 코드 형태에 없다 |
| `#3` 위치 단언 옆 (2줄) | 상대 순서(`slow < fast`)가 아니라 **위치**로 사는 이유 | 매핑 행은 「첫 응답을 [0, 12013) 에, 둘째를 그 뒤에」라고 **결과**만 적는다. 더 약한 단언이 왜 기각됐는가 — 끼어든 두 응답도 `slow < fast` 를 만족한다 — 는 어디에도 없고, 그것을 모르면 다음 편집이 단언을 약화시키면서 「같은 것」이라고 생각한다 |
| `#3` 계수 단언 옆 (2줄) | 위치 검사를 통과하면서도 재생된 프롬프트가 있을 수 있다는 것 | 같은 성질 — 계수 단언이 **왜 위치 단언 위에 더 필요한가**. 행은 세 단언을 나열하지만 그 사이의 함의는 적지 않는다 |
| `#3` 프로브 부착 순서 (2줄) | 「표본 수집이 write 보다 **먼저** 시작돼야 첫 invocation 을 놓치지 않는다」 | `go func` → `time.Sleep(3s)` → write 순서는 보이지만 그것이 **계약**이라는 것은 안 보인다. 순서를 바꾸면 프로브가 첫 invocation 을 놓쳐 argv 순서 단언이 **거짓 음성**이 된다 |
| `#4` 바이트 비교 (1줄) | 부분 문자열 검색이 아니라 바이트 비교인 이유(재인코딩 배제) | 바로 아래에 `strings.Count` 를 쓰는 단언이 있어 두 방식이 나란히 서 있다. 왜 여기만 `bytes.Equal` 인가는 코드가 말하지 않는다 |

## 판단이 갈린 것

- **`cc3Settled`·`cc4Settled` 의 경쟁 계약이 두 파일에 각각 남았다.** 내용이 거의 같아 한쪽을
  지우고 다른 쪽을 가리킬까를 검토했다. 남긴 이유: **주석은 복원 경로 넷 중 어디에도 없다.**
  「자매 파일이 말한다」로 지우면 그 자매 파일이 다음 판정에서 지워질 때 지식이 사라지고, 그때
  아무 게이트도 반응하지 않는다. 두 문장을 서로 다르게 적어 어느 쪽도 다른 쪽의 사본이 아니게 했다.
- **`#3` 계수 단언 옆 2줄은 형태를 바꿔 남겼다.** 원문의 앞 절반(「Nothing else got in, and nothing
  ran twice」)은 매핑 행이 소유하므로 걷고, 뒤 절반(위치 검사만으로는 재생을 못 잡는다)만 남겼다.
  위치도 옮겼다 — 원문은 길이 검사 위에 있었는데 그 논증이 가리키는 것은 **계수** 단언이다.
- **`#4` 헤더의 「until this PR」 은 낡음이 아니라 중복으로 걷었다.** 이 모델은 정확성을 보지 않는다.
  다만 낡은 지시어라는 사실이 「이 문단의 자리는 PR 본문이다」를 확증했다.

**코드 동작은 변하지 않았다.** 주석 줄을 걷어낸 뒤의 대조로 확인했다 — 비-주석·비-공백 줄이
`#3` 175줄 · `#4` 184줄로 판정 전후 **개수와 내용이 같고**, 유일한 차이는 상수 정렬 공백 2곳이다
(주석이 갈라 두던 `const` 블록이 합쳐지며 `gofmt` 가 다시 맞춘 것 — 공백을 정규화하면 바이트 동일).

## 다음 패스의 1순위

이 행으로 `control-plane/test/` 에 남는 미판정은 **`shared_volume_test.go` 하나**이고, 그것은
`tbm_session-platform-docs-impl` 의 `rct_20260908-0003`(PR #101)이 **살아 있는 소유 선언**을 갖는다
(10·11·12·13차가 같은 이유로 남겼다). 그 선언이 풀리면 그때가 그 파일의 차례다.

스캔 범위에 남는 나머지는 오탐 7줄뿐이다 — `web/src/design/README.md`(마크다운 볼드 접두 6줄,
실 주석 0)·`data-plane/Dockerfile`(셸 `case` 1줄, 실 주석 0). 9차 패스부터 같은 자리에 있고,
고치는 자리가 지문 패턴이라 **모델 정의 개정 사안**이다.

**이 패스가 새로 남기는 후속 하나** — 12차 패스가 `e2e_state_api_6_stream_contract_test.go` 에
남긴 유지 주석이 `claude-code-workload.md#시나리오 4` 를 **「저작 대기 행」**으로 부른다. 그 행은
09-11 에 매핑 표로 옮겨졌으므로 그 이름은 더 이상 가리킬 곳이 없다. 그 파일은 12차 행에 등재돼
있어 고치면 R2 가 발화하므로 **같은 PR 에서 그 행의 줄 수·지문을 함께 갱신해야 한다** — 이번
슬라이스의 범위 밖이고, 다음 증분 재판정의 입력으로 남긴다.
