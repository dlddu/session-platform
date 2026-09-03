// 매칭 단위 밖 (AC ↔ e2e 1:1). 등재: docs/test/e2e.md.
import { test } from "@playwright/test";

// Deferred browser scenarios — blocked on real adapters / lifecycle triggers the
// α stub SUT cannot reach (every session stays active). Seeded as skips so a
// future PR removes the skip and fills the body. Mapping: docs/test/e2e.md.

// JRN-idle-resume / AC-B1: an active session goes idle (60m) and freezes to a snapshot — the
// card shows the frozen badge and "pod reclaimed". Needs an idle->snapshot
// trigger (reaper or test-only endpoint).
test.skip("JRN-idle-resume: session freezes to a snapshot after idle", async () => {
  // fill when the idle->snapshot trigger lands.
});

// JRN-idle-resume / AC-B2: opening a snapshot card routes to the Restore screen; "Thaw &
// resume" restores into a new pod and returns to the workspace as active. Needs
// a snapshot-state session (AC-B1 trigger) + restore.
test.skip("JRN-idle-resume: thaw & resume restores a snapshot session", async () => {
  // fill when snapshot + restore land.
});

// JRN-concurrent-access / AC-C1: concurrent access to one session converges to a single consistent
// state. JRN-concurrent-access is a backend concurrency journey with no UI surface (intentional
// non-visualization — see docs/user-journeys/JRN-concurrent-access.md), so it has
// no browser assertion. Cross-replica consistency is verified by the Go e2e suite
// (e2e_c1_atomic_state_test.go, against the 2-replica ConfigMap-backed SUT)
// and the hermetic single-winner CAS/Lease proof by the envtest suite. This skip
// stays as a documented pointer, not a pending browser test.
test.skip("JRN-concurrent-access: concurrent access stays consistent (backend-only; see Go e2e + envtest)", async () => {
  // Intentionally no browser body: JRN-concurrent-access has no UI. See the Go/envtest coverage above.
});
