# J6: 목적에 맞는 작업 환경을 골라 에이전트에게 일을 시킨다

> 사용자 여정 문서의 일부입니다. 페르소나 정의·가치 커버리지·미해결 항목은 [README.md](./README.md) 참고.

- **페르소나**: P1 멀티세션 작업자
- **상황**: 작업자가 직접 명령을 치는 대신 **자연어로 일을 맡기고 싶어** 한다. 세션을 만들 때 인터랙티브 쉘 대신 클로드 코드 CLI 환경을 고르고, 프롬프트를 보내 응답을 받으며 하나의 이어지는 대화로 작업을 진행한다.
- **달성 가치**: **V8 목적에 맞는 작업 환경 선택**(타입 선택), **V3 끊김 없는 세션 연속성**(대화 문맥·작업 디렉터리 이어짐)

> 이 여정은 2026-08-08 워크로드 타입이 `shell` 단수에서 `shell`·`claude-code` 복수로 확장(`../prd/claude-code-workload.md`, AC-E1~E6)되고 이를 뒷받침할 가치 **V8**이 신설되면서 만들어졌습니다.
> J5가 `shell` 타입의 사용 루프라면, J6는 **타입을 고르는 순간(S1)** 과 `claude-code` 타입의 사용 루프(S2~S5)를 함께 다룹니다.
> 두 여정은 같은 상위 보장(V1·V2·V4·V5) 위에서 나란히 서며, **타입이 달라도 플랫폼 보장은 동일하다**는 것이 V8의 핵심입니다.

## 단계

> 단계 표기: `J6-S{n}` · 각 단계 끝의 *mockup* 항목은 시각화 산출물 연결 상태입니다.

1. **J6-S1 · 작업 환경 선택** — 작업자가 세션을 만들면서 워크로드 타입을 고른다. `shell`(기본값, 직접 명령을 치는 인터랙티브 쉘)과 `claude-code`(프롬프트로 일을 맡기는 에이전트 CLI) 중 하나이며, `claude-code`를 고르면 사용할 모델도 함께 지정한다. 화면은 no-store `GET /api/v1/config`에서 concrete Secret default 또는 fallback `platform-default`와 ordered model catalog를 읽는다. catalog가 있으면 concrete default를 `<model> (platform default)` 한 항목으로 합치고 나머지 catalog 항목을 picker로 보여 주며, 비어 있으면 기존 free-text 입력을 유지한다. catalog는 편의를 위한 soft catalog이므로 API 사용자는 목록 밖의 문법상 유효한 model도 지정할 수 있다. model 미지정은 `platform-default`로 저장된다. 타입과 저장된 모델/별칭은 **세션 수명 동안 불변**이라, 바꾸려면 새 세션을 만든다. 어떤 타입을 골라도 "1 세션 = 1 전용 pod" 격리는 동일하게 적용된다. *(관련 AC: AC-E1, AC-E6, 보조 AC-A1·A2)* *(mockup: new-session.html — workload type 선택 카드 + catalog가 있는 model 선택 상태, ✅)*
2. **J6-S2 · 프롬프트 전송** — 작업자가 자연어 프롬프트를 보낸다. 플랫폼은 이를 세션 pod 안에서 `claude [--continue] [--model <선택된 모델>] --permission-mode auto -p --output-format stream-json --verbose --include-partial-messages -- "<프롬프트>"` 1회 실행으로 처리한다. `platform-default` 세션은 주 컨테이너가 시작하거나 재시작할 때 Secret의 optional `model`을 읽어 값이 비어 있지 않으면 model 플래그로 사용하고, 키가 없거나 비면 플래그를 생략해 Claude CLI 기본값에 맡긴다. 구체 세션 model은 literal 값으로 Secret 기본값보다 우선한다. **첫 성공 실행 이후**에는 `--continue`도 추가한다. `--` 뒤의 프롬프트는 옵션으로 해석되지 않는 단일 argv다. 실행은 원샷이라 응답을 내고 프로세스가 끝나며, 상주하는 CLI 프로세스는 없다. 전송은 실행 완료를 기다리지 않고 즉시 반환하고, 이전 실행이 끝나지 않았으면 다음 프롬프트는 **직렬로 큐잉**되어 한 세션에서 두 실행이 겹치지 않는다. prompt 한 건이 1 MiB를 넘으면 큐에 들어가지 않고 413, bounded queue가 포화되면 새 제출은 429로 거부된다. 화면은 write 승인 대기만 제출 상태로 표현하며, 서버의 running/queued 개수를 추정하지 않는다. *(관련 AC: AC-E2)* *(mockup: agent-workspace.html — prompt input-row + live output 연결 표시, ✅)*
3. **J6-S3 · 응답 확인** — 작업자가 실행 출력(에이전트 응답)을 기다리면 workspace 수명 SSE가 process 종료 전부터 assistant `text_delta`와 diagnostic stderr를 자동으로 붙인다. output event는 `id=nextOffset`과 `{offset,payloadBase64,nextOffset}`을 가지며 decoded byte 길이는 cursor 차이와 같다. Base64는 정확한 bytes와 overlap 검증을 위한 것이고, Claude의 서버 발급 cursor는 항상 UTF-8 code-point 경계다. 연결이 끊기면 `Last-Event-ID`가 query cursor보다 우선해 누락·중복 없이 이어진다. 요청 cursor가 보존 길이보다 큰 reset을 받으면 화면은 decoder state를 버리고 POST read(`offset=0`) 전체 이력으로 콘솔을 교체한 뒤 새 cursor에서 stream을 연다. 기존 POST read는 이 복구와 수동 catch-up·reconcile에 사용한다. SSE 연결·output/reset·keepalive 자체는 passive지만 reset의 POST read 복구는 일반 접근이라 idle을 승격하고 `lastAccess`를 갱신할 수 있다. 전송 오류 시 화면은 EventSource를 먼저 닫고 상태를 확인해 active/idle만 backoff 재연결하며, snapshot이면 자동 read를 멈추고 Restore 화면으로 이동한다. 한 invocation의 저장 출력은 marker 포함 16 MiB이고, 누적 출력은 terminal marker 포함 256 MiB다. 기존 bytes를 다시 쓰지 않으므로 이미 받은 cursor는 유효하다. 이 stream은 투영된 텍스트 출력이지 구조화된 prompt/agent 턴이나 서버 run/queue 상태가 아니다. *(관련 AC: AC-E3)* *(mockup: agent-workspace.html — 실행 중 응답 렌더 + 자동 재개 cursor 표시, ✅)*
4. **J6-S4 · 대화가 이어짐** — 연속된 프롬프트가 **하나의 이어지는 대화**로 처리된다. 각 실행은 원샷이지만 pod 파일시스템에 남은 대화 기록을 다음 실행이 이어받으므로, N번째 프롬프트는 앞선 1~N-1번째 프롬프트·응답을 문맥으로 갖는다. 작업 디렉터리도 실행 간 유지되어 이전 실행이 만든 파일을 다음 실행이 그대로 본다. 이 **대화 기록 + 작업 디렉터리**가 곧 이 타입에서 세션의 보존 대상 상태다(J5-S4의 쉘 프로세스 트리에 대응). *(관련 AC: AC-E4)* *(mockup: agent-workspace.html — conversation context 패널, ✅)*
5. **J6-S5 · 동결과 복원을 건너 이어감** — 유휴 한계에 도달하면 이 타입은 **CRIU 없이** 작업 디렉터리·대화 기록·누적 출력 버퍼를 아카이브해 외부 스토리지에 저장하고 pod를 회수한다(상주 프로세스가 없어 체크포인트할 프로세스 트리 자체가 없다). control plane은 CP-owned generation의 durable prepare/commit 기록으로 crash를 복구하고, commit 결정 뒤에는 admission을 다시 열지 않은 채 pod 회수를 끝낸다. 재접근 시 새 pod에 아카이브를 복원한 뒤 `active`로 전이하며, 복원 후의 프롬프트는 동결 이전 대화 문맥과 작업 디렉터리 위에서 이어진다. 누적 출력 버퍼·terminal marker도 아카이브에 포함되므로 **동결 전에 받은 `nextOffset` 커서가 복원 후에도 유효**하고 output-full 세션은 복원 뒤에도 새 prompt를 507로 거부한다. `platform-default` 별칭은 그대로지만 복원 pod는 그 시점의 singular Secret `model`을 새로 해석하므로 운영자가 기본값을 바꿨다면 유효 provider model은 달라질 수 있다. 작업자가 보는 경험은 J2와 동일하다 — 멈췄다는 사실을 거의 인지하지 못한 채 이어간다. *(관련 AC: AC-E5, 상위 AC-B1·B2·B3)* *(mockup: agent-workspace.html — snapshot 상태 콘솔(아카이브 기반 동결·복원), ✅)*

## 인접 여정과의 관계

- **J1(세션 생성)**: J6-S1은 J1-S1(생성 요청)에 **타입·모델 선택이라는 축이 추가된 것**이다. J1-S2(전용 pod 기동)는 타입과 무관하게 그대로 성립하며, control plane이 타입에 따라 서로 다른 워크로드로 pod를 프로비저닝한다는 점만 구체화된다.
- **J5(쉘에서 명령 실행)**: J6-S2~S4는 J5-S2~S4의 **타입별 대응**이다. write→stdin/read→stdout이 write→프롬프트/live stream+read→응답으로 바뀌고, 보존 대상 상태가 쉘 프로세스 트리(메모리)에서 대화 기록·작업 디렉터리(디스크)로 바뀐다. **append-only output과 offset cursor 보존 계약은 두 타입이 동일**하다.
- **J2(자리 비움과 재개)**: J6-S5는 J2의 동결·복원을 `claude-code` 타입 경로로 밟는다. 사용자가 보는 흐름(J2-S1~S4)은 같고 메커니즘만 CRIU→파일시스템 아카이브로 갈린다.
- **J3(멀티세션 전환)**: 타입이 섞인 세션들을 한 목록에서 오가며 전환한다. 전환 후에도 동일한 offset 커서 규약으로 출력을 이어 읽는다(AC-E3).

## 구현 상태

> 2026-08-08 기준으로 J6의 제품 경로와 결정은 코드에 반영되었다.

- **재개·모델**: 첫 성공 실행 뒤 `--continue`, 세션별 고정 HOME/workdir, immutable model과 `platform-default` 별칭으로 확정했다. 별칭은 optional Secret `model`을 우선하고 missing/empty면 CLI 기본값으로 fallback하며, 구체 세션 model은 literal로 우선한다.
- **화면·복원**: no-store config API의 optional soft catalog가 있으면 생성 picker를 구성하고 없으면 free-text 모델 입력을 유지한다. catalog 변경은 control-plane rollout 뒤 반영되고 singular 기본 모델 변경은 새/복원 pod 또는 container restart 뒤 반영되며 실행 중 컨테이너를 즉시 바꾸지 않는다. workload별 route/workspace, 실행 중 output SSE와 cursor 재접속, JSON read catch-up, archive 기반 restore 화면이 구현되었다. passive stream은 snapshot을 자동 복원하지 않는다. 외부 저장은 `CLAUDE_CODE_ARCHIVE_ENABLED` 명시적 opt-in이다.
- **출력 경계**: stream-json text delta 투영, 증분 redaction·UTF-8/byte cursor, invocation 16 MiB truncation, 누적 256 MiB terminal 상태, accepted queue drain, 507와 archive 뒤 cursor/full 상태 보존이 구현되었다. 단위 테스트는 같은 로직에 축소 limit를 주입해 결정적으로 검증한다.
- **검증 경계**: queue/cursor/live pre-exit output/reconnect/resume/archive·crash recovery는 fake runner/adapter 단위 테스트, 화면의 자동 append·오류 복구 계약은 Playwright route fixture로 검증한다. 실제 외부 Claude API를 호출하는 배포 e2e는 아직 별도 opt-in 검증으로 남는다.
