import type { Session } from "../api/types";

type RoutableSession = Pick<Session, "id" | "state" | "workloadType">;

// Keep every entry point (create, card click, restore) on the same workload
// routing rule — a copied/deep link should communicate which workspace opens.
// approval-gated gets its own path rather than sharing /agent: the two run the
// same execution model but not the same screen, and the URL is the only part a
// user can read before the session loads.
export function liveSessionPath(
  session: Pick<RoutableSession, "id" | "workloadType">,
): string {
  switch (session.workloadType) {
    case "claude-code":
      return `/agent/${session.id}`;
    case "approval-gated":
      return `/gated/${session.id}`;
    default:
      return `/session/${session.id}`;
  }
}

export function sessionEntryPath(session: RoutableSession): string {
  return session.state === "snapshot"
    ? `/restore/${session.id}`
    : liveSessionPath(session);
}
