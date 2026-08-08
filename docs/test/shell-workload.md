# 테스트 문서: 세션 워크로드 — 인터랙티브 쉘

## 검증 대상 AC
- AC-D1: 세션 워크로드 = 인터랙티브 쉘 프로세스 (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D2: write = 쉘 입력(stdin) (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D3: read = 쉘 출력(stdout/stderr) (PRD: 세션 워크로드 — 인터랙티브 쉘)
- AC-D4: 쉘 프로세스 트리 = 보존 대상 상태 (PRD: 세션 워크로드 — 인터랙티브 쉘)
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

### 시나리오 4: 복원 후 쉘 상태 무결성(구체 마커) + 커서 연속성
- **사전 조건**: active 세션에 `export MARKER=42\n`, `cd /tmp\n`를 write하여 쉘 상태 세팅. 동결 직전 read로 `nextOffset` 커서를 확보(`cursorBefore`)
- **실행 단계**: 세션 동결(스냅샷, in-process `Service.Snapshot` 직접 호출 — HTTP 스냅샷 엔드포인트 미추가) → 이후 접근으로 CRIU 복원(AC-B2) → `echo $MARKER\n`·`pwd\n`를 write → (a) `cursorBefore`로 read, (b) `offset=0`으로 read
- **기대 결과**: (a) `cursorBefore` 델타 read에 `42`와 `/tmp`가 포함(동결 직전 환경 변수·작업 디렉터리 보존) — 이는 AC-B3(무결성)의 구체 마커 검증. 동시에 archive에 별도 직렬화·preload된 scrollback에서 복원 전 커서가 여전히 유효하다는 **커서 연속성**을 입증하며, 델타에는 동결 이전 입력이 재전송되지 않는다. (b) `offset=0` read에는 동결 전 입력 에코와 복원 후 출력이 실행 순서대로 모두 포함(비파괴적 전체 이력)
- **구현**: `control-plane/test/integration_test.go`의 `TestScenario4_CRIUIntegrity`는 env/cluster가 준비된 경우 실행하고, `control-plane/test/e2e_deferred_test.go`의 `TestDeferred_CRIUIntegrity`는 CRIU·MinIO·test-only snapshot trigger를 켠 kind overlay에서 실제 dump→pod 회수→새 pod restore를 단언한다(`../criu-verification.md`)
- **검증 AC**: AC-D4 (AC-B3 구체화)

### 시나리오 5: 유휴 판정은 클라이언트 I/O 기준
- **사전 조건**: active 세션 1개
- **실행 단계**: (a) write 없이 read만 호출 후 `lastAccess` 확인 → (b) read/write 없이 쉘이 자체 출력만 내는 상태(예: 백그라운드 로그 루프)로 두고 `lastAccess` 확인
- **기대 결과**: (a) read만으로도 `lastAccess`가 현재 시각으로 갱신됨. (b) 클라이언트 I/O가 없으면 쉘 자체 출력과 무관하게 `lastAccess`가 갱신되지 않아 유휴 카운트가 진행됨
- **검증 AC**: AC-D5

> 비고: 바쁜 포그라운드 작업 중 유휴 도달 시의 스냅샷 트리거는 AC-B1의 트리거 정책 확정 후 별도 시나리오로 추가한다(`../doc-tracker.md`의 열린 항목 참고).
