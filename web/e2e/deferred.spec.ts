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

// J4 / AC-C1: a worker (P1) and an automation client (P2) hit the same session
// concurrently and observe a single consistent state. Needs a Redis-backed
// StateStore + multi-replica control-plane.
test.skip("J4: concurrent access stays consistent", async () => {
  // fill when the redis adapter + multi-replica deploy land.
});
