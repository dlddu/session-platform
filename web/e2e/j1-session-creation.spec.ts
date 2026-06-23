import { test, expect } from "@playwright/test";

// J1 — first session creation and isolated work.
// Value V1 (isolation) + V5 (single control-plane entry point); AC-A1/A2 (a
// dedicated active session) and AC-C2/C3 (state-branched read/write).
test("create a session and exercise read/write/switch in the workspace", async ({ page }) => {
  const name = `j1-${Date.now()}`;

  // J1-S1: enter via the Sessions console and open the New session modal.
  await page.goto("/");
  await page.getByTestId("new-session-link").click();
  await expect(page).toHaveURL(/\/new$/);

  await page.getByTestId("new-session-name").fill(name);
  await page.getByTestId("new-session-submit").click();

  // J1-S2: routed into the new session's workspace at /session/:id, active.
  await expect(page).toHaveURL(/\/session\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();
  await expect(page.getByTestId("ws-state")).toHaveText("active");

  // J1-S3: read / write / switch hit the state-branched stub endpoints and the
  // SUT's real responses are reflected in the console log.
  const log = page.getByTestId("ws-log");

  await page.getByTestId("ws-read").click();
  await expect(log).toContainText(/read\s*→\s*active/);
  await expect(log).toContainText(/stub:read:/);

  await page.getByTestId("ws-write").click();
  await expect(log).toContainText(/write\s*→\s*active/);

  await page.getByTestId("ws-switch").click();
  await expect(log).toContainText(/switch\s*→\s*active/);

  // switch on an already-active session is a no-op: state stays active.
  await expect(page.getByTestId("ws-state")).toHaveText("active");
});
