# PRD: 세션 아키텍처 & 격리

> 대상 요구사항: ① control plane API server + data plane pod, ⑤ 세션별 별도 pod

## 달성 가치
- **V1 세션 격리** — 세션별 전용 pod 모델로 격리를 구조적으로 보장
- **V2 유휴 자원 회수** — control plane이 pod 생명주기를 단일 관리하여 회수 가능
- **V5 일관된 세션 상태** — control plane이 세션 메타데이터의 단일 진입점 역할

## Acceptance Criteria

### AC-A1: Control plane / data plane 분리
- **설명**: 시스템은 control plane API server와 data plane pod로 분리된다. Control plane은 세션 생성·조회·전환·스냅샷·복원 요청을 수신·오케스트레이션하고, 실제 세션 워크로드는 data plane pod에서만 수행된다. Control plane은 세션 자체 연산을 직접 수행하지 않는다.
- **달성 가치**: V1, V5
- **검증 방법**: control plane에 세션 생성 요청 시 별도 data plane pod가 기동되며, control plane 프로세스 내부에서는 세션 워크로드가 실행되지 않음을 확인한다.

### AC-A2: 세션당 전용 Pod
- **설명**: 각 세션은 정확히 하나의 전용 data plane pod에 매핑된다(1:1). 서로 다른 세션은 동일 pod를 공유하지 않는다.
- **달성 가치**: V1
- **검증 방법**: N개의 세션을 생성하면 N개의 고유 pod가 존재하고 세션-pod 매핑이 1:1임을 확인한다. 한 세션 pod의 강제 종료가 다른 세션에 영향을 주지 않음을 확인한다.

### AC-A3: 세션 종료·동결 시 자원 회수
- **설명**: 세션이 종료되거나 스냅샷으로 동결되면 해당 data plane pod와 점유 자원(CPU/메모리)이 회수된다.
- **달성 가치**: V2
- **검증 방법**: 세션 동결/종료 후 대상 pod가 제거되고 클러스터 가용 자원이 회복됨을 확인한다.
