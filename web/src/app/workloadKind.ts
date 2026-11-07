import type { WorkloadType } from "../api/types";

// One place to answer "which interaction model does this type get?", so a
// fourth type cannot be added by editing two of the five screens.
// 실행 모델 계약은 AC-F1 이 AC-E2~E6 을 재사용하므로, 그 계약에서 파생된 것은 둘 다 받아야 한다.
// 동결 방식은 이 판정과 무관하다 — 모든 타입이 파일시스템 아카이브로 동결된다(AC-B1).
export function isAgentWorkload(workloadType: WorkloadType): boolean {
  return (
    workloadType === "claude-code" ||
    workloadType === "approval-gated" ||
    workloadType === "minimax-code" ||
    isCodexWorkload(workloadType)
  );
}

// AC-J1 · AC-M1. 채팅용과 작업용은 러너·플러그인 배선을 같이 쓰므로 로그인 안내와
// 플러그인 새로고침을 함께 받는다.
export function isCodexWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "codex-chat" || workloadType === "codex-task";
}

// AC-F2~F6 이 얹는 차이. 화면은 이것을 게이트 전용 표면에만 쓰고, 에이전트인지
// 판정하는 데는 쓰지 않는다.
export function isGatedWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "approval-gated";
}

export function hasModelChoice(workloadType: WorkloadType): boolean {
  return isAgentWorkload(workloadType) && workloadType !== "minimax-code";
}
