# CRIU 검증 환경 — 확인필요 (open item)

> 상태: **미확정 / 확인필요**. 이 문서는 CRIU 체크포인트·복원의 실구현을 검증할
> 환경을 확정하기 위한 자리표시자다. 부트스트랩 스캐폴딩에서 CRIU 경로는
> 게이트(`CRIU_ENABLED`)로 분리되어 있고, 게이트 off에서는 no-op로 통과한다.

## 왜 별도 항목인가
- K8s `ContainerCheckpoint`(kubelet) API는 **alpha**이며 클러스터/런타임 설정이 필요하다.
- "체크포인트를 **새 pod로 복원**"(AC-B2)은 더 미성숙하여, 표준 경로가 확립되어 있지 않다.
- 따라서 happy-path(생성/목록/전환)는 CRIU 없이 동작하도록 설계했고(스텁), CRIU는
  검증 환경이 확정된 뒤 실구현·검증한다.

## 게이트 동작 (현재 스캐폴딩)
- 환경변수 `CRIU_ENABLED`(기본 `false`).
- off: `criu.StubCheckpointer`가 합성 메타데이터로 no-op 성공 → 스냅샷/복원 플로우가
  엔드투엔드로 도는 골격을 유지.
- on: 동일 스텁이 여전히 실제 CRIU 작업을 하지 않음 — 실구현이 들어갈 위치만 표시.
  integration 하니스의 `TestScenario4_CRIUIntegrity`는 게이트 on에서 의도적으로 실패하여
  "실구현 필요"를 가시화한다.

## 확정해야 할 것 (TODO)
- [ ] 검증 클러스터: kind로 충분한지, 아니면 CRIU 지원 노드(런타임 + 커널 옵션)가 필요한지.
- [ ] 컨테이너 런타임: containerd/runc CRIU 지원 빌드 + `ContainerCheckpoint` 게이트 활성.
- [ ] 체크포인트 이미지 저장소 위치(노드 로컬 vs 오브젝트 스토리지) 및 수명주기.
- [ ] AC-B3(무결성) 검증 방법: 동결 전 인메모리 마커 → 복원 후 동일성 확인 절차.
- [ ] "새 pod로 복원" 경로의 표준화 또는 대안(runc restore + pod 재부착) 결정.

## 관련 코드
- `control-plane/internal/adapter/criu/checkpointer.go` — 포트 + 게이트 스텁
- `control-plane/internal/service/manager.go` — Snapshot/Restore 오케스트레이션
- `control-plane/test/integration_test.go` — `TestScenario4_CRIUIntegrity` (skip/gate)
- `deploy/kind-config.yaml` — CRIU 미활성 명시
