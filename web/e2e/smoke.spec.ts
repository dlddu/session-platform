import { test, expect } from "@playwright/test";

// Smoke: the SPA boots against the deployed SUT and the Sessions console renders
// its shell (heading + "New session" entry point). Guards the baseURL wiring and
// that the embedded SPA is being served before the journey specs run.
test("app boots and renders the Sessions console", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sessions", level: 1 })).toBeVisible();
  await expect(page.getByTestId("new-session-link")).toBeVisible();
});
