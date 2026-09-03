// mockup: docs/mockups/index.html
// docs/mockups/README.md 의 「화면 ↔ mockup 매핑」 표와 양방향으로 일치해야 한다 (scripts/check-render-fidelity.py).
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
