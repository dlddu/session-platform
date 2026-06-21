import type { State } from "../api/types";

// Maps session.State enum -> badge styling. Labels match the mockup.
export function StateBadge({ state }: { state: State }) {
  return (
    <span className={`badge ${state}`}>
      <span className="led" />
      {state}
    </span>
  );
}
