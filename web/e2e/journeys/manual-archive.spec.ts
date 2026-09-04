import { expect, test } from "@playwright/test";

// The session under test is real: a claude-code session created through the
// product API on the deployed SUT, archived through the product snapshot
// endpoint. The workspace screen, the archive round trip, the session list and
// the reclaimed-pod ground truth are all served by the deployed control plane.
// Nothing here is intercepted — see the fidelity allowlist in docs/test/e2e.md.
test("manually archives a Claude Code session from the workspace", async ({
  page,
  request,
}) => {
  // A claude-code session pod installs its plugins before the agent starts, and
  // the archive tars the agent workspace into the in-cluster MinIO. Both are far
  // slower than the default test timeout.
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
  // The pending label is observable without injecting a delay: the real archive
  // uploads the agent workspace to MinIO and reclaims the pod before it answers.
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

  // Ground truth from the deployed control plane, not from a fixture: the
  // session really is frozen and its pod really was reclaimed.
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
