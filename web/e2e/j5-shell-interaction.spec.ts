import { test, expect, type Page } from "@playwright/test";

// JRN-shell-interaction — running commands in the session shell and following the output.
// Value V6 (interactive shell session); AC-D2 (write → shell stdin) and AC-D3
// (read = offset-cursored delta of the scrollback; offset 0 replays the full
// history on re-entry).
//
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

  // STP-command-input/STP-output-read: a command's output lands in the console. The computed marker
  // only exists once bash executed the line (the echoed input can't match).
  await runCommand(page, "echo j5-first-$((40+1))");
  await expect(log).toContainText("j5-first-41", { timeout: 15_000 });

  // A second command appends after the first — the scrollback accumulates
  // (cursor reads only fetch the delta, but the console keeps the history).
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

  // Leave for the Sessions console, then come back in through its card.
  await page.getByRole("link", { name: "← Sessions" }).click();
  await expect(page).toHaveURL(/\/$/);
  await page.locator('[data-testid="session-card"]', { hasText: name }).click();
  await expect(page).toHaveURL(/\/session\/[0-9a-f]+$/);

  // AC-D3 non-consuming: the entry read at offset 0 restores the history
  // produced before we left — nothing was consumed by earlier reads.
  await expect(page.getByTestId("ws-log")).toContainText("j5-history-44", { timeout: 15_000 });
});
