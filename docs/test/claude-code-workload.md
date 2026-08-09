# 테스트 문서: 세션 워크로드 — 클로드 코드 CLI

## 검증 대상 AC
- AC-E1: 세션 워크로드 타입 선택 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E2: write = 프롬프트 1회 실행 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E3: read/stream = 실행 중 출력, offset 커서 기반 델타 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E4: 대화 연속성 = pod에 지속되는 대화 기록 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E5: CRIU 비대상 — 파일시스템 아카이브 기반 동결·복원 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E6: 자격 증명은 sidecar Secret, 플랫폼 모델 설정은 optional Secret (PRD: 세션 워크로드 — 클로드 코드 CLI)
## 자동화 상태 (2026-08-09)

- data plane fake runner 단위 테스트가 exact argv(`-p --output-format stream-json --verbose --include-partial-messages -- <prompt>`), concrete `CLAUDE_CODE_MODEL`의 단일 `--model` argv와 empty/`platform-default` fallback, 비블로킹 write, process 종료 전 `text_delta` append, 1 MiB prompt 제한, bounded serial queue, 첫 성공 뒤 `--continue`, 고정 HOME/workdir, managed tool settings을 검증한다. stream 테스트는 SSE event id와 `{offset,payloadBase64,nextOffset}`의 decoded byte invariant, 직접 agent와 public proxy 모두의 `Last-Event-ID` 우선 재접속, 서버 발급 UTF-8 경계, stale cursor reset, read reconcile, stdout/stderr write 사이에 걸친 incremental redaction을 검증한다. 축소한 deterministic limit로 live invocation truncation marker와 cumulative terminal marker·기존 cursor 불변을 같은 production 경계 로직에서 검증한다. credential-proxy 테스트는 loopback bind, **HTTPS-only** 고정 upstream, 인증·라우팅 헤더 제거/토큰 주입, 1xx·최종 응답 redaction, upstream EOF 전 safe SSE chunk 전달, response-read 경계 split-token redaction, 축소 limit를 주입한 raw-upstream 64 MiB SSE cap 로직과 주 컨테이너의 실제 credential 거부를 검증한다.
- archive 단위 테스트가 accepted queue drain/admission barrier, CP-supplied generation 기반 abort와 stale abort 거부, workspace·CLI home·resume flag·bounded scrollback 왕복, pre-snapshot cursor, output-full 복원 후 507, traversal/symlink 방어를 검증한다.
- control plane 단위/통합 테스트가 workload/model 계약(각 필드 생략만 default, explicit workload `shell` 허용, explicit empty/null/비문자열·앞뒤 공백·허용값 밖 입력은 400), existing-session route의 immutable-field 변경 시도 400, 필수 base URL/token SecretKeyRef의 sidecar 한정, `platform-default` 주 컨테이너의 optional `model` SecretKeyRef와 구체 model의 literal 우선, 주 컨테이너의 localhost placeholder, sidecar 보안 컨텍스트·service-account token 비활성화, Claude pod 비-CRIU 분기와 disabled-strategy 안전성을 검증한다. `CLAUDE_CODE_MODELS` parser 테스트는 JSON array·model pattern·빈/공백 항목·중복·reserved alias를 엄격히 검증하고, config API 테스트는 stable alias·ordered/non-null catalog·`no-store`와 catalog 밖 model의 API 허용을 검증한다. 단위 테스트는 durable prepare/commit 순서, crash-boundary recovery, owner fence, Lease Renew timeout/loss, latest-only Touch와 aggregate CAS도 검증한다.
- Playwright `j6-agent-prompt-loop.spec.ts`는 API route fixture로 non-empty config catalog picker, missing/empty catalog의 기존 free-text 입력, picker/model 요청, workload별 route/card, newline 없는 prompt, UTF-8 경계 SSE chunk 자동 append, cursor 재연결·read catch-up, stale cursor reset의 decoder 폐기·전체 replay, snapshot 오류 뒤 Restore 이동을 검증한다. UI는 output 연결 상태만 표시하고 서버 내부 running/queued 상태를 주장하지 않는다.
- 실제 공급자 API를 호출하는 배포 full-stack 테스트는 비용·자격 증명·응답 비결정성 때문에 자동화 범위에 포함하지 않는다. 별도 opt-in smoke에서는 설치된 Claude Code가 위 stream-json argv로 process 종료 전에 첫 `text_delta`를 실제 방출하는지 확인한다.


## 테스트 시나리오

### 시나리오 1: 워크로드 타입 선택과 불변성
- **사전 조건**: 없음
- **실행 단계**: (a) `workloadType=claude-code`로 세션 생성 → (b) `workloadType` 미지정 및 explicit `shell`로 각각 생성 → (c) 허용값 외 타입(`foo`) 및 explicit `""`/`null`/비문자열로 생성 시도 → (d) (a)의 read/write/switch route에 workload/model 변경 필드를 보냄
- **기대 결과**: (a) 클로드 코드 CLI 실행이 가능한 pod가 기동되고 세션 조회 응답의 타입이 `claude-code`. (b) 두 요청 모두 기존과 동일한 `shell` 세션(PTY 쉘 1개, AC-D1). (c) 모두 pod 생성 전에 400 거부. (d) 모든 route가 400으로 거부하고 원래 타입·모델 유지
- **검증 AC**: AC-E1

### 시나리오 2: write 1회 = claude 1회 실행, 비블로킹
- **사전 조건**: active 상태의 `claude-code` 세션 1개
- **실행 단계**: output SSE를 연 뒤 `payload="1+1은?"`로 write → fake runner가 첫 text delta를 쓰고 release 전 대기 → SSE/read 확인 → runner release·process 종료
- **기대 결과**: write는 실행 완료를 기다리지 않고 즉시 반환. pod 안에서 `claude [--continue] [--model <선택된 모델>] -p --output-format stream-json --verbose --include-partial-messages -- <prompt>` exact argv 프로세스가 1회 기동 후 종료(상주 프로세스 없음). runner release 전 첫 delta가 SSE와 같은 cursor의 read에 보인다. concrete `CLAUDE_CODE_MODEL`만 model 플래그를 만들고 empty/`platform-default`는 이를 생략하며, `--`는 대시로 시작하는 prompt의 옵션 해석을 차단한다. 첫 성공 실행 이후 invocation만 `--continue`를 사용한다.
- **검증 AC**: AC-E2, AC-E3

### 시나리오 3: 연속 write는 직렬 실행
- **사전 조건**: active 상태의 `claude-code` 세션 1개
- **실행 단계**: 첫 write의 실행이 끝나기 전에 두 번째 write 호출 → 두 실행의 시작·종료 시각과 read 출력 순서 확인
- **기대 결과**: 두 `claude` 실행 구간이 겹치지 않음(직렬). read(`offset=0`) 출력이 write를 보낸 순서대로 누적
- **검증 AC**: AC-E2

### 시나리오 4: live stream·재접속·read reconcile
- **사전 조건**: active 상태의 `claude-code` 세션 1개
- **실행 단계**: 프롬프트 A를 write하며 UTF-8 경계의 SSE output 두 chunk 수신 → 첫 event 뒤 연결 중단 → query에는 다른 offset을 두고 `Last-Event-ID`로 재연결 → 프롬프트 B 출력 수신 → 현재 길이보다 큰 cursor로 연결해 reset 수신 → POST read(`offset=0`) → 마지막 cursor로 POST read
- **기대 결과**: output마다 `id=nextOffset`, decoded `payloadBase64` 길이=`nextOffset-offset`이고 cursor는 code-point 경계다. Last-Event-ID가 query보다 우선하고 A·B bytes가 누락·중복 없이 한 번씩 표시된다. reset은 payload 없이 현재 길이를 id/`nextOffset`으로 발급하며 그 신호 자체는 `lastAccess`를 바꾸지 않는다. SPA는 decoder를 폐기한 뒤 일반 Read API read(0)의 A·B 전체 이력으로 콘솔을 교체하고 그 cursor에서 재연결한다. 이 read는 idle 승격·`lastAccess` 갱신 의미를 유지한다. 마지막 cursor read는 빈 delta이며 raw stream-json final/result로 text delta가 중복되지 않는다.
- **검증 AC**: AC-E3

### 시나리오 5: 대화·작업 디렉터리 연속성과 세션 간 격리
- **사전 조건**: active 상태의 `claude-code` 세션 두 개(S1, S2)
- **실행 단계**: (a) S1에 `"내 이름은 다르마야"` write → `"내 이름이 뭐야?"` write → read → (b) S2에 `"내 이름이 뭐야?"`만 write → read → (c) S1에 `"touch marker.txt"` 취지의 프롬프트 write → `"작업 디렉터리에 어떤 파일이 있어?"` write → read
- **기대 결과**: (a) 두 번째 응답에 "다르마" 포함(대화 이어짐). (b) S2 응답은 이름을 알지 못함(세션 간 대화 격리, AC-A2). (c) 두 번째 응답에 `marker.txt` 포함(실행 간 작업 디렉터리 유지)
- **검증 AC**: AC-E4
- **비고**: 첫 성공 실행 뒤 `--continue`와 세션별 고정 HOME/workdir를 사용하도록 확정했으며, fake runner와 archive 왕복 테스트로 해당 wiring을 자동 검증한다. 실제 응답 의미(“다르마” 포함)는 provider opt-in smoke 대상이다.

### 시나리오 6: CRIU 없이 동결·회수, 아카이브 복원 후 연속성
- **사전 조건**: `claude-code` 세션 1개에 대화 몇 턴 + 파일 생성 프롬프트 수행. 동결 직전 read로 커서 확보(`cursorBefore`)
- **실행 단계**: 유휴 한계 도달(또는 스냅샷 직접 호출)로 동결 → CRIU 호출 여부·아카이브 생성·pod 상태 확인 → 재접근으로 복원 → 동결 전 대화를 참조하는 프롬프트 write, 작업 디렉터리 조회 프롬프트 write → (a) `cursorBefore`로 read, (b) `offset=0`으로 read
- **기대 결과**: 동결 시 **CRIU dump가 호출되지 않고** 작업 디렉터리·대화 기록·출력 버퍼 아카이브가 생성되며 pod가 회수되어 자원이 회복됨(AC-A3). control plane은 `preparing`을 agent 호출 전에, archive ref를 포함한 `committing`을 pod Stop 전에 durable하게 남긴다. 복원 후 세션이 `active`가 되고, 동결 전 대화 참조 프롬프트가 정상 응답하며 동결 전 생성 파일이 남아 있음. (a) `cursorBefore` 델타 read가 유효하게 동작(복원 전 커서가 깨지지 않음). (b) `offset=0`에 동결 전·후 출력이 순서대로 모두 포함
- **검증 AC**: AC-E5 (AC-B1/B2/B3의 `claude-code` 타입 경로)

### 시나리오 7: 자격 증명 주입, 기본 모델, UI catalog
- **사전 조건**: 플랫폼 Secret에 필수 `base-url`·`auth-token`과 선택적 `model`·`models` 변형을 준비
- **실행 단계**: (a) 세션 생성 요청 본문에 base URL·토큰을 넣어 생성 시도 → (b) 세션 pod의 주 컨테이너·credential sidecar 환경과 보안 컨텍스트 확인 → (c) proxy에 위조 인증·라우팅 헤더를 넣어 테스트 upstream에서 관찰 → (d) upstream이 첫 safe SSE chunk를 flush하고 EOF 전 block하며 token literal을 response read 경계에 나누는 응답과 raw 64 MiB 초과 SSE 응답 전달 → (e) token literal을 stdout/stderr 여러 runner write에 나누어 출력하며 매 SSE event·read·control plane 로그 검색 → (f) non-empty/missing/empty `model` Secret으로 각각 `model` 미지정 세션을 프로비저닝하고 구체 model 세션도 생성 → (g) model을 explicit empty/null/비문자열·앞뒤 공백·invalid pattern으로 생성 시도 → (h) `CLAUDE_CODE_MODELS`를 valid ordered array, unset/empty/`[]`, malformed JSON, object, null, 빈/공백/invalid/중복/`platform-default` 항목으로 각각 시작 → (i) config API와 생성 UI를 확인하고 catalog 밖 valid model로 API 생성 → (j) singular `model`을 변경한 뒤 실행 중 컨테이너, container restart, 새 pod, 복원 pod를 비교하고 plural `models`는 rollout 전후를 비교 → (k) `CLAUDE_CODE_CREDENTIALS_SECRET`을 custom 이름으로 바꾼 Deployment의 두 Secret ref 확인
- **기대 결과**: (a) 요청의 base URL·토큰은 unknown field로 400 거부됨. (b) 필수 base URL/token은 sidecar에만 있고 주 컨테이너에는 localhost URL·placeholder만 있으며 service-account token이 자동 마운트되지 않음. `platform-default` 주 컨테이너에는 optional `model` SecretKeyRef만 있고 구체 model 세션에는 같은 환경 변수가 literal이며, control plane에는 Deployment가 `models` 키만 투영함. (c) upstream은 **HTTPS origin으로 고정**되고 평문 HTTP 설정은 시작 시 거부되며 실제 Authorization 하나만 전달됨. (d) client가 upstream release/EOF 전에 첫 safe SSE chunk를 받고 split token은 어느 순간에도 노출되지 않으며 proxy는 전체 SSE body를 메모리에 모으지 않고 raw upstream 64 MiB 상한을 집행함. (e) runner write 경계 중간을 포함해 read/SSE/log 어디에도 토큰 문자열이 일시적으로도 노출되지 않음. (f) non-empty Secret model은 `--model <Secret 값>`, missing/empty는 `--model` 생략이며 구체 세션 model은 literal로 Secret보다 우선함. (g) 모두 pod 생성 전 400. (h) valid array는 입력 순서대로 수용되고 unset/empty/`[]`는 non-null 빈 catalog이며 나머지는 startup 실패. (i) `GET /api/v1/config`가 정확히 stable alias와 catalog를 `Cache-Control: no-store`로 반환하고 credential/singular model 값은 노출하지 않음. non-empty catalog면 picker, 빈 catalog면 free-text 입력이며 catalog 밖 valid model도 API에서 201. (j) singular 변경은 실행 중 컨테이너에 즉시 반영되지 않고 container restart 또는 새/복원 `platform-default` pod의 다음 시작부터 반영되며, concrete model은 literal을 유지함. plural 변경은 control-plane rollout 전에는 보이지 않고 rollout 뒤 config/UI에 반영됨. (k) `CLAUDE_CODE_CREDENTIALS_SECRET`과 `CLAUDE_CODE_MODELS.valueFrom.secretKeyRef.name`이 같은 custom literal을 가리킴
- **검증 AC**: AC-E6

### 시나리오 8: prompt·invocation·누적 출력 상한, queue drain, archive 연속성
- **사전 조건**: fresh `claude-code` 세션. 테스트에서는 production과 같은 로직에 축소한 deterministic limit를 주입
- **실행 단계**: (a) 1 MiB 초과 prompt write → (b) 한 invocation이 저장-output limit를 넘는 text delta/stderr 생성하며 SSE 관찰 → (c) 기존 `nextOffset`을 발급받은 뒤 cumulative limit를 넘기며 그 직전에 후속 prompt도 미리 수락 → (d) 새 prompt write → (e) output-full 세션 checkpoint/restore → 복원 전 cursor로 read하고 다시 write
- **기대 결과**: (a) prompt는 큐에 들어가거나 실행되지 않고 control-plane API까지 413. (b) 이미 SSE로 노출한 bytes를 고치지 않고 invocation truncation marker가 live append되며 전체가 limit 안에 남음(운영값 16 MiB, raw JSONL framing은 quota 제외). (c) 기존 bytes는 바뀌지 않고 cumulative terminal marker가 전체 limit 안에 live append되며(운영값 256 MiB), 미리 수락된 prompt는 모두 직렬 실행되지만 marker 이후 출력은 버려짐. (d)·(e) 신규 write는 control-plane API까지 507. checkpoint/restore 뒤에도 full buffer·terminal marker·`nextOffset`·resume state가 같고 복원 전 cursor가 정확한 delta를 가리킴
- **검증 AC**: AC-E2, AC-E3, AC-E5

### 시나리오 9: passive stream 상태·오류 복구
- **사전 조건**: 같은 output cursor를 가진 active, pod 보유 idle, snapshot 세션과 제어 가능한 SSE 단절 준비
- **실행 단계**: 각 상태에서 stream 연결 → active stream에 keepalive만 전달하며 시간 진행 → 연결 오류 발생 시 SPA 동작 관찰 → snapshot 상태에서 자동 read 호출 여부 확인
- **기대 결과**: active/idle은 기존 pod output을 stream하되 상태 승격·restore·`lastAccess` touch가 없고, keepalive도 cursor/유휴 시간을 바꾸지 않음. snapshot stream은 invalid-state이며 pod를 만들지 않음. SPA는 EventSource를 즉시 닫고 GET session 후 active/idle만 마지막 cursor로 backoff 재연결하며 snapshot은 read fallback 없이 Restore 화면으로 이동
- **검증 AC**: AC-E3, AC-E5 (보조 AC-B1/B2)
