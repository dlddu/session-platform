import type { WorkloadType } from "../api/types";

// One place to answer "which interaction model does this type get?", so a
// fourth type cannot be added by editing two of the five screens.
//
// `claude-code` and `approval-gated` share the agent family: AC-F1 reuses
// AC-E2~E6 for the execution model (one-shot prompt, serial queue, offset
// cursor, archive) instead of restating it, so anything derived from *that*
// contract must accept both. Before this module the SPA asked
// `workloadType === "claude-code"` and every other value fell through to the
// shell branch, which silently rendered an `approval-gated` session as a bash
// terminal labelled `$ shell`.
export function isAgentWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "claude-code" || workloadType === "approval-gated";
}

// What `approval-gated` adds on top of the agent family (AC-F2~F6): a locked
// egress list, a helper pod, and an approval gate in front of every outbound
// call. Screens use this only for the gate-specific surface — never to decide
// whether the session is an agent.
export function isGatedWorkload(workloadType: WorkloadType): boolean {
  return workloadType === "approval-gated";
}
