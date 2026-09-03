// 매칭 단위 밖 (AC ↔ e2e 1:1). 등재: docs/test/e2e.md.
import { test, expect } from "@playwright/test";

// JRN-multi-session-switch — multiple sessions and free switching. Sessions are
// created via the API so the spec focuses on list -> card -> workspace
// navigation. In the α scope every target is active, so switching is a no-op
// (the snapshot -> restore path is deferred; see deferred.spec.ts).
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

  // STP-session-list: the created sessions appear as active cards in the list.
  for (const id of ids) {
    const card = page.locator(`[data-testid="session-card"][data-session-id="${id}"]`);
    await expect(card).toBeVisible();
    await expect(card).toHaveAttribute("data-state", "active");
  }

  // STP-switch-away/STP-target-activation: open one session, switch (active target -> no-op), state preserved.
  await page.locator(`[data-session-id="${ids[0]}"]`).click();
  await expect(page).toHaveURL(new RegExp(`/session/${ids[0]}$`));
  await expect(page.getByTestId("ws-state")).toHaveText("active");
  await page.getByTestId("ws-switch").click();
  await expect(page.getByTestId("ws-log")).toContainText(/switch\s*→\s*active/);

  // STP-switch-back: navigate back and open a different session; its state is intact.
  await page.locator("a.back").click();
  await expect(page).toHaveURL(/\/$/);
  await page.locator(`[data-session-id="${ids[1]}"]`).click();
  await expect(page).toHaveURL(new RegExp(`/session/${ids[1]}$`));
  await expect(page.getByTestId("ws-state")).toHaveText("active");
});
