import { test } from "@playwright/test";

// Deferred browser scenarios — blocked on real adapters / lifecycle triggers the
// α stub SUT cannot reach (every session stays active). Seeded as skips so a
// future PR removes the skip and fills the body. Mapping: docs/test/e2e.md.

// J2 / AC-B1: an active session goes idle (60m) and freezes to a snapshot — the
// card shows the frozen badge and "pod reclaimed". Needs an idle->snapshot
// trigger (reaper or test-only endpoint).
test.skip("J2: session freezes to a snapshot after idle", async () => {
  // fill when the idle->snapshot trigger lands.
});

// J2 / AC-B2: opening a snapshot card routes to the Restore screen; "Thaw &
// resume" restores into a new pod and returns to the workspace as active. Needs
// a snapshot-state session (AC-B1 trigger) + restore.
test.skip("J2: thaw & resume restores a snapshot session", async () => {
  // fill when snapshot + restore land.
});

// J4 / AC-C1: concurrent access to one session converges to a single consistent
// state. J4 is a backend concurrency journey with no UI surface (intentional
// non-visualization — see docs/user-journeys/j4-concurrent-access.md), so it has
// no browser assertion. Cross-replica consistency is verified by the Go e2e suite
// (TestDeferred_CrossReplicaAtomicity, against the 2-replica ConfigMap-backed SUT)
// and the hermetic single-winner CAS/Lease proof by the envtest suite. This skip
// stays as a documented pointer, not a pending browser test.
test.skip("J4: concurrent access stays consistent (backend-only; see Go e2e + envtest)", async () => {
  // Intentionally no browser body: J4 has no UI. See the Go/envtest coverage above.
});
