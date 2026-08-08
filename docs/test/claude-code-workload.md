# 테스트 문서: 세션 워크로드 — 클로드 코드 CLI

## 검증 대상 AC
- AC-E1: 세션 워크로드 타입 선택 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E2: write = 프롬프트 1회 실행 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E3: read = 실행 출력, offset 커서 기반 델타 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E4: 대화 연속성 = pod에 지속되는 대화 기록 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E5: CRIU 비대상 — 파일시스템 아카이브 기반 동결·복원 (PRD: 세션 워크로드 — 클로드 코드 CLI)
- AC-E6: 자격 증명은 Secret 주입, 모델은 세션별 설정 (PRD: 세션 워크로드 — 클로드 코드 CLI)
## 자동화 상태 (2026-08-08)

- data plane fake runner 단위 테스트가 exact argv(`-p -- <prompt>`), 비블로킹 write, 1 MiB prompt 제한, bounded serial queue, 첫 성공 뒤 `--continue`, 고정 HOME/workdir, managed tool settings, stdout+stderr cursor를 검증한다. 축소한 deterministic limit로 invocation truncation marker와 cumulative terminal marker·기존 cursor 불변을 같은 production 경계 로직에서 검증한다. credential-proxy 테스트는 loopback bind, **HTTPS-only** 고정 upstream, 인증·라우팅 헤더 제거/토큰 주입, 1xx·최종 응답 redaction과 주 컨테이너의 실제 credential 거부를 검증한다.
- archive 단위 테스트가 accepted queue drain/admission barrier, CP-supplied generation 기반 abort와 stale abort 거부, workspace·CLI home·resume flag·bounded scrollback 왕복, pre-snapshot cursor, output-full 복원 후 507, traversal/symlink 방어를 검증한다.
- control plane 단위/통합 테스트가 workload/model 계약(각 필드 생략만 default, explicit workload `shell` 허용, explicit empty/null/비문자열·앞뒤 공백·허용값 밖 입력은 400), existing-session route의 immutable-field 변경 시도 400, SecretKeyRef가 credential sidecar에만 존재함, 주 컨테이너의 localhost placeholder, sidecar 보안 컨텍스트·service-account token 비활성화, Claude pod 비-CRIU 분기와 disabled-strategy 안전성을 검증한다. 단위 테스트는 durable prepare/commit 순서, crash-boundary recovery, owner fence, Lease Renew timeout/loss, latest-only Touch와 aggregate CAS도 검증한다.
- Playwright `j6-agent-prompt-loop.spec.ts`는 API route fixture로 picker/model 요청, workload별 route/card, newline 없는 prompt, cursor refresh, archive restore 화면과 **클라이언트가 제출 후 출력을 확인 중인 횟수**(`submissions · checking output`)를 검증한다. 이 표시는 서버 내부 running/queued 상태를 주장하지 않는다.
- 실제 공급자 API를 호출하는 배포 full-stack 테스트는 비용·자격 증명·응답 비결정성 때문에 자동화 범위에 포함하지 않는다. 필요하면 별도 opt-in smoke로 운영한다.


## 테스트 시나리오

### 시나리오 1: 워크로드 타입 선택과 불변성
- **사전 조건**: 없음
- **실행 단계**: (a) `workloadType=claude-code`로 세션 생성 → (b) `workloadType` 미지정 및 explicit `shell`로 각각 생성 → (c) 허용값 외 타입(`foo`) 및 explicit `""`/`null`/비문자열로 생성 시도 → (d) (a)의 read/write/switch route에 workload/model 변경 필드를 보냄
- **기대 결과**: (a) 클로드 코드 CLI 실행이 가능한 pod가 기동되고 세션 조회 응답의 타입이 `claude-code`. (b) 두 요청 모두 기존과 동일한 `shell` 세션(PTY 쉘 1개, AC-D1). (c) 모두 pod 생성 전에 400 거부. (d) 모든 route가 400으로 거부하고 원래 타입·모델 유지
- **검증 AC**: AC-E1

### 시나리오 2: write 1회 = claude 1회 실행, 비블로킹
- **사전 조건**: active 상태의 `claude-code` 세션 1개
- **실행 단계**: `payload="1+1은?"`로 write → pod 프로세스 관찰 → 잠시 후 read(`offset=0`)
- **기대 결과**: write는 실행 완료를 기다리지 않고 즉시 반환. pod 안에서 `claude [--model <명시 모델>] -p -- <prompt>` 형태 프로세스가 1회 기동 후 종료(상주 프로세스 없음). `platform-default`는 model 플래그를 생략하고, `--`는 대시로 시작하는 prompt의 옵션 해석을 차단한다. 첫 성공 실행 이후 invocation만 `--continue`를 사용한다. read 응답 `payload`에 해당 프롬프트의 응답 포함
- **검증 AC**: AC-E2, AC-E3

### 시나리오 3: 연속 write는 직렬 실행
- **사전 조건**: active 상태의 `claude-code` 세션 1개
- **실행 단계**: 첫 write의 실행이 끝나기 전에 두 번째 write 호출 → 두 실행의 시작·종료 시각과 read 출력 순서 확인
- **기대 결과**: 두 `claude` 실행 구간이 겹치지 않음(직렬). read(`offset=0`) 출력이 write를 보낸 순서대로 누적
- **검증 AC**: AC-E2

### 시나리오 4: read 커서 델타·offset=0 전체(비파괴적)
- **사전 조건**: active 상태의 `claude-code` 세션 1개
- **실행 단계**: 프롬프트 A를 write → read(`offset=0`, 1회차) → 프롬프트 B를 write → 1회차 `nextOffset`으로 read(2회차) → read(`offset=0`, 3회차)
- **기대 결과**: 1회차에 A의 응답 + `nextOffset` 발급. 2회차에는 B의 응답만 포함되고 A는 미포함. 3회차에는 A·B 응답이 실행 순서대로 모두 포함(비파괴적 전체 이력)
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

### 시나리오 7: 자격 증명 주입과 모델 설정
- **사전 조건**: 플랫폼 Secret에 base URL·토큰 설정
- **실행 단계**: (a) 세션 생성 요청 본문에 base URL·토큰을 넣어 생성 시도 → (b) 세션 pod의 주 컨테이너·credential sidecar 환경과 보안 컨텍스트 확인 → (c) proxy에 위조 인증·라우팅 헤더를 넣어 테스트 upstream에서 관찰 → (d) 세션 조회 응답·read 출력·control plane 로그에서 토큰 문자열 검색 → (e) `model`을 각각 다르게 지정한 두 세션과 `model` 미지정 세션을 만들어 실행 → (f) model을 explicit empty/null/비문자열·앞뒤 공백·invalid pattern으로 생성 시도
- **기대 결과**: (a) 요청의 base URL·토큰은 unknown field로 400 거부됨. (b) Secret 값은 sidecar에만 있고 주 컨테이너에는 localhost URL·placeholder만 있으며 service-account token이 자동 마운트되지 않음. (c) upstream은 **HTTPS origin으로 고정**되고 평문 HTTP 설정은 시작 시 거부되며 실제 Authorization 하나만 전달됨. (d) 어디에도 토큰 문자열이 노출되지 않음. (e) 각 세션이 지정한 model로 실행되고, 미지정 세션은 플랫폼 기본 모델로 실행. (f) 모두 pod 생성 전 400
- **검증 AC**: AC-E6

### 시나리오 8: prompt·invocation·누적 출력 상한, queue drain, archive 연속성
- **사전 조건**: fresh `claude-code` 세션. 테스트에서는 production과 같은 로직에 축소한 deterministic limit를 주입
- **실행 단계**: (a) 1 MiB 초과 prompt write → (b) 한 invocation이 per-run limit를 넘는 stdout/stderr 생성 → (c) 기존 `nextOffset`을 발급받은 뒤 cumulative limit를 넘기며 그 직전에 후속 prompt도 미리 수락 → (d) 새 prompt write → (e) output-full 세션 checkpoint/restore → 복원 전 cursor로 read하고 다시 write
- **기대 결과**: (a) prompt는 큐에 들어가거나 실행되지 않고 control-plane API까지 413. (b) tail 대신 invocation truncation marker가 limit 안에 남음(운영값 16 MiB). (c) 기존 bytes는 바뀌지 않고 cumulative terminal marker가 전체 limit 안에 append되며(운영값 256 MiB), 미리 수락된 prompt는 모두 직렬 실행되지만 marker 이후 출력은 버려짐. (d)·(e) 신규 write는 control-plane API까지 507. checkpoint/restore 뒤에도 full buffer·terminal marker·`nextOffset`·resume state가 같고 복원 전 cursor가 정확한 delta를 가리킴
- **검증 AC**: AC-E2, AC-E3, AC-E5
