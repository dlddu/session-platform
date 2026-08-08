import type { Session } from "../api/types";

type RoutableSession = Pick<Session, "id" | "state" | "workloadType">;

// Keep every entry point (create, card click, restore) on the same workload
// routing rule. Shell URLs stay backwards compatible; agent sessions have a
// distinct URL so a copied/deep link still communicates which workspace opens.
export function liveSessionPath(
  session: Pick<RoutableSession, "id" | "workloadType">,
): string {
  return session.workloadType === "claude-code"
    ? `/agent/${session.id}`
    : `/session/${session.id}`;
}

export function sessionEntryPath(session: RoutableSession): string {
  return session.state === "snapshot"
    ? `/restore/${session.id}`
    : liveSessionPath(session);
}
