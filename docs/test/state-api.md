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
- **기대 결과**: Lease/CAS 단일 승자만 전이하고 최종 상태가 유효한 단일 상태로 수렴하며, 중복 pod 기동·이중 스냅샷이 없음. 긴 Snapshot/Restore 동안 Lease heartbeat가 유지되고 Renew 실패/timeout은 side effect context를 취소. `claude-code` archive 분기에서는 private transaction·최신 `lastAccess`가 aggregate CAS/partial Touch로 함께 보존되고 stale owner는 새 generation을 commit/clear하지 못함. shell CRIU에는 durable post-dump owner/phase transaction을 주장하지 않음
- **검증 AC**: AC-C1

### 시나리오 2: Read 상태별 분기
- **사전 조건**: `active`/`idle`/`snapshot` 상태 세션을 각각 준비
- **실행 단계**: 동일한 read 요청을 세 세션에 각각 호출
- **기대 결과**: active=직접 읽기 즉시 응답, idle=`idle→active` 승격 후 읽기, snapshot=workload별 복원(`shell`=CRIU, `claude-code`=archive) 후 읽기 — 각 경로로 처리되고 호출 후 세션 상태가 모두 `active`, 올바른 결과 반환
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

### 시나리오 5: optional body와 strict JSON validation
- **사전 조건**: active 세션 1개
- **실행 단계**: (a) body 없이 read/write/switch → (b) 각 route에 explicit `null`(top-level 또는 선언된
  field 값), unknown field, malformed JSON, 두 JSON 값을 이은 trailing input 전송 → (c) workloadType/model
  field로 immutable metadata 변경 시도 → (d) 8 MiB 초과 wire body 전송 → (e) snapshot `claude-code`
  세션에 decoded 1 MiB 초과 prompt write
- **기대 결과**: (a) read는 `offset=0`, write는 빈 payload, switch는 field 없음으로 정상 처리. (b)·(c)는
  모두 400이고 agent side effect와 workload/model 변경 없음. (d)·(e)는 413이고, (e)는 pod 복원이나
  agent write 전에 거부됨. 서버 body read는 30초로 제한됨
- **검증 AC**: AC-C2, AC-C3, AC-C4 (wire validation)
