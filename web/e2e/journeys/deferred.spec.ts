// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { test } from "@playwright/test";

// JRN-idle-resume / AC-B1. Needs an idle->snapshot trigger (reaper or test-only endpoint).
test.skip("JRN-idle-resume: session freezes to a snapshot after idle", async () => {});

// JRN-idle-resume / AC-B2. Needs a snapshot-state session (AC-B1 trigger) + restore.
test.skip("JRN-idle-resume: thaw & resume restores a snapshot session", async () => {});

// JRN-concurrent-access / AC-C1: 의도적 비시각화라 브라우저 단언이 없다 — 이 skip 은 대기 중인
// 테스트가 아니라 문서화된 포인터다. docs/user-journeys/JRN-concurrent-access.md · docs/test/e2e.md.
test.skip("JRN-concurrent-access: concurrent access stays consistent (backend-only; see Go e2e + envtest)", async () => {});
