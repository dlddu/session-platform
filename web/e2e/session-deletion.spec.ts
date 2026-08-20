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

// The snapshot session under test is real: created through the product API and
// frozen through the product snapshot endpoint (the same trigger the Go suite
// uses for AC-A3/AC-D4), so /restore, the list, the single GET and the retried
// DELETE are all served by the deployed control plane. The only substitution is
// the very first DELETE's 409 — see the fidelity allowlist in docs/test/e2e.md.
test("delete a snapshot from Restore and retry after a conflict", async ({
  page,
  request,
}) => {
  // A real freeze runs CRIU inside the session pod and uploads the archive to
  // the in-cluster MinIO, which is far slower than the default test timeout.
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
  // A SUT without a reachable product snapshot endpoint (older build, or the
  // CRIU gate off) cannot produce a snapshot-state session at all; the Go suite
  // skips on the same signal rather than reporting a browser regression.
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
  // mock-exception: DELETE-CONFLICT-ERR — 409는 다른 라이프사이클 연산이 세션 Lease를
  // 쥐고 있을 때만 나오는 응답이라(service.Terminate → session.ErrConflict) 실 SUT에
  // 요청 시점에 결정적으로 유발할 수 없다. 등재: docs/test/e2e.md 「e2e 충실도 허용목록」.
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
  // The rejected attempt changed nothing on the real SUT.
  expect(
    (await request.get("/api/v1/sessions/" + created.id)).status(),
  ).toBe(200);

  // The retry is no longer intercepted: the deployed control plane deletes it.
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
