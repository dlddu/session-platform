// 검증 AC: 없음 (스모크·인프라)
//
// Non-AC smoke: the SPA boots against the deployed SUT and the Sessions console
// renders its shell (heading + "New session" entry point). Guards the baseURL
// wiring and that the embedded SPA is being served before anything else runs.
//
// Registered as a non-AC matching unit in docs/test/e2e.md (1:1 rule 3). The
// journey specs under ./journeys/ are outside the matching space — see the
// registry for why.
import { test, expect } from "@playwright/test";

test("app boots and renders the Sessions console", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sessions", level: 1 })).toBeVisible();
  await expect(page.getByTestId("new-session-link")).toBeVisible();
});
