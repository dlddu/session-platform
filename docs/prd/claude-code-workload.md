# PRD: 세션 워크로드 — 클로드 코드 CLI

> 대상 요구사항: 세션 워크로드 타입에 **클로드 코드 CLI** 추가
> (기존 인터랙티브 쉘과 병존하는 두 번째 타입, `shell-workload.md`와 대응 관계)

## 달성 가치
- **V8 목적에 맞는 작업 환경 선택** — 세션 생성 시 워크로드 타입을 고를 수 있게 하고, 선택과 무관하게 V1~V5 보장이 동일하게 성립함을 확정 (AC-E1)
- **V1 세션 격리** — 에이전트 실행과 자격 증명을 세션 전용 pod에 가둠 (AC-A1/A2 구체화)
- **V2 유휴 자원 회수** — CRIU 없이 파일시스템 아카이브 기반으로 동결·회수 (AC-B1의 타입별 경로)
- **V3 끊김 없는 세션 연속성** — 메모리가 아닌 **대화 기록·작업 디렉터리** 보존으로 연속성 확보(AC-B2/B3 구체화), write/read를 프롬프트·실행 출력 시맨틱으로 확정 (AC-C2/C3 구체화)
- **V4 자유로운 멀티세션 전환** — 전환 후에도 동일한 offset 커서 규약으로 출력을 이어 읽음 (AC-C2 구체화)

> 📌 **가치 연결 규칙 (2026-08-08)**: `shell-workload.md`와 동일하게, 각 AC의 달성 가치는 자신이 구체화하는 상위 AC의 가치를 상속한다. 이전 판에서 참조하던 "V7 에이전트 세션"은 워크로드 타입을 가치로 올린 것이어서 `../values.md`에서 삭제되었다.
>
> ⚠️ **다만 AC-E1(타입 선택)은 예외다.** 이 AC는 상위 AC의 구체화가 아니라 새 축이라 상속할 가치가 없다. 2026-08-08 3차 개정에서 이를 뒷받침할 **V8(목적에 맞는 작업 환경 선택)** 이 신설되어 AC-E1 → V8로 직접 연결되었다(부가로 V1: 타입이 달라도 1세션=1전용 pod 매핑 유지).

> 이 PRD는 세션 워크로드가 **단수(인터랙티브 쉘)에서 복수(타입)로 확장**되는 변경을 다룬다.
> AC-E1이 타입 선택이라는 새 축을 도입하고, AC-E2~E6은 `claude-code` 타입에서 기존 AC들
> (AC-A1·B1·B2·B3·C2·C3)이 어떻게 구체화되는지를 못박는다. 쉘 타입의 대응 명세는
> `shell-workload.md`(AC-D1~D5)이며, 두 문서는 같은 상위 AC에 대한 **타입별 구체화**로 나란히 존재한다.

## Acceptance Criteria

### AC-E1: 세션 워크로드 타입 선택
- **설명**: 세션 생성 요청은 워크로드 타입 `workloadType`을 받는다. 허용값은 `shell`과 `claude-code`이며, **필드 생략만** `shell` 기본값으로 해석한다(explicit empty/null/비문자열은 400). 타입은 세션 수명 동안 **불변**이다(타입을 바꾸려면 새 세션을 만든다). 하나의 세션은 정확히 하나의 워크로드 타입을 가지고, control plane은 타입에 따라 서로 다른 data plane 워크로드(에이전트 모드/이미지)로 pod를 프로비저닝한다. 타입이 늘어나도 AC-A1(control plane은 워크로드를 직접 실행하지 않음)과 AC-A2(1 세션 = 1 전용 pod)는 그대로 유지된다.
- **달성 가치**: V8, V1
- **구체화 대상**: AC-A1의 "실제 세션 워크로드" — 이제 **타입별로 분기**한다 (AC-D1은 `shell` 타입의 구체화, AC-E1~E6은 `claude-code` 타입의 구체화)
- **검증 방법**: `workloadType=claude-code`로 세션을 생성하면 해당 pod에서 클로드 코드 CLI 실행이 가능하고, `workloadType` 미지정 생성은 기존과 동일하게 `shell` 세션으로 동작함을 확인한다. explicit `""`/`null`/비문자열과 허용값 외 타입은 pod 생성 전 400으로 거부됨을 확인한다. 기존 세션 route에 workload/model 변경 필드를 보내면 400이고 원래 값이 유지됨을 확인한다.

### AC-E2: write = 프롬프트 1회 실행
- **설명**: `claude-code` 세션에서 `POST /sessions/{id}/write`의 `payload`는 **프롬프트**로 해석되어, 세션 pod 안에서 다음 형태의 1회 실행을 트리거한다:

  ```
  ANTHROPIC_BASE_URL=http://127.0.0.1:8091  ANTHROPIC_AUTH_TOKEN=<비밀이 아닌 proxy placeholder> \
    claude [--continue] [--model <선택된 모델>] -p \
      --output-format stream-json --verbose --include-partial-messages -- "<payload>"
  ```

  실제 공급자 HTTPS URL·토큰은 별도 credential-proxy sidecar에만 SecretKeyRef로 주입된다. CLI와 도구가 실행되는 주 컨테이너는 localhost proxy와 비밀이 아닌 placeholder만 보며, proxy가 HTTPS 목적지를 고정하고 요청 자격 증명을 실제 토큰으로 교체한다. 평문 HTTP upstream은 같은 pod 네트워크에서의 패킷 관찰 위험 때문에 시작 시 거부한다.

  `-p`는 print(비대화) 모드이고 `--` 뒤 payload는 옵션이 아닌 단일 positional argv로 전달된다. `stream-json`과 partial-message 플래그는 실행이 끝나기 전의 assistant `text_delta`를 stdout JSONL로 내보낸다. data plane은 JSONL 원문이나 최종 result를 사용자 출력으로 저장하지 않고, `text_delta`만 한 번씩 순서대로 투영하며 diagnostic stderr도 같은 append-only 출력에 합친다. 세션 metadata가 `platform-default`이면 pod spec이 credentials Secret의 optional `model` 키를 주 컨테이너의 `CLAUDE_CODE_MODEL` SecretKeyRef로 둔다. 주 컨테이너가 시작하거나 재시작할 때 해석한 값이 비어 있지 않으면 `--model <Secret 값>`을 넣고, 키가 없거나 값이 비어 있으면 `--model`을 생략해 설치된 Claude CLI 기본 선택에 위임한다. 구체 model을 명시한 세션은 SecretKeyRef 대신 그 값을 literal `CLAUDE_CODE_MODEL`로 받아 Secret 기본값보다 우선하며 `--model`의 단일 argv로 전달한다. **첫 성공 실행 이후**부터 `--continue`도 추가한다(실패·timeout인 첫 실행은 대화를 만든 것으로 보지 않는다). 실행은 **원샷**이다 — 프로세스는 응답을 출력한 뒤 종료하며, 세션이 살아 있는 동안 상주하는 CLI 프로세스는 없다. write는 쉘 타입(AC-D2)과 동일한 비블로킹 규약을 따른다: 실행 완료를 기다리지 않고 즉시 반환하고, 실행 중 출력은 AC-E3의 stream/read로 회수한다. 한 prompt payload는 최대 1 MiB이며 초과 write는 큐에 넣지 않고 `413 Payload Too Large`로 거부한다. 동일 세션에서 이전 실행이 아직 끝나지 않은 상태에서 다음 write가 오면 **직렬로 큐잉**된다 — 한 세션 안에서 두 개의 `claude` 실행이 동시에 돌지 않으며, 이것이 AC-E4의 대화 순서를 보장한다. bounded queue가 포화된 새 write는 `429 Too Many Requests`로 거부된다.

  각 invocation에서 증분 redaction 후 저장되는 **assistant text + diagnostic stderr는 marker를 포함해 16 MiB**로 제한한다. raw stream-json framing은 이 quota나 cursor에 포함하지 않는다. 넘친 tail은 버리고 이미 노출한 bytes를 고치지 않은 채 보존된 prefix 끝에 `[session-platform: invocation output truncated at 16 MiB]`를 붙인다. 해당 invocation과 이미 받아 둔 후속 queue는 정상적으로 직렬 실행한다.
- **달성 가치**: V3
- **구체화 대상**: AC-C3(Write API 상태별 분기)의 "write" 시맨틱 — `claude-code` 타입 버전
- **검증 방법**: active 세션에 `payload="1+1은?"`로 write 후 pod에서 위 exact argv의 `claude` 프로세스가 1회 기동되고 응답 출력 후 종료함을 확인한다. write가 실행 완료를 기다리지 않고 반환하며, fake runner가 첫 delta 뒤 대기하는 동안에도 그 delta가 read와 SSE에 보이는지 확인한다. 연속 2회 write 시 두 실행이 겹치지 않고 순서대로 수행됨을 확인한다. 첫 실행 실패·timeout 뒤에는 `--continue`가 붙지 않고 첫 성공 뒤에만 붙음을 확인한다. prompt가 1 MiB를 넘으면 public API까지 413이고 실행되지 않으며, queue 포화 시 429, invocation 출력 초과 시 16 MiB 경계 안의 append-only truncation marker를 확인한다.

> ✅ **구현 결정 (headless 권한 정책, 2026-08-08)**: 전용 session pod를 실행 경계로 삼고, 세션 HOME의 플랫폼 관리 `.claude/settings.json`에 코딩에 필요한 `Read`·`Write`·`Edit`·`Glob`·`Grep`·`Bash`만 허용한다. `--dangerously-skip-permissions`는 사용하지 않으며 설정 파일은 파일시스템 아카이브에 포함해 복원 후에도 동일 정책을 유지한다.

### AC-E3: read/stream = 실행 중 출력, offset 커서 기반 델타
- **설명**: `claude-code` 세션의 출력은 쉘 타입(AC-D3)과 **동일한 append-only offset 커서 규약**을 따른다. `POST /sessions/{id}/read`는 요청 `offset`(직전 응답이 발급한 `nextOffset`, 생략 시 0) 이후에 누적된 출력을 반환하고 새 `nextOffset`을 발급한다. 이 JSON read는 비파괴적 catch-up·reconcile·`offset=0` 전체 replay 용으로 계속 지원한다.

  정상 UI 경로는 `GET /sessions/{id}/stream?offset=N`의 workspace 수명 SSE다. 실행이 끝날 때까지 기다리지 않고 안전하게 확정된 새 bytes마다 `event: output`을 보낸다. event id는 서버가 발급한 `nextOffset`이고 data는 `{offset,payloadBase64,nextOffset}`이다. base64는 정확한 byte 범위·길이·overlap 검증을 보존하기 위한 wire encoding이며 UTF-8 rune 중간 cursor를 허용하기 위한 것이 아니다. 증분 redaction을 마친 Claude 저장 출력은 valid UTF-8이고 모든 서버 발급 cursor와 event 경계는 code-point 경계다. 재연결 요청에 `Last-Event-ID`가 있으면 query `offset`보다 우선한다. 연결은 한 invocation 완료로 닫히지 않으며 run/queue 상태 event도 만들지 않는다. comment keepalive는 연결만 유지하고 output cursor나 `lastAccess`를 바꾸지 않는다. stream은 passive 관찰 경로라 active/idle의 기존 pod만 읽고 상태를 승격하거나 snapshot을 복원하지 않으며 `lastAccess`도 touch하지 않는다.

  누적 대상은 stdout stream-json에서 순서대로 투영한 assistant `text_delta`와 diagnostic stderr다. raw JSONL과 delta를 중복하는 최종 result는 누적하지 않는다. credential literal과 불완전 UTF-8 suffix는 다음 runner write까지 보류할 수 있으며, 한 번 cursor로 노출한 bytes는 이후 절대 다시 쓰지 않는다. 요청 cursor가 현재 보존 길이보다 크면 stream은 출력 대신 `event: reset`, `id=currentLength`, `{"nextOffset":currentLength}`를 보낸다. 브라우저는 미완성 decoder state를 버리고 `POST /read`의 `offset=0` 전체 이력으로 콘솔을 교체한 뒤 그 read의 `nextOffset`에서 stream을 다시 연다. SSE reset 신호 자체는 passive지만 이 `POST /read` 복구는 일반 Read API이므로 idle을 active로 승격하고 `lastAccess`를 갱신할 수 있다. 전송 오류에서는 native 자동 재접속을 먼저 닫고 세션 상태를 확인한다. active/idle이면 마지막 cursor로 backoff 재연결하고, snapshot이면 자동 stream/read로 복원하지 않고 Restore 화면으로 이동한다. 즉 워크로드 타입이 달라도 **보존·커서 계약은 동일**하고, 그 안에 담기는 내용만 쉘 출력과 에이전트 출력으로 갈린다.

  Claude 누적 scrollback은 terminal marker를 포함해 **256 MiB**로 제한한다. 다음 출력이 이 경계를 넘기려는 순간 기존 bytes를 다시 쓰지 않고 남은 prefix와 `[session-platform: session output limit reached; further prompts are disabled]`를 append해 닫는다. 따라서 이미 발급한 byte offset은 그대로 유효하다. 이 전에 수락된 queued prompt는 끝까지 실행하되 이후 출력은 버리고, terminal marker 뒤의 새 write는 `507 Insufficient Storage`로 거부한다.
- **달성 가치**: V3, V4
- **구체화 대상**: AC-C2의 JSON read와 같은 보존 cursor를 사용하는 `claude-code` live output 구체화
- **검증 방법**: write 후 process가 종료되기 전에 둘 이상의 SSE output delta가 순서대로 도착하고, event id/data cursor와 decoded byte 수가 일치하며 모든 cursor가 UTF-8 code-point 경계임을 확인한다. 중간에 연결을 끊은 뒤 `Last-Event-ID`로 재접속해 누락·중복 없이 이어 받고, 동일 cursor의 POST read가 같은 저장 bytes를 valid UTF-8로 반환하며 `offset=0`은 전체 이력을 반환함을 확인한다. current length보다 큰 cursor에는 reset이 발급되고 SPA가 decoder state 폐기→read(0) 화면 교체→새 cursor stream 순으로 복구해야 한다. runner write가 UTF-8 rune이나 credential 중간에서 갈려도 잘못된 문자나 secret이 한 번도 노출되지 않아야 한다. 축소한 결정적 limit로 기존 커서를 발급한 뒤 256 MiB와 같은 경계 로직을 넘겨 live terminal marker·커서 불변·accepted queue drain·후속 507을 검증한다.

### AC-E4: 대화 연속성 = pod에 지속되는 대화 기록
- **설명**: 같은 세션에 대한 연속된 write는 **하나의 이어지는 대화**로 처리된다. 각 실행은 원샷이지만, 클로드 코드 CLI가 pod 파일시스템에 남기는 대화 기록을 다음 실행이 이어받으므로, N번째 프롬프트는 앞서 **성공한** 프롬프트·응답을 문맥으로 갖는다. 성공한 첫 실행까지는 새 대화 모드이고 그 이후 write부터 재개(resume) 모드다(첫 실행 실패·timeout은 resume 상태를 만들지 않는다). 세션의 작업 디렉터리도 실행 간 유지되어, 이전 실행이 만든 파일을 다음 실행이 그대로 본다.
- **달성 가치**: V3
- **검증 방법**: `write("내 이름은 다르마야")` → `write("내 이름이 뭐야?")` 후 read하면 두 번째 응답이 "다르마"를 포함함을 확인한다(문맥 이어짐). 별개 세션에서 같은 두 번째 프롬프트를 실행하면 이름을 알지 못함을 확인한다(세션 간 대화 격리, AC-A2). `write("touch marker.txt")` 후 `write("작업 디렉터리에 어떤 파일이 있어?")` 응답에 `marker.txt`가 포함됨을 확인한다.

> ✅ **구현 결정 (재개 방식, 2026-08-08)**: 첫 실행은 새 대화를 시작하고, 성공한 첫 실행 이후부터 `--continue`로 해당 세션 pod의 최근 대화를 잇는다. 세션별 고정 HOME과 작업 디렉터리를 사용하며, 별도 대화 ID를 control-plane 메타데이터에 저장하지 않는다. resume 플래그와 CLI 기록은 파일시스템 아카이브에 함께 보존된다.

### AC-E5: CRIU 비대상 — 파일시스템 아카이브 기반 동결·복원
- **설명**: `claude-code` 세션은 **CRIU 체크포인트 대상이 아니다.** 원샷 실행 모델이라 동결 시점에 보존해야 할 상주 프로세스 트리가 존재하지 않기 때문이다(쉘 타입은 상주 쉘의 메모리 상태가 곧 세션 상태이므로 CRIU가 필요했다 — AC-D4). 대신 유휴 한계 도달 시(AC-B1) 시스템은 세션의 **작업 디렉터리·대화 기록·누적 출력 버퍼를 아카이브하여 외부 스토리지에 저장**하고 pod를 회수한다. 재접근 시(AC-B2)에는 새 pod에 아카이브를 복원한 뒤 `active`로 전이하며, 복원 후의 write는 동결 이전 대화 문맥과 작업 디렉터리 위에서 이어진다. 누적 출력 버퍼(256 MiB terminal marker와 output-full 상태 포함)가 아카이브에 포함되므로 **복원 전 발급된 `nextOffset` 커서는 복원 후에도 유효**하고, 이미 output-full이었다면 복원 후 새 write도 계속 507이다(AC-E3의 커서 연속성 — 쉘 타입은 scrollback을 CRIU 이미지와 같은 체크포인트 아카이브에 직렬화하고, 이 타입은 filesystem archive에 직렬화한다).
- **달성 가치**: V2, V3
- **구체화 대상**: AC-B1(동결)·AC-B2(복원)·AC-B3(무결성)의 **메커니즘이 타입별로 분기**함 — `shell`은 CRIU 체크포인트(AC-D4), `claude-code`는 파일시스템 아카이브(AC-E5)
- **검증 방법**: `claude-code` 세션이 유휴 한계에 도달하면 CRIU dump 없이 아카이브가 생성되고 pod가 회수됨을 확인한다(AC-A3의 자원 회복도 동일하게 성립). 재접근 시 새 pod로 복원되어 `active`가 되고, 동결 전 대화를 참조하는 프롬프트가 정상 응답하며 동결 전에 만든 파일이 작업 디렉터리에 남아 있음을 확인한다. 동결 직전 확보한 `nextOffset`으로 복원 후 read하면 델타만 반환되고, `offset=0`은 동결 전·후 전체 이력을 반환함을 확인한다.
> 🔐 작업 디렉터리·대화·출력을 외부 저장소로 전송하므로 이 경로는 `CLAUDE_CODE_ARCHIVE_ENABLED=true`로 명시적으로 허용해야 한다(기본값 `false`). 비활성 상태에서는 pod를 회수하지 않고 snapshot 요청을 거부한다.

> ✅ **구현 결정 (아카이브 crash recovery, 2026-08-08)**: control plane은 128-bit generation·현재 Lease owner·source pod를 가진 private durable `preparing` transaction을 먼저 aggregate CAS로 저장한 뒤, 그 generation을 agent에 넘긴다. agent는 admission을 닫고 accepted queue를 drain해 같은 generation의 archive를 내보낸다. archive ref가 durable해지면 control plane은 pod 삭제 전에 transaction을 `committing`으로 CAS한다. `preparing` 실패/재시작은 새 holder가 owner를 먼저 CAS로 claim한 뒤 **그 generation만** abort하고 조건부로 transaction을 지운다. `committing` 이후의 Stop/finalize 실패는 abort하거나 admission을 다시 열지 않고, recovery가 Stop을 idempotently 재시도한 뒤 한 번의 aggregate CAS로 `snapshot`을 확정한다. 활성 checkpoint와 다른 generation의 늦은 abort는 409이므로 새 barrier를 열 수 없고, 이미 abort된 generation 재요청은 idempotent하다.
>
> Snapshot·Restore·recovery는 기본 15초 Lease를 5초마다 갱신하며 각 Renew 호출에도 5초 deadline을 둔다. holder 상실·hung Renew는 진행 중 side effect의 context를 취소한다. `Touch`는 최신 aggregate의 `lastAccess`만 단조 증가시키고, transaction CAS는 더 최신 `lastAccess`를 merge하므로 owner/phase fence를 stale metadata가 덮지 않는다.


### AC-E6: 자격 증명은 sidecar Secret, 플랫폼 모델 설정은 optional Secret
- **설명**: 플랫폼 전역 Kubernetes Secret의 **필수** `base-url`·`auth-token` 키는 각각 `ANTHROPIC_BASE_URL`·`ANTHROPIC_AUTH_TOKEN`으로 세션 pod의 localhost credential-proxy sidecar에만 주입된다. Claude CLI·Bash가 실행되는 주 컨테이너에는 실제 자격 증명이나 service-account token을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다. 두 컨테이너는 네트워크만 공유하고 PID·파일시스템은 공유하지 않는다. proxy는 시작 시 **HTTPS** upstream origin을 고정하고, 허용 목록에 든 Anthropic/Claude/Stainless 요청 헤더만 전달한 뒤 플랫폼 토큰 하나를 주입한다. CONNECT·Upgrade와 모든 요청 trailer를 거부/제거하고, 중간 1xx를 포함한 응답에서 토큰 리터럴이 클라이언트로 노출되지 않게 한다. Provider SSE response는 raw upstream 64 MiB 상한 내에서 whole-body buffering 없이 증분 전달하여 안전한 upstream flush가 EOF 전에 Claude CLI에 도달한다. proxy의 tail-safe redactor는 token 후보 suffix를 response read 경계 너머까지 보류하므로 split credential도 일시적으로 노출하지 않는다. 자격 증명은 세션 생성 요청으로 지정할 수 없고, 세션 조회 응답·로그·read·SSE 출력 어디에도 토큰 값이 노출되지 않는다. runner 출력의 별도 증분 redactor도 token이 여러 runner write에 걸쳐도 가능한 match suffix를 보류한다. 반면 Secret의 `model` 키는 자격 증명이 아닌 **optional 플랫폼 기본값**이다. 세션 metadata가 `platform-default`일 때만 주 컨테이너의 `CLAUDE_CODE_MODEL`에 optional SecretKeyRef로 주입되고 sidecar에는 노출되지 않는다. 키가 없거나 값이 비면 Claude CLI의 기존 기본 선택을 쓰도록 `--model`을 생략한다. 생성 요청의 `model` 필드에 구체 값을 명시하면 SecretKeyRef 대신 그 값을 literal `CLAUDE_CODE_MODEL`로 주입하여 플랫폼 기본값보다 우선한다. model 필드의 **생략만** 안정적인 별칭 `platform-default`로 해석하며 explicit empty/null/비문자열, 앞뒤 공백이 있는 값, 허용 pattern 밖의 값은 pod 생성 전에 400으로 거부한다. 세션에 저장된 model 또는 별칭은 수명 동안 불변이다(다른 설정을 고르려면 새 세션을 만든다 — AC-E1의 타입 불변성과 같은 규칙).

  주 컨테이너는 Secret 또는 literal에서 얻은 non-empty effective model도 `^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`로 다시 검증한다. 선행 `~` 하나는 OpenRouter moving alias를 지원하며 전체 길이 상한은 128자다. invalid Secret 값은 CLI argv로 넘기지 않고 data-plane agent 시작을 실패시킨다.

  별도의 optional `models` 키는 모델 선택 UI를 위한 **순서가 있는 soft catalog**이며 JSON string array로 저장한다. Deployment는 공개 설정인 `model`과 `models`를 각각 control plane의 `CLAUDE_CODE_DEFAULT_MODEL`과 `CLAUDE_CODE_MODELS` 환경 변수로 키 단위 투영하고, credential인 `base-url`·`auth-token`은 주입하지 않으며 Secret 읽기 권한도 주지 않는다. `CLAUDE_CODE_CREDENTIALS_SECRET` 이름을 기본값에서 바꾸면 Deployment의 `CLAUDE_CODE_DEFAULT_MODEL`과 `CLAUDE_CODE_MODELS`의 `valueFrom.secretKeyRef.name`을 같은 literal 이름으로 함께 patch해야 한다(Kubernetes는 env 값을 Secret ref 이름으로 동적 치환하지 않는다). 값이 없거나 빈 문자열 또는 `[]`이면 catalog는 빈 배열이고 기존 free-text 모델 입력을 유지한다. 비어 있지 않은 값은 control-plane 시작 시 JSON string array인지 엄격히 검사하고, 각 항목에 세션 model과 같은 pattern(`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`)을 적용한다. 빈 항목·앞뒤 공백·중복·reserved alias `platform-default`·JSON `null`·배열 외 값은 설정 오류로 시작을 실패시킨다. `GET /api/v1/config`는 concrete Secret model이 있으면 이를 `defaultModel`로, missing/empty면 `platform-default`를 반환하고 `models` catalog와 함께 `Cache-Control: no-store`로 공개한다. UI는 concrete default를 `<model> (platform default)` 한 항목으로 합쳐 표시하고 그 선택의 create request에서는 model을 생략한다. provider credential은 공개하지 않는다. catalog는 선택 편의만 제공하므로 목록 밖의 유효한 model도 Create Session API가 계속 허용한다.

  singular `model` Secret 변경은 `platform-default` 주 컨테이너가 다음에 시작될 때(새 세션 pod, snapshot 복원으로 재생성되는 pod, 기존 pod의 container restart) 적용되고 실행 중인 컨테이너나 concrete-model 세션 환경을 즉시 바꾸지 않는다. UI에 표시하는 singular `model`과 plural `models`는 모두 control-plane 시작 시 읽는 환경 snapshot이므로 Deployment rollout 뒤에 config API와 UI에 반영된다. 따라서 세션 metadata의 `platform-default` 별칭은 불변이지만 복원·재시작 시점의 플랫폼 기본 설정이 달라졌다면 유효 provider model은 달라질 수 있다.
- **달성 가치**: V1
- **검증 방법**: Secret의 필수 HTTPS `base-url`·`auth-token`이 sidecar에만 주입되고 주 컨테이너에는 localhost URL·placeholder만 있음을 확인한다. sidecar가 loopback에만 바인딩되고 평문 HTTP·고정 upstream 외 목적지·허용 목록 밖 요청 헤더·CONNECT/Upgrade를 거부하거나 제거하며, 중간 1xx 응답도 자격 증명을 누출하지 않고 pod가 service-account token을 자동 마운트하지 않음을 확인한다. 지연 가능한 테스트 upstream이 첫 safe SSE chunk를 flush한 뒤 EOF를 막아도 proxy client가 release 전에 그 chunk를 받고, token을 response read 여러 개에 나누어도 어느 pre-release chunk에도 원문이 없으며 raw upstream 64 MiB SSE 상한을 full-body buffer 없이 지키는지 확인한다. 생성 요청의 자격 증명 필드는 unknown field로 400 거부되고 세션 조회 응답과 read/SSE 출력에 토큰 문자열이 포함되지 않음을 확인한다. runner에서도 token을 여러 stdout/stderr write에 나누어도 중간 SSE event에 원문이 없어야 한다. `platform-default` 세션의 주 컨테이너만 optional `model` SecretKeyRef를 가지며, Secret 값이 비어 있지 않으면 그 값으로 실행하고 키가 없거나 값이 비면 `--model`을 생략하는지 확인한다. 구체 `model`로 생성한 세션은 literal 환경 변수를 사용해 Secret 기본값보다 우선하고, explicit empty/null/비문자열·앞뒤 공백·invalid pattern은 400임을 확인한다.

  공개 `model`과 `models`만 control-plane 환경에 키 단위 투영되고 credential은 없는지 확인하고, custom credentials Secret 이름을 쓸 때 두 Secret ref 이름이 함께 patch되는지 확인한다. unset/empty/`[]`는 config API의 non-null 빈 배열과 free-text UI로 이어져야 한다. 올바른 배열은 순서대로 노출되고, malformed JSON·null·배열 외 값·빈/공백/invalid/중복/reserved 항목은 control-plane startup 오류인지 확인한다. config 응답은 concrete default 또는 fallback `platform-default`와 catalog만 포함하고 `Cache-Control: no-store`이며, catalog 밖 valid model도 API로 생성되는지 검증한다. singular `model` 변경은 새/복원 pod와 container restart의 실행값에 반영되고, singular/plural UI snapshot은 control-plane rollout 뒤에 함께 반영되는지 확인한다.

> ✅ **구현 결정 (모델 정책, 2026-08-08; 2026-08-09 개정)**: model은 workload type과 함께 생성 시 확정되어 불변이다. `platform-default`는 특정 공급자 모델 버전을 API에 하드코딩하지 않는 세션 메타데이터 별칭이다. 이 별칭의 유효 모델은 credentials Secret의 optional `model` 키로 운영자가 선택하며, 키가 없거나 비면 설치된 Claude Code CLI의 기본 선택에 위임한다. 구체 세션 model은 literal 값으로 이 플랫폼 기본값보다 우선한다.

> ✅ **구현 결정 (모델 카탈로그, 2026-08-09)**: optional `model`과 `models`는 control-plane rollout 단위의 UI default와 soft catalog다. `GET /api/v1/config`가 concrete default 또는 fallback alias와 catalog를 공개하되 Create Session API의 allowlist로 사용하지 않으며, 빈 catalog는 기존 free-text 입력을 보존한다.

> ✅ **구현 결정 (이미지/모드, 2026-08-08)**: 하나의 data-plane 이미지에 PTY shell, CRIU, Claude Code CLI를 포함하고 `DATA_PLANE_WORKLOAD`로 시작 모드를 분기한다. Kubernetes pod label/env와 control-plane의 immutable workload spec이 선택한 모드를 끝까지 전달한다.
