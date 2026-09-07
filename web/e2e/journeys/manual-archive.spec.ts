import { expect, test } from "@playwright/test";

// 인터셉트 0 — 충실도 허용목록(docs/test/e2e.md)의 「해소된 것」②.
test("manually archives a Claude Code session from the workspace", async ({
  page,
  request,
}) => {
  // 플러그인 설치와 MinIO 아카이브가 선행하므로 기본 타임아웃으로는 모자란다.
  test.setTimeout(300_000);

  const name = "manual-archive-" + Date.now();
  const createResponse = await request.post("/api/v1/sessions", {
    data: { name, workloadType: "claude-code" },
    timeout: 180_000,
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as {
    id: string;
    state: string;
    workloadType: string;
  };
  expect(created.workloadType).toBe("claude-code");
  expect(created.state).toBe("active");

  await page.goto("/agent/" + created.id);
  const archiveButton = page.getByTestId("ws-archive-session");
  await expect(archiveButton).toHaveText("Archive now");
  await expect(archiveButton).toBeEnabled();

  await archiveButton.click();
  await expect(archiveButton).toHaveText("Archiving…");
  await expect(archiveButton).toBeDisabled();

  await expect(page).toHaveURL(/\/$/, { timeout: 180_000 });
  await expect(page.getByRole("status")).toContainText(
    "Session archived — pod reclaimed",
  );
  await expect(
    page.locator(
      '[data-testid="session-card"][data-session-id="' + created.id + '"]',
    ),
  ).toHaveAttribute("data-state", "snapshot");

  const after = await request.get("/api/v1/sessions/" + created.id);
  expect(after.status()).toBe(200);
  const frozen = (await after.json()) as {
    state: string;
    pod?: string;
    checkpoint?: { ref?: string };
  };
  expect(frozen.state).toBe("snapshot");
  expect(frozen.pod ?? "").toBe("");
  expect(frozen.checkpoint?.ref ?? "").not.toBe("");
});
