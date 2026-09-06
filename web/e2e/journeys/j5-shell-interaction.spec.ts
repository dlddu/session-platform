// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { test, expect, type Page } from "@playwright/test";

// JRN-shell-interaction — docs/user-journeys/JRN-shell-interaction.md.
// Output timing is non-deterministic (PTY echo, bash scheduling), so every
// assertion is containment with a generous timeout — never an exact match.

async function createSession(page: Page, name: string) {
  await page.goto("/");
  await page.getByTestId("new-session-link").click();
  await page.getByTestId("new-session-name").fill(name);
  await page.getByTestId("new-session-submit").click();
  await expect(page).toHaveURL(/\/session\/[0-9a-f]+$/);
  await expect(page.getByTestId("ws-state")).toHaveText("active");
}

async function runCommand(page: Page, cmd: string) {
  await page.getByTestId("ws-cmd").fill(cmd);
  await page.getByTestId("ws-cmd").press("Enter");
}

test("commands run in the session shell and output accumulates across them", async ({ page }) => {
  await createSession(page, `j5-${Date.now()}`);
  const log = page.getByTestId("ws-log");

  // STP-command-input / STP-output-read: the computed marker only exists once bash
  // executed the line — the echoed input can't match it.
  await runCommand(page, "echo j5-first-$((40+1))");
  await expect(log).toContainText("j5-first-41", { timeout: 15_000 });

  await runCommand(page, "echo j5-second-$((40+3))");
  await expect(log).toContainText("j5-second-43", { timeout: 15_000 });
  await expect(log).toContainText("j5-first-41");
});

test("re-entering the workspace replays the full scrollback (offset=0)", async ({ page }) => {
  const name = `j5-replay-${Date.now()}`;
  await createSession(page, name);
  const log = page.getByTestId("ws-log");

  await runCommand(page, "echo j5-history-$((40+4))");
  await expect(log).toContainText("j5-history-44", { timeout: 15_000 });

  await page.getByRole("link", { name: "← Sessions" }).click();
  await expect(page).toHaveURL(/\/$/);
  await page.locator('[data-testid="session-card"]', { hasText: name }).click();
  await expect(page).toHaveURL(/\/session\/[0-9a-f]+$/);

  // AC-D3 비파괴 — 재진입 read(offset=0)가 떠나기 전 이력을 그대로 되돌린다.
  await expect(page.getByTestId("ws-log")).toContainText("j5-history-44", { timeout: 15_000 });
});
