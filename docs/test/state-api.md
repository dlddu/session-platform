# 테스트 문서: 세션 상태 일관성 & Read/Write API

## 검증 대상 AC
- AC-C1: ConfigMap(resourceVersion CAS) + Lease 기반 atomic 상태 전이 (PRD: 세션 상태 일관성 & Read/Write API)
- AC-C2: Read API 상태별 분기 (PRD: 세션 상태 일관성 & Read/Write API)
- AC-C3: Write API 상태별 분기 (PRD: 세션 상태 일관성 & Read/Write API)
- AC-C4: 세션 간 자유 전환 (PRD: 세션 상태 일관성 & Read/Write API)

## 테스트 시나리오

### 시나리오 1: 동시 요청 시 atomic 전이
- **사전 조건**: `snapshot` 또는 `idle` 세션 1개 존재
- **실행 단계**: 동일 세션에 복원·스냅샷·전환 요청을 동시 다발(N개)로 발생
- **기대 결과**: 단일 전이만 성공, 최종 상태가 유효한 단일 상태로 수렴, 중복 pod 기동·이중 스냅샷 없음
- **검증 AC**: AC-C1

### 시나리오 2: Read 상태별 분기
- **사전 조건**: `active`/`idle`/`snapshot` 상태 세션을 각각 준비
- **실행 단계**: 동일한 read 요청을 세 세션에 각각 호출
- **기대 결과**: active=직접 읽기 즉시 응답, idle=`idle→active` 승격 후 읽기, snapshot=CRIU 복원 후 읽기 — 각 경로로 처리되고 호출 후 세션 상태가 모두 `active`, 올바른 결과 반환
- **검증 AC**: AC-C2

### 시나리오 3: Write 상태별 분기
- **사전 조건**: `active`/`idle`/`snapshot` 상태 세션을 각각 준비
- **실행 단계**: 각 세션에 write 요청
- **기대 결과**: active=직접 write, idle=승격 후 write, snapshot=복원 후 write(거부 아님) — 대상이 `active`로 처리되어 데이터가 일관되게 반영, 상태 전이는 atomic
- **검증 AC**: AC-C3

### 시나리오 4: 세션 간 자유 전환
- **사전 조건**: 상태가 서로 다른 세션 여러 개 보유(active + snapshot 혼재)
- **실행 단계**: 세션들 사이를 순차/반복 전환
- **기대 결과**: 전환마다 대상 세션이 올바른 상태로 활성화(snapshot은 복원), 이전 세션 상태 보존, 격리 유지
- **검증 AC**: AC-C4
