import { expect, test } from "@playwright/test";

test("delete a session from the list after explicit confirmation", async ({
  page,
  request,
}) => {
  const name = "delete-" + Date.now();
  const createResponse = await request.post("/api/v1/sessions", {
    data: { name },
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as { id: string };

  await page.goto("/");
  const card = page.locator(
    '[data-testid="session-card"][data-session-id="' + created.id + '"]',
  );
  await expect(card).toBeVisible();

  const actions = card.getByTestId("session-actions");
  await actions.click();
  const deleteAction = card.getByTestId("session-delete");
  await expect(deleteAction).toBeFocused();
  await deleteAction.click();
  await expect(page.getByTestId("delete-session-dialog")).toBeVisible();
  const cancelButton = page.getByTestId("delete-session-cancel");
  const confirmButton = page.getByTestId("delete-session-confirm");
  await expect(cancelButton).toBeFocused();
  await cancelButton.press("Shift+Tab");
  await expect(confirmButton).toBeFocused();
  await confirmButton.press("Tab");
  await expect(cancelButton).toBeFocused();

  await cancelButton.click();
  await expect(page.getByTestId("delete-session-dialog")).toBeHidden();
  await expect(card).toBeVisible();
  await expect(actions).toBeFocused();
  expect(
    (await request.get("/api/v1/sessions/" + created.id)).status(),
  ).toBe(200);

  await actions.press("Enter");
  await expect(deleteAction).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(actions).toBeFocused();
  await expect(deleteAction).toBeHidden();

  await actions.press("Enter");
  await expect(deleteAction).toBeFocused();
  await deleteAction.press("Enter");
  await confirmButton.click();

  await expect(card).toHaveCount(0);
  await expect(page.getByRole("status")).toContainText(
    'Session "' + name + '" deleted',
  );
  expect(
    (await request.get("/api/v1/sessions/" + created.id)).status(),
  ).toBe(404);
});

test("delete an open session and return to the session list", async ({
  page,
  request,
}) => {
  const name = "delete-open-" + Date.now();
  const createResponse = await request.post("/api/v1/sessions", {
    data: { name },
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as { id: string };

  await page.goto("/session/" + created.id);
  await expect(page.getByTestId("ws-state")).toHaveText("active");
  await page.getByTestId("ws-delete-session").click();
  await page.getByTestId("delete-session-confirm").click();

  await expect(page).toHaveURL(/\/$/);
  await expect(
    page.locator(
      '[data-testid="session-card"][data-session-id="' + created.id + '"]',
    ),
  ).toHaveCount(0);
  expect(
    (await request.get("/api/v1/sessions/" + created.id)).status(),
  ).toBe(404);
});

test("delete a snapshot from Restore and retry after a conflict", async ({
  page,
}) => {
  const snapshot = {
    id: "snapshot-delete",
    name: "snapshot-delete",
    workloadType: "shell" as const,
    state: "snapshot" as const,
    createdAt: "2026-08-08T12:00:00Z",
    lastAccess: "2026-08-08T12:00:00Z",
    checkpoint: {
      ref: "s3://sessions/snapshot-delete/checkpoint.tar",
      sizeBytes: 4096,
      createdAt: "2026-08-08T12:00:00Z",
      reclaimed: "1 vCPU",
    },
  };
  let deleteAttempts = 0;
  let deleted = false;

  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    const method = request.method();

    if (pathname === "/api/v1/sessions" && method === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ sessions: deleted ? [] : [snapshot] }),
      });
      return;
    }
    if (
      pathname === `/api/v1/sessions/${snapshot.id}` &&
      method === "GET"
    ) {
      await route.fulfill({
        status: deleted ? 404 : 200,
        contentType: "application/json",
        body: JSON.stringify(deleted ? { error: "not found" } : snapshot),
      });
      return;
    }
    if (
      pathname === `/api/v1/sessions/${snapshot.id}` &&
      method === "DELETE"
    ) {
      deleteAttempts += 1;
      if (deleteAttempts === 1) {
        await route.fulfill({
          status: 409,
          contentType: "application/json",
          body: JSON.stringify({ error: "lifecycle operation in progress" }),
        });
        return;
      }
      deleted = true;
      await route.fulfill({ status: 204 });
      return;
    }

    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({ error: `${method} ${pathname} unhandled` }),
    });
  });

  await page.goto("/restore/" + snapshot.id);
  const deleteButton = page.getByTestId("restore-delete-session");
  await expect(deleteButton).toBeVisible();
  await deleteButton.click();
  const confirmButton = page.getByTestId("delete-session-confirm");
  await confirmButton.click();

  await expect(page.getByTestId("delete-session-error")).toContainText(
    "409 lifecycle operation in progress",
  );
  await expect(page).toHaveURL(new RegExp(`/restore/${snapshot.id}$`));
  expect(deleteAttempts).toBe(1);

  await confirmButton.click();
  await expect(page).toHaveURL(/\/$/);
  expect(deleteAttempts).toBe(2);
  await expect(page.getByRole("status")).toContainText(
    'Session "snapshot-delete" deleted',
  );
});
