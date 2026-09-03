import type { State } from "../api/types";

export function StateBadge({ state }: { state: State }) {
  return (
    <span className={`badge ${state}`}>
      <span className="led" />
      {state}
    </span>
  );
}
