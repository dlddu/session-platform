import { Buffer } from "node:buffer";
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
  snapshotAfterFirstStream?: boolean;
  resetBeforeAgentOutput?: boolean;
};

function json(route: Route, status: number, body: unknown) {
  return route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

function sse(route: Route, body: string) {
  return route.fulfill({
    status: 200,
    contentType: "text/event-stream",
    headers: {
      "Cache-Control": "no-cache",
      Connection: "keep-alive",
    },
    body,
  });
}

function outputEvent(offset: number, payload: Uint8Array) {
  const bytes = Buffer.from(payload);
  const nextOffset = offset + bytes.byteLength;
  return {
    nextOffset,
    wire:
      `event: output\n` +
      `id: ${nextOffset}\n` +
      `data: ${JSON.stringify({
        offset,
        payloadBase64: bytes.toString("base64"),
        nextOffset,
      })}\n\n`,
  };
}

function resetEvent(nextOffset: number) {
  return (
    `event: reset\n` +
    `id: ${nextOffset}\n` +
    `data: ${JSON.stringify({ nextOffset })}\n\n`
  );
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
  const streamOffsets: number[] = [];
  const existingOutput = Buffer.from("existing agent response\n");
  const firstAgentChunk = Buffer.from("new agent response 응");
  const secondAgentChunk = Buffer.from("답\n");
  const newOutput = Buffer.concat([firstAgentChunk, secondAgentChunk]);
  let retainedOutput = existingOutput;
  let deliveredAgentOutput = false;
  let streamRequests = 0;
  let finalStreamOffset = existingOutput.byteLength;
  let signalFirstWrite: () => void = () => undefined;
  let firstWriteSignalled = false;
  const firstWriteAccepted = new Promise<void>((resolve) => {
    signalFirstWrite = resolve;
  });

  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const pathname = url.pathname;
    const method = request.method();

    if (pathname.endsWith("/stream") && method === "GET") {
      const offset = Number(url.searchParams.get("offset") ?? "0");
      streamOffsets.push(offset);
      streamRequests += 1;

      if (streamRequests === 1) {
        const initial = outputEvent(0, retainedOutput);
        if (options.snapshotAfterFirstStream) {
          session = {
            ...session,
            state: "snapshot",
            pod: undefined,
            checkpoint: {
              ref: "s3://sessions/a11ce/archive-after-stream.tar.zst",
              sizeBytes: 8192,
              createdAt: now,
              reclaimed: "1 vCPU · 2 GB",
            },
          };
        }
        await sse(route, initial.wire);
        return;
      }

      if (!deliveredAgentOutput) {
        await Promise.race([
          firstWriteAccepted,
          new Promise<void>((resolve) => page.once("close", resolve)),
        ]);
        if (page.isClosed()) return;

        // Each event ends on a UTF-8 boundary. Replaying the first byte range
        // still exercises cursor-based de-duplication with non-ASCII output.
        const chunkStart = options.resetBeforeAgentOutput
          ? Math.min(Buffer.byteLength("existing "), retainedOutput.byteLength)
          : retainedOutput.byteLength;
        const resetWire = options.resetBeforeAgentOutput
          ? resetEvent(chunkStart)
          : "";
        if (options.resetBeforeAgentOutput) {
          retainedOutput = retainedOutput.subarray(0, chunkStart);
        }

        const first = outputEvent(chunkStart, firstAgentChunk);
        const second = outputEvent(first.nextOffset, secondAgentChunk);
        retainedOutput = Buffer.concat([retainedOutput, newOutput]);
        finalStreamOffset = retainedOutput.byteLength;
        deliveredAgentOutput = true;
        await sse(
          route,
          resetWire + first.wire + first.wire + second.wire,
        );
        return;
      }

      // Playwright route.fulfill cannot leave a streaming response open. Keep
      // the final reconnect pending until the page closes instead.
      await new Promise<void>((resolve) => page.once("close", resolve));
      return;
    }

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
      await json(route, 200, {
        session,
        path: "active/read",
        payload: retainedOutput
          .subarray(Math.min(offset, retainedOutput.byteLength))
          .toString(),
        nextOffset: retainedOutput.byteLength,
      });
      return;
    }

    if (pathname.endsWith("/write") && method === "POST") {
      const body = request.postDataJSON() as { payload?: string };
      writePayloads.push(body.payload ?? "");
      if (!firstWriteSignalled) {
        firstWriteSignalled = true;
        signalFirstWrite();
      }
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

  return {
    createBodies,
    writePayloads,
    readOffsets,
    streamOffsets,
    getFinalStreamOffset: () => finalStreamOffset,
    getRetainedOutput: () => retainedOutput.toString(),
    getSession: () => session,
  };
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

test("streams partial agent output, de-duplicates it, and reconnects by byte cursor", async ({
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
    "2 submissions pending",
  );
  await expect
    .poll(() => mock.writePayloads.length, { timeout: 5_000 })
    .toBe(2);
  expect(mock.writePayloads).toEqual(["first prompt", "second prompt"]);
  expect(mock.writePayloads.every((payload) => !payload.endsWith("\n"))).toBe(
    true,
  );

  const log = page.getByTestId("ws-log");
  await expect(log).toContainText("▸ first prompt");
  await expect(log).toContainText("new agent response 응답", {
    timeout: 5_000,
  });
  expect(mock.readOffsets).toEqual([]);

  // The fixture replayed a complete UTF-8 byte range, but it appears only once.
  // The non-ASCII chunks also verify that byte offsets are not JS string lengths.
  const logText = await log.textContent();
  expect(logText?.split("new agent response").length).toBe(2);

  // A finite mocked stream disconnects after the chunks. The browser reconnects
  // with the server-issued byte cursor, not JS string length or offset zero.
  await expect
    .poll(() => mock.streamOffsets, { timeout: 5_000 })
    .toContain(mock.getFinalStreamOffset());
  await expect(page.getByTestId("ws-stream-status")).toHaveAttribute(
    "data-stream-state",
    "reconnecting",
  );

  // POST /read remains an explicit recovery path, but normal output needed no
  // click and made no read request.
  await expect(page.getByTestId("ws-refresh-output")).toBeEnabled({
    timeout: 5_000,
  });
  const readsBeforeRefresh = mock.readOffsets.length;

  await page.getByTestId("ws-refresh-output").click();
  await expect.poll(() => mock.readOffsets.length).toBeGreaterThan(
    readsBeforeRefresh,
  );
  await expect
    .poll(() => mock.readOffsets[mock.readOffsets.length - 1])
    .toBe(mock.getFinalStreamOffset());
});

test("an authoritative reset replaces retained output before reconnecting", async ({
  page,
}) => {
  const mock = await installAgentApi(page, {
    resetBeforeAgentOutput: true,
  });

  await page.goto("/agent/a11ce");
  const log = page.getByTestId("ws-log");
  await expect(log).toContainText("existing agent response");

  const prompt = page.getByTestId("ws-prompt");
  await prompt.fill("trigger reset");
  await prompt.press("Enter");
  await expect.poll(() => mock.writePayloads).toContain("trigger reset");

  // reset closes the SSE source, performs the explicit offset-zero read, and
  // replaces stale mixed scrollback before reconnecting at the replay cursor.
  await expect.poll(() => mock.readOffsets).toContain(0);
  await expect
    .poll(() => log.textContent())
    .toBe(mock.getRetainedOutput());
  await expect(log).not.toContainText("▸ trigger reset");
  await expect
    .poll(() => mock.streamOffsets)
    .toContain(mock.getFinalStreamOffset());
});

test("a stream disconnect observes snapshot state without restoring by read", async ({
  page,
}) => {
  const mock = await installAgentApi(page, {
    snapshotAfterFirstStream: true,
  });

  await page.goto("/agent/a11ce");
  await expect(page).toHaveURL(/\/restore\/a11ce$/, { timeout: 5_000 });
  await expect(
    page.getByRole("heading", { name: "Resume from session archive" }),
  ).toBeVisible();

  expect(mock.getSession().state).toBe("snapshot");
  expect(mock.readOffsets).toEqual([]);
  expect(mock.streamOffsets).toEqual([0]);

  // Cleanup closes the EventSource, so the Restore screen cannot reconnect and
  // accidentally turn passive observation into snapshot activation.
  await page.waitForTimeout(400);
  expect(mock.streamOffsets).toEqual([0]);
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
