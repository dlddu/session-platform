import type { Session } from "../api/types";
import { isAgentWorkload, isGatedWorkload } from "./workloadKind";

type RoutableSession = Pick<Session, "id" | "state" | "workloadType">;

// Keep every entry point (create, card click, restore) on the same workload
// routing rule — a copied/deep link should communicate which workspace opens.
// A gated session gets its own path rather than sharing /agent: it runs the
// same execution model but not the same screen, and the URL is the only part a
// user can read before the session loads. Gated is a capability of a type
// (isGatedWorkload), checked before the agent family it also belongs to.
export function liveSessionPath(
  session: Pick<RoutableSession, "id" | "workloadType">,
): string {
  if (isGatedWorkload(session.workloadType)) return `/gated/${session.id}`;
  if (isAgentWorkload(session.workloadType)) return `/agent/${session.id}`;
  return `/session/${session.id}`;
}

export function sessionEntryPath(session: RoutableSession): string {
  return session.state === "snapshot"
    ? `/restore/${session.id}`
    : liveSessionPath(session);
}
