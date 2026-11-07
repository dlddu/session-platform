# PRD: 세션 워크로드 — 클로드 코드 CLI

> 대상 요구사항: 세션 워크로드 타입에 **클로드 코드 CLI** 추가
> (기존 인터랙티브 쉘과 병존하는 두 번째 타입, `shell-workload.md`와 대응 관계)

## 달성 가치
- **V8 목적에 맞는 작업 환경 선택** — 세션 생성 시 워크로드 타입을 고를 수 있게 하고, 선택과 무관하게 V1~V5 보장이 동일하게 성립함을 확정 (AC-E1)
- **V1 세션 격리** — 에이전트 실행과 자격 증명을 세션 전용 워크로드 파드에 가둠 (AC-A1/A2 구체화). credential-proxy는 같은 파드 안의 사이드카 컨테이너이므로 AC-A2의 보조 파드가 아니다
- **V2 유휴 자원 회수** — 파일시스템 아카이브 기반으로 동결·회수 (AC-B1의 타입별 경로)
- **V3 끊김 없는 세션 연속성** — 메모리가 아닌 **대화 기록·작업 디렉터리** 보존으로 연속성 확보(AC-B2/B3 구체화), write/read를 프롬프트·실행 출력 시맨틱으로 확정 (AC-C2/C3 구체화)
- **V4 자유로운 멀티세션 전환** — 전환 후에도 동일한 offset 커서 규약으로 출력을 이어 읽음 (AC-C2 구체화)

> 📌 **가치 연결 규칙 (2026-08-08)**: `shell-workload.md`와 동일하게, 각 AC의 달성 가치는 자신이 구체화하는 상위 AC의 가치를 상속한다. 이전 판에서 참조하던 "V7 에이전트 세션"은 워크로드 타입을 가치로 올린 것이어서 `../values.md`에서 삭제되었다.
>
> ⚠️ **다만 AC-E1(타입 선택)은 예외다.** 이 AC는 상위 AC의 구체화가 아니라 새 축이라 상속할 가치가 없다. 2026-08-08 3차 개정에서 이를 뒷받침할 **V8(목적에 맞는 작업 환경 선택)** 이 신설되어 AC-E1 → V8로 직접 연결되었다(부가로 V1: 타입이 달라도 1세션=1전용 워크로드 파드 매핑 유지).

> 이 PRD는 세션 워크로드가 **단수(인터랙티브 쉘)에서 복수(타입)로 확장**되는 변경을 다룬다.
> AC-E1이 타입 선택이라는 새 축을 도입하고, AC-E2~E6은 `claude-code` 타입에서 기존 AC들
> (AC-A1·B1·B2·B3·C2·C3)이 어떻게 구체화되는지를 못박는다. 다른 타입의 대응 명세는
> `shell-workload.md`(AC-D1~D5)와 `approval-gated-workload.md`(AC-F1~F6)이며, 세 문서는 같은 상위 AC에
> 대한 **타입별 구체화**로 나란히 존재한다.
>
> 📌 **역참조 (2026-09-03)**: `approval-gated` 타입은 이 문서의 배선을 **복제하지 않고 재사용한다** —
> 원샷 실행·직렬 큐·offset 커서·파일시스템 아카이브(AC-E2/E3/E5), credential-proxy의 **동작 계약**과
> optional `model`/`models` 기본값·soft catalog, 출력 증분 redaction(AC-E6)이 그대로 적용된다.
> 그 위에 얹히는 차이(egress 제한·승인 게이트·보조 파드·공유 볼륨·게이트웨이 자격 증명)만
> AC-F2~F6이 정의한다. **따라서 AC-E2·E3·E5·E6을 고칠 때는 `approval-gated`도 함께 바뀐다.**
>
> ⚠️ **두 가지는 재사용하지 않는다 (2026-09-03 확정)**: ① credential-proxy의 **배치** — 이 문서는 워크로드
> 파드 안의 loopback 사이드카이고, `approval-gated`는 같은 계약의 프록시를 **세션 헬퍼 보조 파드의 컨테이너**로
> 옮겨 파드 IP에 바인딩한다(AC-F2·F4·F6). 동작 계약이 갈라지지 않도록 **정본은 계속 AC-E6**이며 배치만 타입별로 다르다.
> ② 주 컨테이너의 `k3s-mcp-token`과 **런타임 plugin 부트스트랩** — 부트스트랩이 K3s MCP·`github.com`으로
> 직접 나가야 하는데 `approval-gated`의 egress 허용 목록에 그 목적지가 없다. 2026-09-03에 그 타입에서는
> **부트스트랩을 아예 두지 않는 것**으로 확정했다(AC-F6). 따라서 이 문서의 plugin 배선을 고쳐도
> `approval-gated`는 영향을 받지 않으며, 반대로 **plugin이 제공하는 플랫폼 도구를 그 타입도 써야 한다면
> 세션 MCP 쪽에 따로 실어야 한다.**

## Acceptance Criteria

### AC-E1: 세션 워크로드 타입 선택
- **설명**: 세션 생성 요청은 워크로드 타입 `workloadType`을 받는다. 허용값은 `shell`·`claude-code`·`approval-gated`(AC-F1)이며, **필드 생략만** `shell` 기본값으로 해석한다(explicit empty/null/비문자열은 400). 타입은 세션 수명 동안 **불변**이다(타입을 바꾸려면 새 세션을 만든다). 하나의 세션은 정확히 하나의 워크로드 타입을 가지고, control plane은 타입에 따라 서로 다른 data plane 워크로드(에이전트 모드/이미지)로 pod를 프로비저닝한다. 타입이 늘어나도 AC-A1(control plane은 워크로드를 직접 실행하지 않음)과 AC-A2(1 세션 = 1 전용 워크로드 파드)는 그대로 유지된다.
- **달성 가치**: V8, V1
- **구체화 대상**: AC-A1의 "실제 세션 워크로드" — 이제 **타입별로 분기**한다 (AC-D1은 `shell` 타입의 구체화, AC-E1~E6은 `claude-code` 타입의 구체화)
- **검증 방법**: `workloadType=claude-code`로 세션을 생성하면 해당 pod에서 클로드 코드 CLI 실행이 가능하고, `workloadType` 미지정 생성은 기존과 동일하게 `shell` 세션으로 동작함을 확인한다. explicit `""`/`null`/비문자열과 허용값 외 타입은 pod 생성 전 400으로 거부됨을 확인한다. 기존 세션 route에 workload/model 변경 필드를 보내면 400이고 원래 값이 유지됨을 확인한다.

### AC-E2: write = 프롬프트 1회 실행
- **설명**: `claude-code` 세션에서 `POST /sessions/{id}/write`의 `payload`는 **프롬프트**로 해석되어, 세션 pod 안에서 다음 형태의 1회 실행을 트리거한다:

  ```
  ANTHROPIC_BASE_URL=http://127.0.0.1:8091  ANTHROPIC_AUTH_TOKEN=<비밀이 아닌 proxy placeholder> \
    claude [--continue] [--model <선택된 모델>] \
      --permission-mode auto -p \
      --output-format stream-json --verbose --include-partial-messages -- "<payload>"
  ```

  실제 공급자 HTTPS URL·토큰은 별도 credential-proxy sidecar에만 ConfigMapKeyRef·SecretKeyRef로 주입된다. CLI와 도구가 실행되는 주 컨테이너는 localhost proxy와 비밀이 아닌 placeholder만 보며, proxy가 HTTPS 목적지를 고정하고 요청 자격 증명을 실제 토큰으로 교체한다. 평문 HTTP upstream은 같은 pod 네트워크에서의 패킷 관찰 위험 때문에 시작 시 거부한다.

  `-p`는 print(비대화) 모드이고 `--` 뒤 payload는 옵션이 아닌 단일 positional argv로 전달된다. `stream-json`과 partial-message 플래그는 실행이 끝나기 전의 assistant `text_delta`를 stdout JSONL로 내보낸다. data plane은 JSONL 원문이나 최종 result를 사용자 출력으로 저장하지 않고, `text_delta`만 한 번씩 순서대로 투영하며 diagnostic stderr도 같은 append-only 출력에 합친다. 세션 metadata가 `platform-default`이면 pod spec이 플랫폼 설정 ConfigMap의 optional `model` 키를 주 컨테이너의 `CLAUDE_CODE_MODEL` ConfigMapKeyRef로 둔다. 주 컨테이너가 시작하거나 재시작할 때 해석한 값이 비어 있지 않으면 `--model <ConfigMap 값>`을 넣고, 키가 없거나 값이 비어 있으면 `--model`을 생략해 설치된 Claude CLI 기본 선택에 위임한다. 구체 model을 명시한 세션은 ConfigMapKeyRef 대신 그 값을 literal `CLAUDE_CODE_MODEL`로 받아 ConfigMap 기본값보다 우선하며 `--model`의 단일 argv로 전달한다. **첫 성공 실행 이후**부터 `--continue`도 추가한다(실패·timeout인 첫 실행은 대화를 만든 것으로 보지 않는다). 실행은 **원샷**이다 — 프로세스는 응답을 출력한 뒤 종료하며, 세션이 살아 있는 동안 상주하는 CLI 프로세스는 없다. write는 쉘 타입(AC-D2)과 동일한 비블로킹 규약을 따른다: 실행 완료를 기다리지 않고 즉시 반환하고, 실행 중 출력은 AC-E3의 stream/read로 회수한다. 한 prompt payload는 최대 1 MiB이며 초과 write는 큐에 넣지 않고 `413 Payload Too Large`로 거부한다. 동일 세션에서 이전 실행이 아직 끝나지 않은 상태에서 다음 write가 오면 **직렬로 큐잉**된다 — 한 세션 안에서 두 개의 `claude` 실행이 동시에 돌지 않으며, 이것이 AC-E4의 대화 순서를 보장한다. bounded queue가 포화된 새 write는 `429 Too Many Requests`로 거부된다.

  각 invocation에서 증분 redaction 후 저장되는 **assistant text + diagnostic stderr는 marker를 포함해 16 MiB**로 제한한다. raw stream-json framing은 이 quota나 cursor에 포함하지 않는다. 넘친 tail은 버리고 이미 노출한 bytes를 고치지 않은 채 보존된 prefix 끝에 `[session-platform: invocation output truncated at 16 MiB]`를 붙인다. 해당 invocation과 이미 받아 둔 후속 queue는 정상적으로 직렬 실행한다.
- **달성 가치**: V3
- **구체화 대상**: AC-C3(Write API 상태별 분기)의 "write" 시맨틱 — `claude-code` 타입 버전
- **검증 방법**: active 세션에 `payload="1+1은?"`로 write 후 pod에서 위 exact argv의 `claude` 프로세스가 1회 기동되고 응답 출력 후 종료함을 확인한다. write가 실행 완료를 기다리지 않고 반환하며, fake runner가 첫 delta 뒤 대기하는 동안에도 그 delta가 read와 SSE에 보이는지 확인한다. 연속 2회 write 시 두 실행이 겹치지 않고 순서대로 수행됨을 확인한다. 첫 실행 실패·timeout 뒤에는 `--continue`가 붙지 않고 첫 성공 뒤에만 붙음을 확인한다. prompt가 1 MiB를 넘으면 public API까지 413이고 실행되지 않으며, queue 포화 시 429, invocation 출력 초과 시 16 MiB 경계 안의 append-only truncation marker를 확인한다.

> ✅ **구현 결정 (headless 권한 정책, 2026-08-08)**: 전용 session pod를 실행 경계로 삼고, 모든 invocation을 `--permission-mode auto`로 시작한다. 세션 HOME의 플랫폼 관리 `.claude/settings.json`에는 코딩에 필요한 `Read`·`Write`·`Edit`·`Glob`·`Grep`·`Bash`만 허용한다. `--dangerously-skip-permissions`는 사용하지 않으며 설정 파일은 파일시스템 아카이브에 포함해 복원 후에도 동일 정책을 유지한다.

### AC-E3: read/stream = 실행 중 출력, offset 커서 기반 델타
- **설명**: `claude-code` 세션의 출력은 쉘 타입(AC-D3)과 **동일한 append-only offset 커서 규약**을 따른다. `POST /sessions/{id}/read`는 요청 `offset`(직전 응답이 발급한 `nextOffset`, 생략 시 0) 이후에 누적된 출력을 반환하고 새 `nextOffset`을 발급한다. 이 JSON read는 비파괴적 catch-up·reconcile·`offset=0` 전체 replay 용으로 계속 지원한다.

  정상 UI 경로는 `GET /sessions/{id}/stream?offset=N`의 workspace 수명 SSE다. 실행이 끝날 때까지 기다리지 않고 안전하게 확정된 새 bytes마다 `event: output`을 보낸다. event id는 서버가 발급한 `nextOffset`이고 data는 `{offset,payloadBase64,nextOffset}`이다. base64는 정확한 byte 범위·길이·overlap 검증을 보존하기 위한 wire encoding이며 UTF-8 rune 중간 cursor를 허용하기 위한 것이 아니다. 증분 redaction을 마친 Claude 저장 출력은 valid UTF-8이고 모든 서버 발급 cursor와 event 경계는 code-point 경계다. 재연결 요청에 `Last-Event-ID`가 있으면 query `offset`보다 우선한다. 연결은 한 invocation 완료로 닫히지 않으며 run/queue 상태 event도 만들지 않는다. comment keepalive는 연결만 유지하고 output cursor나 `lastAccess`를 바꾸지 않는다. stream은 passive 관찰 경로라 active/idle의 기존 pod만 읽고 상태를 승격하거나 snapshot을 복원하지 않으며 `lastAccess`도 touch하지 않는다.

  누적 대상은 stdout stream-json에서 순서대로 투영한 assistant `text_delta`와 diagnostic stderr다. raw JSONL과 delta를 중복하는 최종 result는 누적하지 않는다. 각 invocation은 **turn 마커**로 감싸여, 클라이언트가 바이트열만으로 write마다 입력과 출력을 갈라 볼 수 있다 — 쉘에서 PTY echo가 입력을 출력에 남기는 것에 해당하는 입력 기록이다. 마커는 RS(`0x1E`)로 시작해 개행으로 끝나는 한 줄이고, 한 turn은 다음 순서로 기록된다.

  ```
  0x1E "session-platform input seq=<n> at=<RFC3339 UTC 초>" LF   <프롬프트> LF
  0x1E "session-platform output seq=<n>" LF                      <assistant text·stderr·platform notice>
  0x1E "session-platform end seq=<n> status=<ok|error|timeout|canceled> at=<RFC3339 UTC 초>" LF
  ```

  - **RS는 마커만 쓴다** — 프롬프트·투영된 출력·platform notice에서 RS를 제거하고 저장하므로, 바이트열의 RS는 언제나 마커의 시작이다. 마커 줄은 한 번의 append로 기록되어 read 응답이 마커 한가운데서 끊기지 않는다(SSE chunk 경계는 끊을 수 있어 클라이언트는 개행 전의 RS 조각을 보류한다).
  - **input 블록**: 큐에서 꺼내 실행을 시작하는 시점에 input 마커·프롬프트·output 마커를 한 번에 기록한다. 그래서 아직 대기 중인 프롬프트는 보이지 않는다. 프롬프트는 끝 개행을 걷고 개행 하나로 닫으며, 출력과 같은 credential redaction·UTF-8 정규화를 거친다.
  - **end 마커**: 출력 sink를 모두 flush한 뒤 기록한다. `status`는 성공(`ok`, wait-delay 종료 포함)·실행 시간 초과(`timeout`)·agent 종료로 인한 취소(`canceled`)·그 밖의 실패(`error`)다. 출력이 개행으로 끝나지 않았으면 마커 앞에 개행을 붙인다(input 마커 앞도 같다).
  - **seq**는 세션 안에서 1부터 늘어나는 turn 번호다. 복원 시 보존된 출력의 input 마커 수에서 이어 센다.
  - 마커는 세션 256 MiB 한도에는 포함되지만 실행당 16 MiB 출력 quota에는 포함되지 않는다. 한도가 차면 마커도 더 기록되지 않으므로 마지막 turn에 end가 없을 수 있다.

  credential literal과 불완전 UTF-8 suffix는 다음 runner write까지 보류할 수 있으며, 한 번 cursor로 노출한 bytes는 이후 절대 다시 쓰지 않는다. 요청 cursor가 현재 보존 길이보다 크면 stream은 출력 대신 `event: reset`, `id=currentLength`, `{"nextOffset":currentLength}`를 보낸다. 브라우저는 미완성 decoder state를 버리고 `POST /read`의 `offset=0` 전체 이력으로 콘솔을 교체한 뒤 그 read의 `nextOffset`에서 stream을 다시 연다. SSE reset 신호 자체는 passive지만 이 `POST /read` 복구는 일반 Read API이므로 idle을 active로 승격하고 `lastAccess`를 갱신할 수 있다. 전송 오류에서는 native 자동 재접속을 먼저 닫고 세션 상태를 확인한다. active/idle이면 마지막 cursor로 backoff 재연결하고, snapshot이면 자동 stream/read로 복원하지 않고 Restore 화면으로 이동한다. 즉 워크로드 타입이 달라도 **보존·커서 계약은 동일**하고, 그 안에 담기는 내용만 쉘 출력과 에이전트 출력으로 갈린다.

  Claude 누적 scrollback은 terminal marker를 포함해 **256 MiB**로 제한한다. 다음 출력이 이 경계를 넘기려는 순간 기존 bytes를 다시 쓰지 않고 남은 prefix와 `[session-platform: session output limit reached; further prompts are disabled]`를 append해 닫는다. 따라서 이미 발급한 byte offset은 그대로 유효하다. 이 전에 수락된 queued prompt는 끝까지 실행하되 이후 출력은 버리고, terminal marker 뒤의 새 write는 `507 Insufficient Storage`로 거부한다.
- **달성 가치**: V3, V4
- **구체화 대상**: AC-C2의 JSON read와 같은 보존 cursor를 사용하는 `claude-code` live output 구체화
- **검증 방법**: write 후 process가 종료되기 전에 둘 이상의 SSE output delta가 순서대로 도착하고, event id/data cursor와 decoded byte 수가 일치하며 모든 cursor가 UTF-8 code-point 경계임을 확인한다. 중간에 연결을 끊은 뒤 `Last-Event-ID`로 재접속해 누락·중복 없이 이어 받고, 동일 cursor의 POST read가 같은 저장 bytes를 valid UTF-8로 반환하며 `offset=0`은 전체 이력을 반환함을 확인한다. current length보다 큰 cursor에는 reset이 발급되고 SPA가 decoder state 폐기→read(0) 화면 교체→새 cursor stream 순으로 복구해야 한다. runner write가 UTF-8 rune이나 credential 중간에서 갈려도 잘못된 문자나 secret이 한 번도 노출되지 않아야 한다. 각 실행이 input·output·end 마커로 실행 순서대로 감싸이고 seq가 1부터 늘며 복원 후 이어지고, 프롬프트에 든 credential literal도 치환되며, 프롬프트·출력에 섞인 RS가 제거되고, 실패한 실행의 end가 `status=error`, 개행 없이 끝난 출력 뒤에 개행이 붙음을 확인한다. 축소한 결정적 limit로 기존 커서를 발급한 뒤 256 MiB와 같은 경계 로직을 넘겨 live terminal marker·커서 불변·accepted queue drain·후속 507을 검증한다.

> ✅ **구현 결정 (turn 마커, 2026-10-25 — 같은 날 앞선 `▸` echo를 대체)**: 입력 기록은 CLI가 HOME에 남기는 대화 기록 파일을 읽어 오지 않고 agent가 같은 scrollback에 직접 append한다. 대화 기록 파일은 CLI마다 형식이 다르고 문서화되지 않았으며, offset 공간이 출력과 달라 순서를 맞추려면 별도 병합이 필요하다. 같은 scrollback에 두면 커서·SSE·`offset=0` replay·256 MiB 한도·아카이브가 그대로 적용되고 `claude-code`·`approval-gated`·`minimax-code`가 같은 worker 경로로 함께 얻는다. 처음 넣은 `▸ <프롬프트>` 줄은 사람이 읽기엔 충분했지만 입력과 출력의 경계를 기계가 가를 수 없어(프롬프트·응답이 같은 모양의 줄을 담을 수 있다), PocketAide가 write마다 입력/출력을 접어 보이려면 경계가 필요했다. 그래서 출력에 나오지 않는 RS 센티널을 쓰는 마커로 바꿨다. 사람이 읽는 표현은 클라이언트가 만든다 — SPA는 마커를 `▸ <프롬프트>`로 바꿔 그리고 실패한 turn에 `◆ turn ended: <status>`를 덧붙인다.

### AC-E4: 대화 연속성 = pod에 지속되는 대화 기록
- **설명**: 같은 세션에 대한 연속된 write는 **하나의 이어지는 대화**로 처리된다. 각 실행은 원샷이지만, 클로드 코드 CLI가 pod 파일시스템에 남기는 대화 기록을 다음 실행이 이어받으므로, N번째 프롬프트는 앞서 **성공한** 프롬프트·응답을 문맥으로 갖는다. 성공한 첫 실행까지는 새 대화 모드이고 그 이후 write부터 재개(resume) 모드다(첫 실행 실패·timeout은 resume 상태를 만들지 않는다). 세션의 작업 디렉터리도 실행 간 유지되어, 이전 실행이 만든 파일을 다음 실행이 그대로 본다.
- **달성 가치**: V3
- **검증 방법**: `write("내 이름은 다르마야")` → `write("내 이름이 뭐야?")` 후 read하면 두 번째 응답이 "다르마"를 포함함을 확인한다(문맥 이어짐). 별개 세션에서 같은 두 번째 프롬프트를 실행하면 이름을 알지 못함을 확인한다(세션 간 대화 격리, AC-A2). `write("touch marker.txt")` 후 `write("작업 디렉터리에 어떤 파일이 있어?")` 응답에 `marker.txt`가 포함됨을 확인한다.

> ✅ **구현 결정 (재개 방식, 2026-08-08)**: 첫 실행은 새 대화를 시작하고, 성공한 첫 실행 이후부터 `--continue`로 해당 세션 pod의 최근 대화를 잇는다. 세션별 고정 HOME과 작업 디렉터리를 사용하며, 별도 대화 ID를 control-plane 메타데이터에 저장하지 않는다. resume 플래그와 CLI 기록은 파일시스템 아카이브에 함께 보존된다.

### AC-E5: 파일시스템 아카이브 기반 동결·복원
> 📌 **개정 (2026-11-01)**: 초판 제목은 「CRIU 비대상 — …」였고, 쉘 타입만 CRIU로 프로세스 트리를 보존한다는 대비 위에 서 있었다. CRIU가 모든 워크로드에서 퇴역하면서 이 AC가 정의한 아카이브가 **모든 타입의 공통 동결 경로**가 됐다(`shell`의 보존 대상은 AC-D4, `approval-gated`의 공유 볼륨은 AC-F5). 아래 계약 자체는 바뀌지 않는다.
- **설명**: `claude-code` 세션은 원샷 실행 모델이라 동결 시점에 보존해야 할 상주 프로세스 트리가 존재하지 않는다. 유휴 한계 도달 시(AC-B1) 시스템은 세션의 **작업 디렉터리·대화 기록·누적 출력 버퍼를 아카이브하여 외부 스토리지에 저장**하고 pod를 회수한다. 재접근 시(AC-B2)에는 새 pod에 아카이브를 복원한 뒤 `active`로 전이하며, 복원 후의 write는 동결 이전 대화 문맥과 작업 디렉터리 위에서 이어진다. 누적 출력 버퍼(256 MiB terminal marker와 output-full 상태 포함)가 아카이브에 포함되므로 **복원 전 발급된 `nextOffset` 커서는 복원 후에도 유효**하고, 이미 output-full이었다면 복원 후 새 write도 계속 507이다(AC-E3의 커서 연속성 — 모든 타입이 scrollback을 같은 filesystem archive에 직렬화한다).
- **달성 가치**: V2, V3
- **구체화 대상**: AC-B1(동결)·AC-B2(복원)·AC-B3(무결성) — 메커니즘은 모든 타입이 이 아카이브이고, 타입별로 다른 것은 보존 대상이다(`shell`=홈 디렉터리 파일+scrollback, AC-D4 · `claude-code`=이 AC · `approval-gated`=이 AC+공유 볼륨, AC-F5)
- **검증 방법**: `claude-code` 세션이 유휴 한계에 도달하면 아카이브가 생성되고 pod가 회수됨을 확인한다(AC-A3의 자원 회복도 동일하게 성립). 재접근 시 새 pod로 복원되어 `active`가 되고, 동결 전 대화를 참조하는 프롬프트가 정상 응답하며 동결 전에 만든 파일이 작업 디렉터리에 남아 있음을 확인한다. 동결 직전 확보한 `nextOffset`으로 복원 후 read하면 델타만 반환되고, `offset=0`은 동결 전·후 전체 이력을 반환함을 확인한다.
> 🔐 작업 디렉터리·대화·출력을 외부 저장소로 전송하므로 이 경로는 `CLAUDE_CODE_ARCHIVE_ENABLED=true`로 명시적으로 허용해야 한다(기본값 `false`). 비활성 상태에서는 pod를 회수하지 않고 snapshot 요청을 거부한다.

> ✅ **구현 결정 (아카이브 crash recovery, 2026-08-08)**: control plane은 128-bit generation·현재 Lease owner·source pod를 가진 private durable `preparing` transaction을 먼저 aggregate CAS로 저장한 뒤, 그 generation을 agent에 넘긴다. agent는 admission을 닫고 accepted queue를 drain해 같은 generation의 archive를 내보낸다. archive ref가 durable해지면 control plane은 pod 삭제 전에 transaction을 `committing`으로 CAS한다. `preparing` 실패/재시작은 새 holder가 owner를 먼저 CAS로 claim한 뒤 **그 generation만** abort하고 조건부로 transaction을 지운다. `committing` 이후의 Stop/finalize 실패는 abort하거나 admission을 다시 열지 않고, recovery가 Stop을 idempotently 재시도한 뒤 한 번의 aggregate CAS로 `snapshot`을 확정한다. 활성 checkpoint와 다른 generation의 늦은 abort는 409이므로 새 barrier를 열 수 없고, 이미 abort된 generation 재요청은 idempotent하다.
>
> Snapshot·Restore·recovery는 기본 15초 Lease를 5초마다 갱신하며 각 Renew 호출에도 5초 deadline을 둔다. holder 상실·hung Renew는 진행 중 side effect의 context를 취소한다. `Touch`는 최신 aggregate의 `lastAccess`만 단조 증가시키고, transaction CAS는 더 최신 `lastAccess`를 merge하므로 owner/phase fence를 stale metadata가 덮지 않는다.


### AC-E6: 공급자 자격 증명은 sidecar Secret, 플러그인 토큰은 main Secret, 플랫폼 모델은 optional ConfigMap
- **설명**: 플랫폼 설정은 둘로 나뉜다 — 자격 증명만 담는 플랫폼 전역 Kubernetes **Secret**(`auth-token`·`k3s-mcp-token`, 이름은 `CLAUDE_CODE_CREDENTIALS_SECRET`, 기본 `claude-code-credentials`)과 자격 증명이 아닌 키를 담는 **ConfigMap**(`base-url`·`model`·`models`·`ca-cert`·`k3s-mcp-url`·`plugin-marketplace-url`, 이름은 `CLAUDE_CODE_CONFIG_CONFIGMAP`, 기본 `claude-code-config`). 키 이름과 필수/선택 여부는 어느 쪽에 있든 같다. ConfigMap의 **필수** `base-url`과 Secret의 **필수** `auth-token` 키는 각각 `ANTHROPIC_BASE_URL`·`ANTHROPIC_AUTH_TOKEN`으로 세션 pod의 localhost credential-proxy sidecar에만 주입된다. Claude CLI·Bash가 실행되는 주 컨테이너에는 실제 공급자 자격 증명을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다. 세션 pod에는 전용 `data-plane` ServiceAccount token을 마운트하며, Kubernetes 기본 `view` ClusterRole을 클러스터 범위로 바인딩해 Secret을 제외한 읽기 권한만 제공한다. 두 컨테이너는 네트워크만 공유하고 PID·파일시스템은 공유하지 않는다. proxy는 시작 시 **HTTPS** upstream origin을 고정하고, 허용 목록에 든 Anthropic/Claude/Stainless 요청 헤더만 전달한 뒤 플랫폼 토큰 하나를 주입한다. CONNECT·Upgrade와 모든 요청 trailer를 거부/제거하고, 중간 1xx를 포함한 응답에서 토큰 리터럴이 클라이언트로 노출되지 않게 한다. Provider SSE response는 raw upstream 64 MiB 상한 내에서 whole-body buffering 없이 증분 전달하여 안전한 upstream flush가 EOF 전에 Claude CLI에 도달한다. proxy의 tail-safe redactor는 token 후보 suffix를 response read 경계 너머까지 보류하므로 split credential도 일시적으로 노출하지 않는다. 자격 증명은 세션 생성 요청으로 지정할 수 없고, 세션 조회 응답·로그·read·SSE 출력 어디에도 토큰 값이 노출되지 않는다. runner 출력의 별도 증분 redactor도 token이 여러 runner write에 걸쳐도 가능한 match suffix를 보류한다. 별도의 optional `ca-cert` 키는 자격 증명이 아니라 **신뢰 앵커**다 — CA 인증서는 공개 물건이므로 주소류 설정에 가깝다. 공급자 `base-url`이 공개 저장소가 모르는 발급자의 사설 게이트웨이일 때만 필요하며, 자기가 짝지어진 자격 증명과 같은 **credential-proxy 컨테이너에만** `ANTHROPIC_CA_CERT`로 optional ConfigMapKeyRef 주입되고 주 컨테이너에는 노출되지 않는다. proxy는 이 PEM 번들을 시스템 루트 풀에 **추가**할 뿐 대체하지 않으므로, 공개 발급 엔드포인트를 쓰는 배포도 이 키가 있는 채로 계속 동작한다. 키가 없거나 값이 비면 지금까지처럼 시스템 풀만 쓰고 `k8s/` 매니페스트는 한 줄도 달라지지 않는다. 값이 비어 있지 않은데 인증서가 하나도 파싱되지 않으면 첫 요청까지 미루지 않고 proxy 시작을 실패시킨다 — 조용히 시스템 신뢰로 되돌아가면 그 키를 넣은 이유였던 게이트웨이로의 모든 호출이 거부되기 때문이다. 이 키는 신뢰하는 발급자 집합만 넓히며, upstream origin을 시작 시 **HTTPS**로 고정하는 요구를 낮추지 않는다. 반면 ConfigMap의 `model` 키는 자격 증명이 아닌 **optional 플랫폼 기본값**이다. 세션 metadata가 `platform-default`일 때만 주 컨테이너의 `CLAUDE_CODE_MODEL`에 optional ConfigMapKeyRef로 주입되고 sidecar에는 노출되지 않는다. 키가 없거나 값이 비면 Claude CLI의 기존 기본 선택을 쓰도록 `--model`을 생략한다. 생성 요청의 `model` 필드에 구체 값을 명시하면 ConfigMapKeyRef 대신 그 값을 literal `CLAUDE_CODE_MODEL`로 주입하여 플랫폼 기본값보다 우선한다. model 필드의 **생략만** 안정적인 별칭 `platform-default`로 해석하며 explicit empty/null/비문자열, 앞뒤 공백이 있는 값, 허용 pattern 밖의 값은 pod 생성 전에 400으로 거부한다. 세션에 저장된 model 또는 별칭은 수명 동안 불변이다(다른 설정을 고르려면 새 세션을 만든다 — AC-E1의 타입 불변성과 같은 규칙).

  Data plane의 `entrypoint.sh`는 `claude-code` workload에서만 agent 시작 전에 `K3S_MCP_TOKEN`으로 K3s MCP의 `github_app_installation_token`을 호출한다. 발급 범위는 private `plugin-marketplace` 저장소의 `contents:read`로 제한하며, 단명 GitHub token은 해당 저장소 URL에 한정한 프로세스 범위 Git HTTPS Authorization header로만 전달한 채 `claude plugin marketplace add https://github.com/dlddu/plugin-marketplace.git`와 `claude plugin install session-platform@dlddu-plugins`를 실행한다. 이후 token과 header를 unset하고 runtime plugin seed 경로만 agent에 상속한다. shell과 credential-proxy는 이 bootstrap을 건너뛴다. 세션 HOME의 managed settings는 이 plugin만 활성화한다. 플랫폼 Secret의 **필수** `k3s-mcp-token` 키는 `K3S_MCP_TOKEN`으로 주 컨테이너에만 주입하며 credential-proxy와 control plane에는 주입하지 않는다. plugin MCP는 이 환경 변수를 사용하고, agent는 그 literal을 runner 출력의 증분 redaction 대상에 포함한다. 부트스트랩이 닿는 **두 상류 주소**는 자격 증명이 아니라 주소류 설정이므로 플랫폼 ConfigMap의 optional `k3s-mcp-url`·`plugin-marketplace-url` 키로 재정의할 수 있다 — 각각 `K3S_MCP_URL`·`CLAUDE_CODE_PLUGIN_MARKETPLACE_URL`로 **주 컨테이너에만** optional ConfigMapKeyRef 주입되며 credential-proxy와 control plane에는 주입하지 않는다. 키가 없거나 값이 비면 entrypoint가 프로덕션 기본값(`https://homelab-k3s-mcp.llkm.nl/mcp`와 `https://github.com/dlddu/plugin-marketplace.git`)을 그대로 쓰므로 `k8s/` 매니페스트는 한 줄도 달라지지 않는다 — `ca-cert`와 같은 형태의 optional 주소류 키다. 재정의는 **코드 경로를 바꾸지 않는다**: 같은 `claude plugin marketplace add`가 실행되므로 가리키는 원격은 같은 저장소 모양을 서빙해야 `session-platform@dlddu-plugins` 설치가 성립하고, 재정의된 K3s MCP 주소도 같은 `github_app_installation_token` 호출을 받는다. 단명 GitHub token을 싣는 Git `extraheader`는 **재정의된 marketplace URL 자체에 범위가 걸리므로** 주소를 옮기면 범위도 함께 옮겨가고 다른 호스트로 넓어지지 않는다. 공급자 upstream과 달리 이 두 주소에는 **스킴을 강제하지 않는다** — 시작 시 HTTPS로 고정하는 것은 공급자 자격 증명이 흐르는 proxy 경로이고, 이 두 주소를 재정의하는 쪽은 클러스터 내부 원격처럼 신뢰 구간을 가리키는 배치다(kind e2e SUT가 그렇게 쓴다). 평문 원격을 가리키면 단명 GitHub token이 그 구간을 평문으로 지나므로, 재정의의 전송 신뢰는 그 주소를 고른 배치가 진다.

  주 컨테이너는 ConfigMap 또는 literal에서 얻은 non-empty effective model도 `^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`로 다시 검증한다. 선행 `~` 하나는 OpenRouter moving alias를 지원하며 전체 길이 상한은 128자다. invalid ConfigMap 값은 CLI argv로 넘기지 않고 data-plane agent 시작을 실패시킨다.

  별도의 optional `models` 키는 모델 선택 UI를 위한 **순서가 있는 soft catalog**이며 JSON string array로 저장한다. Deployment는 공개 설정인 `model`과 `models`를 각각 control plane의 `CLAUDE_CODE_DEFAULT_MODEL`과 `CLAUDE_CODE_MODELS` 환경 변수로 키 단위 투영하고, 공급자 연결 설정인 `base-url`·`auth-token`은 주입하지 않으며 Secret 읽기 권한도 주지 않는다. `CLAUDE_CODE_CONFIG_CONFIGMAP` 이름을 기본값에서 바꾸면 Deployment의 `CLAUDE_CODE_DEFAULT_MODEL`과 `CLAUDE_CODE_MODELS`의 `valueFrom.configMapKeyRef.name`을 같은 literal 이름으로 함께 patch해야 한다(Kubernetes는 env 값을 ConfigMap ref 이름으로 동적 치환하지 않는다). 값이 없거나 빈 문자열 또는 `[]`이면 catalog는 빈 배열이고 기존 free-text 모델 입력을 유지한다. 비어 있지 않은 값은 control-plane 시작 시 JSON string array인지 엄격히 검사하고, 각 항목에 세션 model과 같은 pattern(`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`)을 적용한다. 빈 항목·앞뒤 공백·중복·reserved alias `platform-default`·JSON `null`·배열 외 값은 설정 오류로 시작을 실패시킨다. `GET /api/v1/config`는 concrete ConfigMap model이 있으면 이를 `defaultModel`로, missing/empty면 `platform-default`를 반환하고 `models` catalog와 함께 `Cache-Control: no-store`로 공개한다. UI는 concrete default를 `<model> (platform default)` 한 항목으로 합쳐 표시하고 그 선택의 create request에서는 model을 생략한다. provider credential은 공개하지 않는다. catalog는 선택 편의만 제공하므로 목록 밖의 유효한 model도 Create Session API가 계속 허용한다.

  singular `model` ConfigMap 변경은 `platform-default` 주 컨테이너가 다음에 시작될 때(새 세션 pod, snapshot 복원으로 재생성되는 pod, 기존 pod의 container restart) 적용되고 실행 중인 컨테이너나 concrete-model 세션 환경을 즉시 바꾸지 않는다. UI에 표시하는 singular `model`과 plural `models`는 모두 control-plane 시작 시 읽는 환경 snapshot이므로 Deployment rollout 뒤에 config API와 UI에 반영된다. 따라서 세션 metadata의 `platform-default` 별칭은 불변이지만 복원·재시작 시점의 플랫폼 기본 설정이 달라졌다면 유효 provider model은 달라질 수 있다.
- **달성 가치**: V1
- **검증 방법**: ConfigMap의 필수 HTTPS `base-url`과 Secret의 필수 `auth-token`이 sidecar에만 주입되고 주 컨테이너에는 localhost URL·placeholder와 필수 `k3s-mcp-token` SecretKeyRef만 있음을 확인한다. entrypoint가 Claude workload에서만 K3s MCP에 `plugin-marketplace`/`contents:read` 토큰을 요청하고 HTTPS marketplace와 plugin을 설치한 뒤 GitHub token 없이 agent를 exec하며, shell/proxy는 bootstrap을 건너뛰는지 확인한다. optional `k3s-mcp-url`·`plugin-marketplace-url`이 주 컨테이너에만 optional ConfigMapKeyRef로 있고 credential-proxy·control-plane에는 없음을 확인한다. 두 키에 값이 있으면 부트스트랩이 그 주소로 K3s MCP를 호출하고 그 원격에 `marketplace add`를 실행하며 Git `extraheader`의 범위도 그 URL을 따라 이동하는지, 키가 없거나 값이 비면 프로덕션 기본 주소를 쓰는지 확인한다. session managed settings가 해당 plugin만 활성화하는지 확인한다. `K3S_MCP_TOKEN`은 credential-proxy/control-plane에 없고 runner output redaction 대상인지 검증한다. pod가 `data-plane` ServiceAccount와 token을 사용하고 일반 클러스터 리소스에는 `get/list/watch`가 가능하지만 Secret `get/list/watch`는 거부됨을 확인한다. sidecar가 loopback에만 바인딩되고 평문 HTTP·고정 upstream 외 목적지·허용 목록 밖 요청 헤더·CONNECT/Upgrade를 거부하거나 제거하며, 중간 1xx 응답도 자격 증명을 누출하지 않음을 확인한다. optional `ca-cert`가 credential-proxy 컨테이너에만 optional ConfigMapKeyRef로 있고 주 컨테이너에는 없음을 확인한다. 값이 있으면 그 발급자가 서명한 사설 게이트웨이 upstream에 도달하고, 같은 설정에서 공개 발급 upstream의 검증도 계속 성공하며(신뢰 확대가 기존 신뢰를 약화하지 않는다), 키가 없거나 값이 비면 시스템 풀만 쓰고, 인증서가 하나도 없는 비어 있지 않은 번들은 proxy 시작 실패임을 확인한다. 지연 가능한 테스트 upstream이 첫 safe SSE chunk를 flush한 뒤 EOF를 막아도 proxy client가 release 전에 그 chunk를 받고, token을 response read 여러 개에 나누어도 어느 pre-release chunk에도 원문이 없으며 raw upstream 64 MiB SSE 상한을 full-body buffer 없이 지키는지 확인한다. 생성 요청의 자격 증명 필드는 unknown field로 400 거부되고 세션 조회 응답과 read/SSE 출력에 토큰 문자열이 포함되지 않음을 확인한다. runner에서도 token을 여러 stdout/stderr write에 나누어도 중간 SSE event에 원문이 없어야 한다. `platform-default` 세션의 주 컨테이너만 optional `model` ConfigMapKeyRef를 가지며, ConfigMap 값이 비어 있지 않으면 그 값으로 실행하고 키가 없거나 값이 비면 `--model`을 생략하는지 확인한다. 구체 `model`로 생성한 세션은 literal 환경 변수를 사용해 ConfigMap 기본값보다 우선하고, explicit empty/null/비문자열·앞뒤 공백·invalid pattern은 400임을 확인한다.

  공개 `model`과 `models`만 control-plane 환경에 키 단위 투영되고 credential은 없는지 확인하고, custom 설정 ConfigMap 이름을 쓸 때 두 ConfigMap ref 이름이 함께 patch되는지 확인한다. unset/empty/`[]`는 config API의 non-null 빈 배열과 free-text UI로 이어져야 한다. 올바른 배열은 순서대로 노출되고, malformed JSON·null·배열 외 값·빈/공백/invalid/중복/reserved 항목은 control-plane startup 오류인지 확인한다. config 응답은 concrete default 또는 fallback `platform-default`와 catalog만 포함하고 `Cache-Control: no-store`이며, catalog 밖 valid model도 API로 생성되는지 검증한다. singular `model` 변경은 새/복원 pod와 container restart의 실행값에 반영되고, singular/plural UI snapshot은 control-plane rollout 뒤에 함께 반영되는지 확인한다.

> ✅ **구현 결정 (모델 정책, 2026-08-08; 2026-08-09 개정)**: model은 workload type과 함께 생성 시 확정되어 불변이다. `platform-default`는 특정 공급자 모델 버전을 API에 하드코딩하지 않는 세션 메타데이터 별칭이다. 이 별칭의 유효 모델은 플랫폼 설정의 optional `model` 키(2026-11-02부터 ConfigMap)로 운영자가 선택하며, 키가 없거나 비면 설치된 Claude Code CLI의 기본 선택에 위임한다. 구체 세션 model은 literal 값으로 이 플랫폼 기본값보다 우선한다.

> ✅ **구현 결정 (모델 카탈로그, 2026-08-09)**: optional `model`과 `models`는 control-plane rollout 단위의 UI default와 soft catalog다. `GET /api/v1/config`가 concrete default 또는 fallback alias와 catalog를 공개하되 Create Session API의 allowlist로 사용하지 않으며, 빈 catalog는 기존 free-text 입력을 보존한다.

> ✅ **구현 결정 (워크로드 타입 목록, 2026-10-23)**: `GET /api/v1/config`가 `workloadTypes` 배열로 이 배포가 생성할 수 있는 타입(셸은 항상, 나머지는 이미지가 설정된 것만)을 표시 순서대로 공개한다. 각 항목은 `type`·`label`·`description`·`takesModel`·`input`(`prompt`|`terminal`)·`defaultModel`·`models`이고, `defaultModel`·`models`의 의미는 위 모델 카탈로그 결정과 같다(`takesModel=false`면 `defaultModel`은 `null`, `models`는 빈 배열). approval-gated는 claude-code ConfigMap의 `model`/`models`를 그대로 보이고, minimax-code는 control plane이 그 ConfigMap을 읽지 않으므로 `platform-default`와 빈 catalog다. 외부 클라이언트는 이 목록으로 타입 선택과 생성 요청 검증을 하므로 타입 추가에 클라이언트 배포가 필요 없다. 기존 `claudeCode` 필드는 하위 호환을 위해 유지한다. 목록은 표시용이며 Create Session API의 검증 범위를 바꾸지 않는다.

> ✅ **구현 결정 (설정·자격 증명 분리, 2026-11-02)**: 운영의 플랫폼 Secret은 external-secrets 가 SSM 에서 렌더한다(flux-cd-apps). 그 Secret에서 자격 증명이 아닌 키를 ConfigMap `claude-code-config`(`CLAUDE_CODE_CONFIG_CONFIGMAP`)로 옮겨, Secret `claude-code-credentials`에는 `auth-token`·`k3s-mcp-token`만 남겼다. 키 이름, 필수/선택 여부, 어느 컨테이너에 들어가는지는 하나도 바뀌지 않았다 — 바뀐 것은 pod spec이 그 키를 `secretKeyRef` 대신 `configMapKeyRef`로 읽는다는 것뿐이다. control plane은 두 객체를 Kubernetes API로 읽지 않고 pod spec과 자기 Deployment env에서 이름으로만 참조하므로 RBAC은 변하지 않는다. 세션 pod의 `data-plane` 서비스 어카운트는 `view`로 ConfigMap을 읽을 수 있지만 그 안에는 자격 증명이 없다.

> ✅ **구현 결정 (이미지/모드, 2026-08-08)**: 하나의 data-plane 이미지에 PTY shell과 Claude Code CLI를 포함하고(*2026-11-01*: CRIU 바이너리는 제거됐다) `DATA_PLANE_WORKLOAD`로 시작 모드를 분기한다. Kubernetes pod label/env와 control-plane의 immutable workload spec이 선택한 모드를 끝까지 전달한다.

### AC-E7: 공급자 인증 모드 — Claude 구독(인증 풀) 또는 게이트웨이
- **설명**: `claude-code`의 공급자 인증은 control plane 설정 `CLAUDE_CODE_AUTH_MODE`가 고른다 (2026-11-07 사용자 결정: 운영은 OpenRouter 게이트웨이에서 Claude 구독으로 옮긴다).
  - **`subscription`** (운영 기본, `k8s/deployment.yaml`): 풀 `claude-code`(`agent-auth-pool.md` AC-L1~L5, `CLAUDE_CODE_AUTH_SLOTS`)의 슬롯을 쓴다. 파드에는 credential-proxy 사이드카가 **없고**, 주 컨테이너는 `ANTHROPIC_BASE_URL`·`ANTHROPIC_AUTH_TOKEN`·`ANTHROPIC_API_KEY`·`CLAUDE_CODE_OAUTH_TOKEN`을 받지 않으며 하나라도 있으면 에이전트가 기동을 거부한다. CLI는 공급자에 직접 붙는다. 슬롯 파일은 `.credentials.json`이고 HOME 쪽 자리는 `$HOME/.claude/.credentials.json`이다(AC-L4). 주 컨테이너는 이 모드를 `CLAUDE_CODE_AUTH_MODE=subscription`으로 받는다. 모델은 게이트웨이와 따로 고른다 — 게이트웨이의 `model`·`models`(OpenRouter id)는 구독에서 뜻이 없고 `approval-gated`가 계속 읽으므로, 같은 ConfigMap의 선택 키 `subscription-model`·`subscription-models`(control plane `CLAUDE_CODE_SUBSCRIPTION_DEFAULT_MODEL`·`CLAUDE_CODE_SUBSCRIPTION_MODELS`)가 이 모드의 UI 카탈로그이고, `platform-default` 파드는 `subscription-model`을 읽는다(없으면 CLI 기본).
  - **`gateway`**: AC-E6 그대로 — loopback credential-proxy가 ConfigMap `base-url`과 Secret `auth-token`을 들고 주 컨테이너는 placeholder만 본다. 풀을 쓰지 않는다. e2e(공급자 대역)와 되돌리기용으로 남긴다.

  값이 비면 `gateway`, 그 밖의 값은 control plane 기동 실패다. `subscription`인데 풀 크기가 0이면 이 타입은 `GET /config`에 나오지 않는다(AC-L1). `approval-gated`는 이 설정과 무관하게 AC-F6의 게이트웨이 자격 증명을 쓴다 — 그 타입의 egress 허용 목록에 공급자가 없다.

  **빈 슬롯의 로그인** (`subscription`): Claude Code의 `claude auth login --claudeai`는 URL을 출력한 뒤 붙여 넣을 코드를 stdin에서 기다린다(device code가 아니다, 2.1.220에서 확인). 그래서 로그인을 두 번의 write로 나눈다.
  1. 턴 시작 시 `claude auth status --json`이 로그인 안 됨을 알리면, 에이전트가 `claude auth login --claudeai`를 띄우고 그 로그인 URL을 플랫폼 안내 줄로 내보낸다 — 「브라우저에서 열어 로그인하고, 화면에 나온 코드를 다음 메시지로 보내라」. 이 프롬프트는 실행하지 않고 보류하며, 턴은 `status=error`로 끝난다.
  2. 다음 write의 payload는 프롬프트가 아니라 **코드**다. 입력 기록에는 `[login code]`만 남기고 코드 자체는 출력에 쓰지 않는다. 에이전트가 코드를 로그인 프로세스의 stdin에 넣고, 성공하면 슬롯에 동기화한 뒤(AC-L4) 같은 턴에서 보류한 프롬프트를 실행한다. 실패하면 안내 한 줄을 남기고 보류한 프롬프트를 버린다 — 다음 프롬프트가 다시 1번부터 한다.
  3. 보류는 15분 뒤, 또는 동결·종료 시 로그인 프로세스를 끝내고 안내 한 줄과 함께 사라진다. 만료·동결로 사라진 뒤 30분 안의 첫 write는 늦게 온 코드로 보고 `[login code]`로만 기록하며 실행하지 않는다 — 코드가 프롬프트로 출력·모델에 새지 않게 한다.
  4. 코드로 읽는 것은 URL 안내 **뒤에** 받은 write뿐이다. 그 전에 이미 큐에 있던 프롬프트는 그대로 기록하되 실행하지 않고 「로그인 후 다시 보내라」는 안내를 남긴다.
  5. `claude auth status`가 시간 초과·실행 실패·읽을 수 없는 결과면 로그인을 시작하지 않는다 — 그 턴만 안내와 함께 실패하고 아무것도 보류하지 않는다.

  슬롯 동기화(AC-L4)는 그 턴의 시작 복사가 성공해 HOME에 파일을 놓았을 때만 턴 끝의 「HOME에 없음」을 로그아웃으로 읽고, 턴 끝 복사가 실패하면 다음 턴 시작은 HOME의 새 사본을 먼저 슬롯에 저장한다. HOME → 슬롯 복사는 내용이 JSON일 때만 한다.
- **달성 가치**: V3, V1
- **구체화 대상**: AC-E6(공급자 자격 증명 배치)를 구독 계정에 맞춘 것, AC-L4의 `claude-code` 슬롯 파일과 로그인 흐름
- **검증 방법**: `subscription`에서 제출된 pod spec에 credential-proxy 컨테이너·`ANTHROPIC_*` env가 없고 슬롯 PVC와 `CLAUDE_CODE_AUTH_MODE=subscription`이 있음을 확인한다. `gateway`에서는 AC-E6의 모양 그대로임을 확인한다. 에이전트 단위 테스트로 로그인 안 됨 → URL 안내·프롬프트 보류 → 다음 write가 코드로 쓰이고 출력에 코드가 없으며 성공 시 보류한 프롬프트가 실행됨을 확인한다. `subscription` 모드 주 컨테이너가 `ANTHROPIC_AUTH_TOKEN`이 있으면 기동을 거부함을 확인한다.

### AC-E8: 세션 추론 수준(`reasoningEffort`) — 생성 시 고정
- **설명**: 생성 요청은 선택 필드 `reasoningEffort`를 받는다. 이 필드를 받는 타입은 `claude-code`와 `codex-task`(AC-M4)뿐이고, 다른 타입에 주면 pod 생성 전 400이다. 값은 타입마다 정해진 목록 중 하나이고(`claude-code`: `low`·`medium`·`high`·`xhigh`·`max` — CLI의 `--effort`), 빈 문자열·비문자열·목록 밖은 400이다. 생략하면 세션에 저장하지 않고 CLI의 기본값을 쓴다. 값은 모델·타입처럼 **생성 시 고정**이다 — 세션 레코드의 `reasoningEffort`로 조회 응답에 나오고, 동결·복원을 건너 유지되며, 바꾸려면 새 세션을 만든다. `GET /config`의 각 타입 항목은 `reasoningEfforts` 배열을 내놓는다(받지 않는 타입은 빈 배열). `claude-code` 파드는 `CLAUDE_CODE_EFFORT`로 받고 모든 실행 argv에 `--effort <값>`을 붙인다. `approval-gated`는 받지 않는다.
- **달성 가치**: V8
- **구체화 대상**: AC-E1(타입 선택)과 같은 축 — 세션 생성 시 고르는 작업 환경에 추론 수준을 더한 것
- **검증 방법**: `reasoningEffort=max`로 만든 `claude-code` 세션의 조회 응답·파드 env·실행 argv에 그 값이 있음을 확인한다. `shell`·`approval-gated`·`codex-chat`에 주면 400이고 파드가 없음을 확인한다. `GET /config`가 타입별 목록을 내놓고, 세션 생성 화면이 그 목록이 빈 타입에서는 선택지를 숨김을 확인한다.
