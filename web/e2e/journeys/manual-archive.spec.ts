import { expect, test, type Route } from "@playwright/test";

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

test("manually archives a Claude Code session from the workspace", async ({
  page,
}) => {
  const now = "2026-08-09T12:00:00Z";
  const active = {
    id: "manual-archive",
    name: "manual-archive",
    workloadType: "claude-code" as const,
    model: "platform-default",
    state: "active" as const,
    pod: "sess-manual-archive",
    createdAt: now,
    lastAccess: now,
  };
  const snapshot = {
    ...active,
    state: "snapshot" as const,
    pod: undefined,
    checkpoint: {
      ref: "s3://sessions/manual-archive/archive.tar.zst",
      sizeBytes: 4096,
      createdAt: now,
      reclaimed: "1 vCPU · 2 GB",
    },
  };

  let archived = false;
  let snapshotRequests = 0;
  let signalSnapshotStarted: () => void = () => undefined;
  const snapshotStarted = new Promise<void>((resolve) => {
    signalSnapshotStarted = resolve;
  });
  let releaseSnapshot: () => void = () => undefined;
  const snapshotRelease = new Promise<void>((resolve) => {
    releaseSnapshot = resolve;
  });

  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    const method = request.method();

    if (
      pathname === `/api/v1/sessions/${active.id}` &&
      method === "GET"
    ) {
      await json(route, 200, archived ? snapshot : active);
      return;
    }
    if (pathname === "/api/v1/sessions" && method === "GET") {
      await json(route, 200, { sessions: [archived ? snapshot : active] });
      return;
    }
    if (
      pathname === `/api/v1/sessions/${active.id}/stream` &&
      method === "GET"
    ) {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        headers: { "Cache-Control": "no-cache" },
        body: "retry: 10000\n: connected\n\n",
      });
      return;
    }
    if (
      pathname === `/api/v1/sessions/${active.id}/snapshot` &&
      method === "POST"
    ) {
      snapshotRequests += 1;
      signalSnapshotStarted();
      await snapshotRelease;
      archived = true;
      await json(route, 200, snapshot);
      return;
    }

    await json(route, 404, { error: `${method} ${pathname} unhandled` });
  });

  await page.goto(`/agent/${active.id}`);
  const archiveButton = page.getByTestId("ws-archive-session");
  await expect(archiveButton).toHaveText("Archive now");
  await expect(archiveButton).toBeEnabled();

  await archiveButton.click();
  await snapshotStarted;
  await expect(archiveButton).toHaveText("Archiving…");
  await expect(archiveButton).toBeDisabled();

  releaseSnapshot();

  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole("status")).toContainText(
    "Session archived — pod reclaimed",
  );
  await expect(
    page.locator(
      `[data-testid="session-card"][data-session-id="${active.id}"]`,
    ),
  ).toHaveAttribute("data-state", "snapshot");
  expect(snapshotRequests).toBe(1);
});
