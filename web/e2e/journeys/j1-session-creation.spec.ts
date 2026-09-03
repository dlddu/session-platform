// 매칭 단위 밖 (AC ↔ e2e 1:1): 이 디렉터리의 여정 spec은 web/e2e 최상위가 아니므로
// AC 매칭 단위가 아니다. 여정 전체를 한 파일에서 훑는 형태를 유지하되, 각 AC의 주검증은
// Go e2e의 전용 파일이 소유한다. 등재: docs/test/e2e.md.
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
