import type { WorkloadType } from "../api/types";

// One place to answer "which interaction model does this type get?", so a
// fourth type cannot be added by editing two of the five screens.
// 실행 모델 계약은 AC-G1 이 AC-E2~E6 을 재사용하므로, 그 계약에서 파생된 것은 둘 다 받아야 한다.
export function isAgentWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "claude-code" || workloadType === "minimax-code";
}

export function hasModelChoice(workloadType: WorkloadType): boolean {
  return isAgentWorkload(workloadType) && workloadType !== "minimax-code";
}
