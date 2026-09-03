import { expect, test } from "@playwright/test";

// The archived session under test is real: created through the product API and
// frozen through the product snapshot endpoint that the workspace's own Archive
// action calls, so the workspace, the in-flight button state, the redirect, the
// toast and the session card that comes back are all served by the deployed
// control plane. Nothing here is intercepted — see the fidelity allowlist in
// docs/test/e2e.md.
//
// The Archive action is workload-agnostic: Workspace.tsx renders it for every
// live session and only the copy differs ("Freeze now" / "Session frozen — pod
// reclaimed" for shell, "Archive now" / "Session archived — pod reclaimed" for
// claude-code). A claude-code session cannot be provisioned in the kind SUT at
// all today, so the agent wording is a deferred seed below.
test("archives a session from the workspace and reclaims its pod", async ({
  page,
  request,
}) => {
  // A real freeze runs CRIU inside the session pod and uploads the archive to
  // the in-cluster MinIO, which is far slower than the default test timeout.
  test.setTimeout(180_000);

  const name = "manual-archive-" + Date.now();
  const createResponse = await request.post("/api/v1/sessions", {
    data: { name },
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as { id: string };

  await page.goto("/session/" + created.id);
  await expect(page.getByTestId("ws-state")).toHaveText("active");

  const archiveButton = page.getByTestId("ws-archive-session");
  await expect(archiveButton).toHaveText("Freeze now");
  await expect(archiveButton).toBeEnabled();

  await archiveButton.click();

  // The in-flight state needs no injected delay: the freeze holds the request
  // open for the whole CRIU dump + archive upload, i.e. seconds, not
  // milliseconds. (If that ever stops being true, the response-delay injection
  // is the registered NET path — see docs/test/e2e.md.)
  await expect(archiveButton).toHaveText("Freezing…");
  await expect(archiveButton).toBeDisabled();
  await expect(archiveButton).toHaveAttribute("aria-busy", "true");

  await expect(page).toHaveURL(/\/$/, { timeout: 150_000 });
  await expect(page.getByRole("status")).toContainText(
    "Session frozen — pod reclaimed",
  );
  await expect(
    page.locator(
      '[data-testid="session-card"][data-session-id="' + created.id + '"]',
    ),
  ).toHaveAttribute("data-state", "snapshot");

  // Ground truth on the SUT: the session really is frozen and its pod is gone.
  const after = await request.get("/api/v1/sessions/" + created.id);
  expect(after.status()).toBe(200);
  const frozen = (await after.json()) as { state: string; pod?: string };
  expect(frozen.state).toBe("snapshot");
  expect(frozen.pod ?? "").toBe("");
});

// Deferred: the claude-code wording of the same action ("Archive now" /
// "Session archived — pod reclaimed", plus the agent output stream going
// offline and restarting when the archive fails). Blocked on the kind SUT being
// able to provision a claude-code session at all — see the two preconditions in
// the fidelity allowlist («미해소 위반») of docs/test/e2e.md. Seeded as a skip so
// a future PR removes the skip and fills the body.
test.skip("archives a claude-code session from the workspace", async () => {
  // fill when the kind SUT can provision a claude-code session.
});
