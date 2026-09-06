// 검증 AC: 없음 (스모크·인프라)
// 등재: docs/test/e2e.md 「비-AC 파일 등재」.
import { test, expect } from "@playwright/test";

test("app boots and renders the Sessions console", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sessions", level: 1 })).toBeVisible();
  await expect(page.getByTestId("new-session-link")).toBeVisible();
});
