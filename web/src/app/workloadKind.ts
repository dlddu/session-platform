import type { WorkloadType } from "../api/types";

// One place to answer "which interaction model does this type get?", so a
// fourth type cannot be added by editing two of the five screens.
// 실행 모델 계약은 AC-F1 이 AC-E2~E6 을 재사용하므로, 그 계약에서 파생된 것은 둘 다 받아야 한다.
export function isAgentWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "claude-code" || workloadType === "approval-gated";
}

// AC-F2~F6 이 얹는 차이. 화면은 이것을 게이트 전용 표면에만 쓰고, 에이전트인지
// 판정하는 데는 쓰지 않는다.
export function isGatedWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "approval-gated";
}
