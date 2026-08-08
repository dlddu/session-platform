import { expect, test, type Page, type Route } from "@playwright/test";

type SessionFixture = {
  id: string;
  name: string;
  workloadType: "shell" | "claude-code";
  model?: string;
  state: "active" | "idle" | "snapshot";
  pod?: string;
  createdAt: string;
  lastAccess: string;
  checkpoint?: {
    ref: string;
    sizeBytes: number;
    createdAt: string;
    reclaimed?: string;
  };
};

type ApiMockOptions = {
  model?: string;
  snapshot?: boolean;
  writeDelayMs?: number;
};

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

async function installAgentApi(page: Page, options: ApiMockOptions = {}) {
  const now = "2026-08-08T12:00:00Z";
  let session: SessionFixture = {
    id: "a11ce",
    name: "agent-session",
    workloadType: "claude-code",
    ...(options.model ? { model: options.model } : {}),
    state: options.snapshot ? "snapshot" : "active",
    ...(options.snapshot ? {} : { pod: "sess-a11ce-agent" }),
    createdAt: now,
    lastAccess: now,
    ...(options.snapshot
      ? {
          checkpoint: {
            ref: "s3://sessions/a11ce/archive.tar.zst",
            sizeBytes: 4096,
            createdAt: now,
            reclaimed: "1 vCPU · 2 GB",
          },
        }
      : {}),
  };
  const createBodies: Array<Record<string, unknown>> = [];
  const writePayloads: string[] = [];
  const readOffsets: number[] = [];
  let deliveredAgentOutput = false;

  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const pathname = new URL(request.url()).pathname;
    const method = request.method();

    if (pathname === "/api/v1/sessions" && method === "GET") {
      await json(route, 200, { sessions: [session] });
      return;
    }

    if (pathname === "/api/v1/sessions" && method === "POST") {
      const body = request.postDataJSON() as Record<string, unknown>;
      createBodies.push(body);
      session = {
        ...session,
        name: String(body.name),
        workloadType: String(body.workloadType) as SessionFixture["workloadType"],
        model:
          typeof body.model === "string" ? body.model : "platform-default",
        state: "active",
        pod: "sess-a11ce-agent",
        checkpoint: undefined,
      };
      await json(route, 201, session);
      return;
    }

    if (pathname.endsWith("/read") && method === "POST") {
      const body = request.postDataJSON() as { offset?: number };
      const offset = body.offset ?? 0;
      readOffsets.push(offset);
      const hasNewRun = writePayloads.length > 0 && !deliveredAgentOutput;
      const payload =
        offset === 0
          ? "existing agent response\n"
          : hasNewRun
            ? "new agent response\n"
            : "";
      if (hasNewRun && offset !== 0) deliveredAgentOutput = true;
      const nextOffset = payload ? offset + payload.length : offset;
      await json(route, 200, {
        session,
        path: "active/read",
        payload,
        nextOffset,
      });
      return;
    }

    if (pathname.endsWith("/write") && method === "POST") {
      const body = request.postDataJSON() as { payload?: string };
      writePayloads.push(body.payload ?? "");
      if (options.writeDelayMs) {
        await new Promise((resolve) => setTimeout(resolve, options.writeDelayMs));
      }
      await json(route, 200, { session, path: "active/write" });
      return;
    }

    if (pathname.endsWith("/switch") && method === "POST") {
      session = {
        ...session,
        state: "active",
        pod: "sess-a11ce-restored",
        checkpoint: undefined,
      };
      await json(route, 200, session);
      return;
    }

    if (
      pathname === `/api/v1/sessions/${session.id}` &&
      method === "GET"
    ) {
      await json(route, 200, session);
      return;
    }

    await json(route, 404, { error: `unhandled mock route: ${method} ${pathname}` });
  });

  return { createBodies, writePayloads, readOffsets, getSession: () => session };
}

test("creates a claude-code session with a custom model and opens its agent workspace", async ({
  page,
}) => {
  const mock = await installAgentApi(page, { model: "claude-sonnet-test" });

  await page.goto("/new");
  await expect(page.getByTestId("new-session-workload-shell")).toHaveAttribute(
    "aria-checked",
    "true",
  );
  await expect(page.getByTestId("new-session-model")).toHaveCount(0);

  await page.getByTestId("new-session-workload-claude-code").click();
  await page.getByTestId("new-session-name").fill("review-agent");
  await page.getByTestId("new-session-model").fill("claude-sonnet-test");
  await page.getByTestId("new-session-submit").click();

  await expect(page).toHaveURL(/\/agent\/a11ce$/, { timeout: 5_000 });
  expect(mock.createBodies).toEqual([
    {
      name: "review-agent",
      workloadType: "claude-code",
      model: "claude-sonnet-test",
    },
  ]);
  await expect(page.getByRole("heading", { name: "review-agent", level: 1 })).toBeVisible();
  await expect(page.getByTestId("ws-model")).toHaveText(
    "model=claude-sonnet-test",
  );
  await expect(page.getByTestId("ws-workload")).toContainText("claude-code");
});

test("omits a blank model so the platform default is used", async ({ page }) => {
  const mock = await installAgentApi(page);

  await page.goto("/new");
  await page.getByTestId("new-session-workload-claude-code").click();
  await page.getByTestId("new-session-name").fill("default-model-agent");
  await page.getByTestId("new-session-submit").click();

  await expect(page).toHaveURL(/\/agent\/a11ce$/, { timeout: 5_000 });
  expect(mock.createBodies).toEqual([
    {
      name: "default-model-agent",
      workloadType: "claude-code",
    },
  ]);
  await expect(page.getByTestId("ws-model")).toHaveText(
    "model=platform default",
  );
});

test("routes an agent card to prompt UX, preserves prompt payloads, and refreshes output", async ({
  page,
}) => {
  const mock = await installAgentApi(page, {
    model: "claude-opus-test",
    writeDelayMs: 600,
  });

  await page.goto("/");
  const card = page.locator(
    '[data-testid="session-card"][data-session-id="a11ce"]',
  );
  await expect(card).toHaveAttribute("data-workload-type", "claude-code");
  await expect(card).toContainText("◇ claude-code");
  await card.click();

  await expect(page).toHaveURL(/\/agent\/a11ce$/);
  const prompt = page.getByTestId("ws-prompt");
  await prompt.fill("first prompt");
  await prompt.press("Enter");
  await prompt.fill("second prompt");
  await prompt.press("Enter");

  await expect(page.getByTestId("agent-queue")).toContainText(
    "2 submissions · checking output",
  );
  await expect
    .poll(() => mock.writePayloads.length, { timeout: 5_000 })
    .toBe(2);
  expect(mock.writePayloads).toEqual(["first prompt", "second prompt"]);
  expect(mock.writePayloads.every((payload) => !payload.endsWith("\n"))).toBe(
    true,
  );

  await expect(page.getByTestId("ws-log")).toContainText("▸ first prompt");
  await expect(page.getByTestId("ws-log")).toContainText("new agent response", {
    timeout: 5_000,
  });

  await expect(page.getByTestId("ws-refresh-output")).toBeEnabled({
    timeout: 5_000,
  });
  const readsBeforeRefresh = mock.readOffsets.length;
  await page.getByTestId("ws-refresh-output").click();
  await expect
    .poll(() => mock.readOffsets.length)
    .toBeGreaterThan(readsBeforeRefresh);
});

test("describes and restores a claude-code snapshot as an archive", async ({
  page,
}) => {
  const mock = await installAgentApi(page, { snapshot: true });

  await page.goto("/restore/a11ce");
  await expect(
    page.getByRole("heading", { name: "Resume from session archive" }),
  ).toBeVisible();
  await expect(page.getByText("No CRIU checkpoint is used.")).toBeVisible();
  await expect(page.getByText(/archive s3:\/\/sessions\/a11ce/)).toBeVisible();

  await page.getByTestId("restore-submit").click();
  await expect(page).toHaveURL(/\/agent\/a11ce$/);
  expect(mock.getSession().state).toBe("active");
  await expect(page.getByTestId("ws-workload")).toContainText("claude-code");
});
