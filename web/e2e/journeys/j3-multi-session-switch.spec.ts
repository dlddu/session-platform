// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { test, expect } from "@playwright/test";

// JRN-multi-session-switch — docs/user-journeys/JRN-multi-session-switch.md.
// snapshot -> restore 갈래는 deferred.spec.ts 가 갖는다.
test("list multiple active sessions and switch between them", async ({ page, request }) => {
  const prefix = `j3-${Date.now()}`;
  const ids: string[] = [];
  for (let i = 0; i < 3; i++) {
    const res = await request.post("/api/v1/sessions", { data: { name: `${prefix}-${i}` } });
    expect(res.status()).toBe(201);
    const body = await res.json();
    ids.push(body.id as string);
  }

  await page.goto("/");

  // STP-session-list
  for (const id of ids) {
    const card = page.locator(`[data-testid="session-card"][data-session-id="${id}"]`);
    await expect(card).toBeVisible();
    await expect(card).toHaveAttribute("data-state", "active");
  }

  // STP-switch-away / STP-target-activation
  await page.locator(`[data-session-id="${ids[0]}"]`).click();
  await expect(page).toHaveURL(new RegExp(`/session/${ids[0]}$`));
  await expect(page.getByTestId("ws-state")).toHaveText("active");
  await page.getByTestId("ws-switch").click();
  await expect(page.getByTestId("ws-log")).toContainText(/switch\s*→\s*active/);

  // STP-switch-back
  await page.locator("a.back").click();
  await expect(page).toHaveURL(/\/$/);
  await page.locator(`[data-session-id="${ids[1]}"]`).click();
  await expect(page).toHaveURL(new RegExp(`/session/${ids[1]}$`));
  await expect(page.getByTestId("ws-state")).toHaveText("active");
});
