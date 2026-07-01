# PRD: 세션 상태 일관성 & Read/Write API

> 대상 요구사항: ④ ConfigMap(resourceVersion CAS) + Lease 기반 atomic operation, ⑥ 세션 단위 read/write API (상태별 분기), ⑦ 세션 간 자유 전환

## 달성 가치
- **V3 끊김 없는 세션 연속성** — 상태와 무관하게 read/write가 동작
- **V4 자유로운 멀티세션 전환** — 세션 간 이동 보장
- **V5 일관된 세션 상태** — ConfigMap(resourceVersion CAS) + Lease 기반 atomic operation으로 상태 일관성 확보

## Acceptance Criteria

### AC-C1: ConfigMap(resourceVersion CAS) + Lease 기반 atomic 상태 전이
- **설명**: 세션 상태 전이(active ↔ idle ↔ snapshot)와 세션 점유는 Kubernetes ConfigMap의 resourceVersion 낙관적 동시성(compare-and-swap)과 `coordination.k8s.io` Lease를 통한 atomic operation으로 처리된다. 동일 세션에 대한 동시 요청(예: 복원과 스냅샷이 동시 발생)에서도 단일 전이만 성공하며 상태가 깨지지 않는다. 상태는 control plane 다중 replica가 공유하므로 어느 replica가 처리하든 동일하게 보인다.
- **달성 가치**: V5
- **검증 방법**: 동일 세션에 복원/스냅샷/전환 요청을 동시 다발로 발생시켜, 최종 상태가 항상 유효한 단일 상태로 수렴하고 중복 pod 기동·이중 스냅샷이 발생하지 않음을 확인한다.

### AC-C2: Read API 상태별 분기
- **설명**: 세션 단위 Read API는 대상 세션을 먼저 `active`로 만든 뒤 그 pod에서 읽는다(통일 규칙: 비-active 접근은 "active 보장 후 처리"). 상태별로 active 보장 경로만 다르다.
  - `active`: pod에서 직접 읽어 즉시 응답
  - `idle`: `idle→active` atomic 승격(AC-C1) 후 pod에서 읽기 (idle은 pod를 아직 보유)
  - `snapshot`: CRIU 복원으로 `active` 전이(AC-B2) 후 읽기
  - 이는 switch(AC-C4)·snapshot 접근(AC-B2)과 동일한 "접근 시 active화" 원칙을 read에 적용한 것이다.
- **달성 가치**: V3, V4
- **구체화**: read가 반환하는 것 = 세션 시작 이후 누적된 쉘 stdout/stderr **전체** 출력(비파괴적) → AC-D3 (`shell-workload.md`)
- **검증 방법**: active/idle/snapshot 세션에 각각 read를 호출하여, 각 경로(active 직접 / idle 승격 후 / snapshot 복원 후)로 처리되고 호출 후 최종 상태가 `active`이며 올바른 결과를 반환함을 확인한다.

### AC-C3: Write API 상태별 분기
- **설명**: 세션 단위 Write API도 read와 같은 통일 규칙을 따른다. 대상 세션을 먼저 `active`로 만든 뒤 write를 적용한다.
  - `active`: pod에 직접 write
  - `idle`: `idle→active` atomic 승격(AC-C1) 후 write
  - `snapshot`: CRIU 복원으로 `active` 전이(AC-B2) 후 write — **snapshot write는 거부하지 않고 복원 후 적용한다** (AC-B2의 "접근=복원"과 일치)
- **달성 가치**: V3, V5
- **구체화**: write가 반영하는 것 = 대상 세션 쉘의 stdin 입력(명령/키 입력) → AC-D2 (`shell-workload.md`)
- **검증 방법**: active/idle/snapshot 세션에 각각 write 요청 시 대상이 `active`로 처리되어 데이터가 일관되게 반영되고 상태 전이가 atomic하게 일어남을 확인한다.

### AC-C4: 세션 간 자유 전환
- **설명**: 사용자는 보유한 여러 세션 사이를 자유롭게 전환할 수 있다. 전환 대상이 `snapshot`이면 복원하여 `active`로, 이미 `active`면 그대로 접근시킨다. 전환은 세션 격리(AC-A2)를 깨지 않는다.
- **달성 가치**: V4, V3
- **검증 방법**: 여러 세션을 오가며 전환 시 매번 대상 세션이 올바른 상태로 활성화되고, 이전 세션의 상태가 보존됨을 확인한다.
