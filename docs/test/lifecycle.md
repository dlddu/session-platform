# 테스트 문서: 세션 라이프사이클 & 워크로드별 스냅샷

## 검증 대상 AC
- AC-B1: 60분 유휴 후 스냅샷 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)
- AC-B2: 스냅샷 복원 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)
- AC-B3: 스냅샷·복원 무결성 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)

## 테스트 시나리오

### 시나리오 1: 60분 유휴 도달 시 스냅샷 (경계값 포함)
- **사전 조건**: active 세션 1개 존재, 유휴 타이머 측정 기준이 설정됨
- **실행 단계**: (a) 세션을 59분간 미사용 후 상태 확인 → (b) Claude 세션에는 passive output SSE를 열고 keepalive만 전달 → (c) 추가로 1분 더 미사용(누적 60분) 후 상태 확인
- **기대 결과**: 59분 시점에는 동결되지 않음. passive stream의 output/reset/keepalive 신호 자체는 `lastAccess`를 touch하지 않아 60분 시점에는 workload별 snapshot(`shell`=CRIU, `claude-code`=filesystem archive) 생성 + 상태 `snapshot` 전이 + pod 회수. reset 뒤 read(0) reconciliation을 실제 호출하면 일반 read 활동이므로 이 시나리오의 “미사용” 조건이 깨진다. 해당 workload의 snapshot gate가 꺼져 있으면 reaper는 대상을 skip/log하고 live pod를 보존하며, 명시적 Snapshot 호출은 unavailable 오류를 반환
- **검증 AC**: AC-B1

### 시나리오 2: 스냅샷 세션 접근 시 복원
- **사전 조건**: `snapshot` 상태 세션 1개 존재
- **실행 단계**: 해당 세션에 명시적 read(또는 write/전환) 요청 — passive stream은 복원 접근에서 제외
- **기대 결과**: workload별 복원(`shell`=CRIU, `claude-code`=filesystem archive)으로 새 pod에 상태 복원, 상태 `active` 전이, 정상 응답
- **검증 AC**: AC-B2

### 시나리오 3: 복원 후 상태 무결성
- **사전 조건**: active 세션에 workload별 마커 상태(`shell`=인메모리 변수/cwd, `claude-code`=대화 기록/workspace/output cursor) 세팅
- **실행 단계**: 세션 동결(스냅샷) → 이후 접근으로 복원 → 마커 상태 조회
- **기대 결과**: 동결 직전 마커 상태가 손실 없이 동일하게 보존됨
- **검증 AC**: AC-B3
- **구체 마커 구현**: `shell`의 환경 변수 `MARKER`·작업 디렉터리·커서 왕복은 T-쉘워크로드 시나리오 4와 `TestScenario4_CRIUIntegrity`/배포 e2e가 검증한다. `claude-code`의 workspace·CLI home·resume flag·bounded scrollback·pre-snapshot cursor 왕복은 T-클로드코드 시나리오 6·8·9와 data-plane archive/control-plane transaction 단위 테스트가 검증한다. 실제 provider API를 호출하는 Claude 배포 smoke는 별도 opt-in 범위다.

### 시나리오 4: stream 단절이 snapshot을 자동 복원하지 않음
- **사전 조건**: workspace가 passive SSE를 보고 있는 Claude 세션과 동결 가능한 시계
- **실행 단계**: 60분 유휴 snapshot으로 pod 회수·SSE 단절 → SPA `onerror`와 후속 API 호출 관찰
- **기대 결과**: SPA가 native EventSource를 즉시 close하고 GET session으로 `snapshot`을 확인한 뒤 자동 stream/read 재시도를 하지 않음. Restore 화면으로 이동하며 사용자가 명시적으로 복원하기 전에는 새 pod가 생기지 않음
- **검증 AC**: AC-B1, AC-B2 (보조 AC-E3)
