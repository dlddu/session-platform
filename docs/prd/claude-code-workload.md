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
    claude [--model <명시 모델>] -p -- "<payload>"
  ```

  실제 공급자 HTTPS URL·토큰은 별도 credential-proxy sidecar에만 SecretKeyRef로 주입된다. CLI와 도구가 실행되는 주 컨테이너는 localhost proxy와 비밀이 아닌 placeholder만 보며, proxy가 HTTPS 목적지를 고정하고 요청 자격 증명을 실제 토큰으로 교체한다. 평문 HTTP upstream은 같은 pod 네트워크에서의 패킷 관찰 위험 때문에 시작 시 거부한다.

  `-p`는 print(비대화) 모드이고 `--` 뒤 payload는 옵션이 아닌 단일 positional argv로 전달된다. 플랫폼 기본 모델 세션은 `--model`을 생략하고, 명시 모델 세션만 해당 플래그를 넣는다. **첫 성공 실행 이후**부터 `--continue`도 추가한다(실패·timeout인 첫 실행은 대화를 만든 것으로 보지 않는다). 실행은 **원샷**이다 — 프로세스는 응답을 출력한 뒤 종료하며, 세션이 살아 있는 동안 상주하는 CLI 프로세스는 없다. write는 쉘 타입(AC-D2)과 동일한 비블로킹 규약을 따른다: 실행 완료를 기다리지 않고 즉시 반환하고, 출력은 AC-E3의 read로 회수한다. 한 prompt payload는 최대 1 MiB이며 초과 write는 큐에 넣지 않고 `413 Payload Too Large`로 거부한다. 동일 세션에서 이전 실행이 아직 끝나지 않은 상태에서 다음 write가 오면 **직렬로 큐잉**된다 — 한 세션 안에서 두 개의 `claude` 실행이 동시에 돌지 않으며, 이것이 AC-E4의 대화 순서를 보장한다. bounded queue가 포화된 새 write는 `429 Too Many Requests`로 거부된다.

  각 invocation의 병합된 **raw stdout/stderr 수집량은 marker를 포함해 16 MiB**로 제한한다. 넘친 tail은 버리고 보존된 prefix 끝에 `[session-platform: invocation output truncated at 16 MiB]`를 붙인 뒤에도, 해당 invocation과 이미 받아 둔 후속 queue는 정상적으로 직렬 실행한다.
- **달성 가치**: V3
- **구체화 대상**: AC-C3(Write API 상태별 분기)의 "write" 시맨틱 — `claude-code` 타입 버전
- **검증 방법**: active 세션에 `payload="1+1은?"`로 write 후 pod에서 위 형태의 `claude` 프로세스가 1회 기동되고 응답 출력 후 종료함을 확인한다. write가 실행 완료를 기다리지 않고 반환함을 확인한다. 연속 2회 write 시 두 실행이 겹치지 않고 순서대로 수행됨을 확인한다. 첫 실행 실패·timeout 뒤에는 `--continue`가 붙지 않고 첫 성공 뒤에만 붙음을 확인한다. prompt가 1 MiB를 넘으면 public API까지 413이고 실행되지 않으며, queue 포화 시 429, invocation 출력 초과 시 16 MiB 경계 안의 truncation marker를 확인한다.

> ✅ **구현 결정 (headless 권한 정책, 2026-08-08)**: 전용 session pod를 실행 경계로 삼고, 세션 HOME의 플랫폼 관리 `.claude/settings.json`에 코딩에 필요한 `Read`·`Write`·`Edit`·`Glob`·`Grep`·`Bash`만 허용한다. `--dangerously-skip-permissions`는 사용하지 않으며 설정 파일은 파일시스템 아카이브에 포함해 복원 후에도 동일 정책을 유지한다.

### AC-E3: read = 실행 출력, offset 커서 기반 델타
- **설명**: `claude-code` 세션의 read는 쉘 타입(AC-D3)과 **동일한 offset 커서 규약**을 따른다. `POST /sessions/{id}/read`는 요청 `offset`(직전 응답이 발급한 `nextOffset`, 생략 시 0) 이후에 누적된 출력을 반환하고 새 `nextOffset`을 발급한다. 누적 대상은 이 세션에서 수행된 `claude` 실행들의 stdout/stderr가 **실행 순서대로 병합**된 것이다. read는 비파괴적이며 `offset=0`은 언제나 세션 시작 이후 서버가 보존한 전체 출력을 반환한다. 즉 워크로드 타입이 달라도 **API 계약(커서 시맨틱)은 동일**하고, 그 안에 담기는 내용(쉘 출력 vs 에이전트 응답)만 다르다.

  Claude 누적 scrollback은 terminal marker를 포함해 **256 MiB**로 제한한다. 다음 출력이 이 경계를 넘기려는 순간 기존 bytes를 다시 쓰지 않고 남은 prefix와 `[session-platform: session output limit reached; further prompts are disabled]`를 append해 닫는다. 따라서 이미 발급한 byte offset은 그대로 유효하다. 이 전에 수락된 queued prompt는 끝까지 실행하되 이후 출력은 버리고, terminal marker 뒤의 새 write는 `507 Insufficient Storage`로 거부한다.
- **달성 가치**: V3, V4
- **구체화 대상**: AC-C2(Read API 상태별 분기)의 "read" 시맨틱 — `claude-code` 타입 버전
- **검증 방법**: write 후 read(`offset=0`)에 해당 실행의 응답이 포함됨을 확인한다. 그 `nextOffset`으로 곧바로 재-read하면 (새 실행이 없는 한) 빈 `payload`가 반환되고, 새 write 후에는 커서 이후 신규 출력만 반환됨을 확인한다. `offset=0` 재조회는 여전히 보존된 전체 출력을 반환함을 확인한다. 축소한 결정적 limit로 기존 커서를 발급한 뒤 256 MiB와 같은 경계 로직을 넘겨 terminal marker·커서 불변·accepted queue drain·후속 507을 검증한다.

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


### AC-E6: 자격 증명은 Secret 주입, 모델은 세션별 설정
- **설명**: `ANTHROPIC_BASE_URL`과 `ANTHROPIC_AUTH_TOKEN`은 **플랫폼 전역 Kubernetes Secret**에서 세션 pod의 localhost credential-proxy sidecar에만 환경 변수로 주입된다. Claude CLI·Bash가 실행되는 주 컨테이너에는 실제 자격 증명이나 service-account token을 주입하지 않고, localhost URL과 비밀이 아닌 proxy placeholder만 준다. 두 컨테이너는 네트워크만 공유하고 PID·파일시스템은 공유하지 않는다. proxy는 시작 시 **HTTPS** upstream origin을 고정하고, 허용 목록에 든 Anthropic/Claude/Stainless 요청 헤더만 전달한 뒤 플랫폼 토큰 하나를 주입한다. CONNECT·Upgrade와 모든 요청 trailer를 거부/제거하고, 중간 1xx를 포함한 응답에서 토큰 리터럴이 클라이언트로 노출되지 않게 한다. 자격 증명은 세션 생성 요청으로 지정할 수 없고, 세션 조회 응답·로그·read 출력 어디에도 토큰 값이 노출되지 않는다. 반면 model은 **세션별 설정**으로, 생성 요청의 `model` 필드로 지정하며 **필드 생략만** 안정적인 별칭 `platform-default`로 해석한다. explicit empty/null/비문자열, 앞뒤 공백이 있는 값, 허용 pattern 밖의 값은 pod 생성 전에 400으로 거부한다. `platform-default` 별칭은 CLI의 플랫폼 기본 선택을 쓰도록 `--model`을 생략한다. 명시한 model만 단일 argv로 전달하며, 세션 model은 수명 동안 불변이다(다른 모델을 쓰려면 새 세션을 만든다 — AC-E1의 타입 불변성과 같은 규칙).
- **달성 가치**: V1
- **검증 방법**: Secret의 HTTPS base URL·토큰이 sidecar에만 주입되고 주 컨테이너에는 localhost URL·placeholder만 있음을 확인한다. sidecar가 loopback에만 바인딩되고 평문 HTTP·고정 upstream 외 목적지·허용 목록 밖 요청 헤더·CONNECT/Upgrade를 거부하거나 제거하며, 중간 1xx 응답도 자격 증명을 누출하지 않고 pod가 service-account token을 자동 마운트하지 않음을 확인한다. 생성 요청의 자격 증명 필드는 unknown field로 400 거부되고 세션 조회 응답과 read 출력에 토큰 문자열이 포함되지 않음을 확인한다. 서로 다른 `model`로 생성한 두 세션이 각자의 model로 실행되고, `model` 미지정 세션은 플랫폼 기본 모델로 실행되며 explicit empty/null/비문자열·앞뒤 공백·invalid pattern은 400임을 확인한다.

> ✅ **구현 결정 (모델 정책, 2026-08-08)**: model은 workload type과 함께 생성 시 확정되어 불변이다. `platform-default`는 특정 공급자 모델 버전을 API에 하드코딩하지 않는 세션 메타데이터 별칭이며, 실제 기본 모델 선택은 플랫폼이 설치한 Claude Code CLI에 위임한다.

> ✅ **구현 결정 (이미지/모드, 2026-08-08)**: 하나의 data-plane 이미지에 PTY shell, CRIU, Claude Code CLI를 포함하고 `DATA_PLANE_WORKLOAD`로 시작 모드를 분기한다. Kubernetes pod label/env와 control-plane의 immutable workload spec이 선택한 모드를 끝까지 전달한다.
