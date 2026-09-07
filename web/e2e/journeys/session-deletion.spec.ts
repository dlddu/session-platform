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

// 인터셉트는 첫 DELETE 의 409 하나뿐 — DELETE-CONFLICT-ERR(NET) 등재, docs/test/e2e.md.
test("delete a snapshot from Restore and retry after a conflict", async ({
  page,
  request,
}) => {
  // 실 CRIU 동결과 MinIO 업로드가 선행하므로 기본 타임아웃으로는 모자란다.
  test.setTimeout(180_000);

  const name = "delete-snapshot-" + Date.now();
  const createResponse = await request.post("/api/v1/sessions", {
    data: { name },
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as { id: string };

  const snapshotResponse = await request.post(
    "/api/v1/sessions/" + created.id + "/snapshot",
    { timeout: 150_000 },
  );
  // snapshot 상태를 만들 수 없는 SUT 는 브라우저 회귀가 아니라 전제 미달이다 — Go 스위트도
  // 같은 신호에 skip 한다.
  test.skip(
    snapshotResponse.status() === 404 || snapshotResponse.status() === 503,
    "SUT has no reachable product snapshot endpoint — no snapshot-state session to delete here",
  );
  expect(snapshotResponse.status()).toBe(200);
  const frozen = (await snapshotResponse.json()) as {
    state: string;
    pod?: string;
  };
  expect(frozen.state).toBe("snapshot");
  expect(frozen.pod ?? "").toBe("");

  let conflictInjected = false;
  // mock-exception: DELETE-CONFLICT-ERR — 세션 Lease 경합은 실 SUT 에 요청 시점에 결정적으로 유발할 수 없다.
  await page.route("**/api/v1/sessions/" + created.id, async (route) => {
    if (conflictInjected || route.request().method() !== "DELETE") {
      await route.continue();
      return;
    }
    conflictInjected = true;
    await route.fulfill({
      status: 409,
      contentType: "application/json",
      body: JSON.stringify({ error: "session state changed concurrently" }),
    });
  });

  await page.goto("/restore/" + created.id);
  const deleteButton = page.getByTestId("restore-delete-session");
  await expect(deleteButton).toBeVisible();
  await deleteButton.click();
  const confirmButton = page.getByTestId("delete-session-confirm");
  await confirmButton.click();

  await expect(page.getByTestId("delete-session-error")).toContainText(
    "409 session state changed concurrently",
  );
  await expect(page).toHaveURL(new RegExp(`/restore/${created.id}$`));
  expect(conflictInjected).toBe(true);
  expect(
    (await request.get("/api/v1/sessions/" + created.id)).status(),
  ).toBe(200);

  await confirmButton.click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole("status")).toContainText(
    'Session "' + name + '" deleted',
  );
  await expect(
    page.locator(
      '[data-testid="session-card"][data-session-id="' + created.id + '"]',
    ),
  ).toHaveCount(0);
  expect(
    (await request.get("/api/v1/sessions/" + created.id)).status(),
  ).toBe(404);
});
