// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { test, expect } from "@playwright/test";

// JRN-session-creation — docs/user-journeys/JRN-session-creation.md.
test("create a session and work in its shell from the workspace", async ({ page }) => {
  const name = `j1-${Date.now()}`;

  // STP-create-request
  await page.goto("/");
  await page.getByTestId("new-session-link").click();
  await expect(page).toHaveURL(/\/new$/);

  await page.getByTestId("new-session-name").fill(name);
  await page.getByTestId("new-session-submit").click();

  // STP-workspace-entry
  await expect(page).toHaveURL(/\/session\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();
  await expect(page.getByTestId("ws-state")).toHaveText("active");

  // STP-isolated-work: the $((…)) marker only appears once the pod's bash executed the
  // line — the echoed input can't match it.
  const log = page.getByTestId("ws-log");
  await page.getByTestId("ws-cmd").fill("echo j1-marker-$((40+2))");
  await page.getByTestId("ws-cmd").press("Enter");
  await expect(log).toContainText("j1-marker-42", { timeout: 15_000 });

  await page.getByTestId("ws-switch").click();
  await expect(log).toContainText(/switch\s*→\s*active/);

  await expect(page.getByTestId("ws-state")).toHaveText("active");
});
