import type { Session } from "../api/types";

type RoutableSession = Pick<Session, "id" | "state" | "workloadType">;

// Keep every entry point (create, card click, restore) on the same workload
// routing rule — a copied/deep link should communicate which workspace opens.
export function liveSessionPath(
  session: Pick<RoutableSession, "id" | "workloadType">,
): string {
  switch (session.workloadType) {
    case "claude-code":
    case "minimax-code":
      return `/agent/${session.id}`;
    default:
      return `/session/${session.id}`;
  }
}

export function sessionEntryPath(session: RoutableSession): string {
  return session.state === "snapshot"
    ? `/restore/${session.id}`
    : liveSessionPath(session);
}
