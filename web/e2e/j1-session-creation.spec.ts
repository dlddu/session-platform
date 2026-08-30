import { test, expect } from "@playwright/test";

// JRN-session-creation — first session creation and isolated work.
// Value V1 (isolation) + V5 (single control-plane entry point); AC-A1/A2 (a
// dedicated active session) and AC-C2/C3 via the shell console (STP-isolated-work's
// "isolated work" is concretely JRN-shell-interaction's shell loop: write → stdin, read →
// scrollback).
test("create a session and work in its shell from the workspace", async ({ page }) => {
  const name = `j1-${Date.now()}`;

  // STP-create-request: enter via the Sessions console and open the New session modal.
  await page.goto("/");
  await page.getByTestId("new-session-link").click();
  await expect(page).toHaveURL(/\/new$/);

  await page.getByTestId("new-session-name").fill(name);
  await page.getByTestId("new-session-submit").click();

  // STP-workspace-entry: routed into the new session's workspace at /session/:id, active.
  await expect(page).toHaveURL(/\/session\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { name, level: 1 })).toBeVisible();
  await expect(page.getByTestId("ws-state")).toHaveText("active");

  // STP-isolated-work: isolated work = running a command in the session's own shell. The
  // $((…)) marker only appears once the pod's bash actually executed it.
  const log = page.getByTestId("ws-log");
  await page.getByTestId("ws-cmd").fill("echo j1-marker-$((40+2))");
  await page.getByTestId("ws-cmd").press("Enter");
  await expect(log).toContainText("j1-marker-42", { timeout: 15_000 });

  await page.getByTestId("ws-switch").click();
  await expect(log).toContainText(/switch\s*→\s*active/);

  // switch on an already-active session is a no-op: state stays active.
  await expect(page.getByTestId("ws-state")).toHaveText("active");
});
