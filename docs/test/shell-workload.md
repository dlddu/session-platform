# 테스트 문서: 세션 워크로드 — 인터랙티브 쉘

## 검증 대상 AC
- AC-D1: 세션 워크로드 = 인터랙티브 쉘 프로세스 (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D2: write = 쉘 입력(stdin) (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D3: read = 쉘 출력(stdout/stderr) (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D4: 보존 대상 상태 = 홈 디렉터리 파일 + 누적 출력 (복원은 새 쉘) (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D5: 유휴 판정 = 클라이언트 쉘 I/O 부재 (PRD: 세션 워크로드 — 인터랙티브 쉘)

## 테스트 시나리오

### 시나리오 1: pod 안에 인터랙티브 쉘 1개 기동
- **사전 조건**: 세션 생성 API 호출로 새 세션 1개, 대상 pod가 Ready
- **실행 단계**: (a) 세션 pod 안의 프로세스 목록 조회 → (b) control plane 프로세스 목록 조회
- **기대 결과**: pod 안에 PTY에 연결된 쉘 프로세스(기본 `/bin/bash`)가 정확히 1개 존재. control plane에는 쉘 프로세스 없음
- **검증 AC**: AC-D1

### 시나리오 2: write로 명령 실행 후 read로 출력 회수
- **사전 조건**: active 세션 1개
- **실행 단계**: `payload="echo hello\n"`로 write → 잠시 후 read(`offset=0`)
- **기대 결과**: write는 명령 완료를 기다리지 않고 즉시 반환. read 응답 `payload`에 `hello` 포함, 응답에 다음 read용 `nextOffset` 커서 발급. 발급된 `nextOffset`으로 곧바로 재-read하면 (새 출력이 없는 한) 빈 `payload`
- **검증 AC**: AC-D2, AC-D3

### 시나리오 3: read는 offset 커서 기반 델타·offset=0은 전체(비파괴적)
- **사전 조건**: active 세션 1개
- **실행 단계**: `echo A` 실행을 write → read(`offset=0`, 1회차) → `echo B` 실행을 write → 1회차가 발급한 `nextOffset`으로 read(2회차) → read(`offset=0`, 3회차)
- **기대 결과**: 1회차 read에 `A` 포함 + `nextOffset` 발급. 2회차 read(커서 이후 델타)에는 `B`만 포함되고 `A`는 미포함. 3회차 read(`offset=0`)에는 `A`와 `B`가 실행 순서대로 **모두** 포함 — 서버는 출력을 버리지 않으므로 `offset=0` 재조회는 언제나 전체 누적 출력(비파괴적)
- **검증 AC**: AC-D3

### 시나리오 4: 복원 후 홈 디렉터리 파일 보존 · 새 쉘 기동(구체 마커) + 커서 연속성
> *2026-11-01 개정* — AC-D4가 「쉘 프로세스 트리 보존(CRIU)」에서 「홈 디렉터리 파일 + 누적 출력 보존, 복원은 새 쉘」로 바뀌면서 기대 결과를 뒤집었다. 이전 판은 `echo $MARKER`가 `42`, `pwd`가 `/tmp`를 돌려주기를 기대했다. 시나리오 번호는 e2e 매핑(`./e2e.md`) 식별자라 유지한다.
- **사전 조건**: active 세션에 `echo 42 > ~/marker.txt\n`, `export MARKER=42\n`, `cd /tmp\n`를 write하여 홈 디렉터리 파일과 프로세스 상태를 함께 세팅. 동결 직전 read로 `nextOffset` 커서를 확보(`cursorBefore`). 이 세션 타입의 아카이브 게이트(`SHELL_ARCHIVE_ENABLED`)가 켜져 있음
- **실행 단계**: 세션 동결(integration은 `Service.Snapshot` 직접 호출, deploy e2e는 제품 HTTP snapshot endpoint 호출) → 이후 접근으로 아카이브 복원(AC-B2) → `cat ~/marker.txt\n`·`echo "[$MARKER]"\n`·`pwd\n`를 write → (a) `cursorBefore`로 read, (b) `offset=0`으로 read
- **기대 결과**: (a) `cursorBefore` 델타 read에 `42`(홈 디렉터리 파일이 같은 내용으로 보존)와 `[]`(환경 변수는 보존되지 않음), 그리고 홈 디렉터리 경로(`pwd` — cwd는 보존되지 않고 새 쉘이 홈에서 시작)가 포함된다 — 이는 AC-B3(무결성)의 구체 마커 검증이다. 동시에 아카이브에 담겨 새 쉘 기동 전에 preload된 scrollback에서 복원 전 커서가 여전히 유효하다는 **커서 연속성**을 입증하며, 델타에는 동결 이전 입력이 재전송되지 않는다. (b) `offset=0` read에는 동결 전 입력 에코와 복원 후 출력이 실행 순서대로 모두 포함(비파괴적 전체 이력)
- **구현**: `control-plane/test/integration_test.go`의 `TestScenario4_ShellArchiveIntegrity`는 `SHELL_ARCHIVE_ENABLED=1`과 cluster가 준비된 경우 실행하고, kind SUT에서는 `control-plane/test/e2e_d4_process_tree_test.go`의 `TestShellArchive_HomeFilesSurviveAndTheShellStartsFresh`가 제품 snapshot endpoint를 통해 동결→복원을 건넌 마커를 단언한다 — 홈 디렉터리 파일(`~/d4marker.txt`)이 복원되고, 환경 변수(`$D4MARK`)는 비어 있으며, `pwd`가 `/session/state/home`이고, 동결 전 커서의 델타에 동결 전 출력이 섞이지 않으며, `offset=0`이 전체 이력을 반환한다(테스트는 위 단계의 `marker.txt`·`MARKER` 대신 이 이름을 쓴다). 커서 연속성의 나머지 절반은 AC-B3의 `e2e_b3_restore_integrity_test.go`가 소유한다. 아카이브 형식·복원 경로의 단위 검증은 data plane의 상태 아카이브 테스트가 갖는다
- **검증 AC**: AC-D4 (AC-B3 구체화)

### 시나리오 5: 유휴 판정은 클라이언트 I/O 기준
- **사전 조건**: active 세션 1개
- **실행 단계**: (a) write 없이 read만 호출 후 `lastAccess` 확인 → (b) read/write 없이 쉘이 자체 출력만 내는 상태(예: 백그라운드 로그 루프)로 두고 `lastAccess` 확인
- **기대 결과**: (a) read만으로도 `lastAccess`가 현재 시각으로 갱신됨. (b) 클라이언트 I/O가 없으면 쉘 자체 출력과 무관하게 `lastAccess`가 갱신되지 않아 유휴 카운트가 진행됨
- **검증 AC**: AC-D5

> 비고: 바쁜 포그라운드 작업 중 유휴 도달 시의 스냅샷 트리거는 AC-B1의 트리거 정책 확정 후 별도 시나리오로 추가한다(`../doc-tracker/`의 열린 항목 참고). *2026-11-01*: 개정된 AC-D4에서 그런 동결은 실행 중인 작업을 **중단시킨다**(프로세스는 복원되지 않는다) — 시나리오를 세울 때 기대 결과는 「작업이 이어진다」가 아니라 정책이 정한 동결 여부와 그 작업 유실의 표시다.
