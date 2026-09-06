// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { Buffer } from "node:buffer";
import { expect, test, type Page, type Route } from "@playwright/test";

// JRN-agent-stream-recovery — 실 스트림이 원리적으로 내지 않는 사건에 대한 브라우저의 복구.
// 무엇을 가로채고 무엇이 남는지는 STREAM-RESET-REPLAY(NET) 등재가 갖는다.

const EXISTING = Buffer.from("existing agent response\n");
const FIRST_CHUNK = Buffer.from("new agent response 응");
const SECOND_CHUNK = Buffer.from("답\n");

type Session = { id: string; state: string; workloadType: string };

function sse(route: Route, body: string) {
  return route.fulfill({
    status: 200,
    contentType: "text/event-stream",
    headers: { "Cache-Control": "no-cache", Connection: "keep-alive" },
    body,
  });
}

function outputEvent(offset: number, payload: Buffer) {
  const nextOffset = offset + payload.byteLength;
  return {
    nextOffset,
    wire:
      `event: output\n` +
      `id: ${nextOffset}\n` +
      `data: ${JSON.stringify({
        offset,
        payloadBase64: payload.toString("base64"),
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

// mock-exception: STREAM-RESET-REPLAY — append-only 스크롤백을 가진 실 SUT 는 past-end `reset` 도 같은 범위의 재전송도 낼 수 없다.
async function installOutputFixture(
  page: Page,
  session: Session,
  onStream: (n: number, state: { retained: Buffer }) => string | null,
) {
  const state = { retained: EXISTING };
  const streamOffsets: number[] = [];
  const readOffsets: number[] = [];
  let streamRequests = 0;

  await page.route(
    new RegExp("/api/v1/sessions/" + session.id + "/stream"),
    async (route) => {
      streamOffsets.push(
        Number(new URL(route.request().url()).searchParams.get("offset") ?? "0"),
      );
      streamRequests += 1;
      const wire = onStream(streamRequests, state);
      if (wire === null) {
        // route.fulfill 은 열린 스트리밍 응답을 남길 수 없다. 마지막 재연결은 페이지가
        // 닫힐 때까지 보류한다.
        await new Promise<void>((resolve) => page.once("close", resolve));
        return;
      }
      await sse(route, wire);
    },
  );

  await page.route(
    new RegExp("/api/v1/sessions/" + session.id + "/read"),
    async (route) => {
      const body = route.request().postDataJSON() as { offset?: number };
      readOffsets.push(body.offset ?? 0);
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          session,
          path: "active/read",
          payload: state.retained.toString(),
          nextOffset: state.retained.byteLength,
        }),
      });
    },
  );

  return {
    streamOffsets,
    readOffsets,
    getRetained: () => state.retained.toString(),
    getRetainedLength: () => state.retained.byteLength,
  };
}

async function createAgentSession(
  request: import("@playwright/test").APIRequestContext,
  name: string,
): Promise<Session> {
  const response = await request.post("/api/v1/sessions", {
    data: { name, workloadType: "claude-code" },
    timeout: 180_000,
  });
  expect(response.status()).toBe(201);
  const created = (await response.json()) as Session;
  expect(created.state).toBe("active");
  return created;
}

test("a replayed byte range is rendered once and the stream reconnects at the server cursor", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const session = await createAgentSession(request, "j6-replay-" + Date.now());
  const fixture = await installOutputFixture(page, session, (n, state) => {
    if (n === 1) return outputEvent(0, EXISTING).wire;
    if (n === 2) {
      const first = outputEvent(state.retained.byteLength, FIRST_CHUNK);
      const second = outputEvent(first.nextOffset, SECOND_CHUNK);
      state.retained = Buffer.concat([
        state.retained,
        FIRST_CHUNK,
        SECOND_CHUNK,
      ]);
      // 각 이벤트가 UTF-8 경계에서 끝나므로 재전송된 첫 범위도 유효한 바이트열이다.
      return first.wire + first.wire + second.wire;
    }
    return null;
  });

  await page.goto("/agent/" + session.id);
  const log = page.getByTestId("ws-log");
  await expect(log).toContainText("existing agent response", {
    timeout: 30_000,
  });
  await expect(log).toContainText("new agent response 응답", {
    timeout: 30_000,
  });

  // 비-ASCII 청크가 「바이트 오프셋 != JS 문자열 길이」까지 함께 증명한다.
  const logText = await log.textContent();
  expect(logText?.split("new agent response").length).toBe(2);

  await expect
    .poll(() => fixture.streamOffsets, { timeout: 30_000 })
    .toContain(fixture.getRetainedLength());
  await expect(page.getByTestId("ws-stream-status")).toHaveAttribute(
    "data-stream-state",
    "reconnecting",
  );

  // 위 출력은 클릭 없이 왔다 — `/read` 는 명시적 복구 경로일 뿐이다.
  expect(fixture.readOffsets).toEqual([]);

  await page.getByTestId("ws-refresh-output").click();
  await expect
    .poll(() => fixture.readOffsets.length, { timeout: 30_000 })
    .toBeGreaterThan(0);
  await expect
    .poll(() => fixture.readOffsets[fixture.readOffsets.length - 1])
    .toBe(fixture.getRetainedLength());
});

test("an authoritative reset replaces retained output before reconnecting", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const session = await createAgentSession(request, "j6-reset-" + Date.now());
  const truncateTo = Buffer.byteLength("existing ");
  const fixture = await installOutputFixture(page, session, (n, state) => {
    if (n === 1) return outputEvent(0, EXISTING).wire;
    if (n === 2) {
      // 클라이언트가 reset 에서 이 소스를 즉시 닫으므로 뒤이은 이벤트는 읽히지 않는다.
      state.retained = Buffer.concat([
        state.retained.subarray(0, truncateTo),
        FIRST_CHUNK,
        SECOND_CHUNK,
      ]);
      return resetEvent(truncateTo);
    }
    return null;
  });

  await page.goto("/agent/" + session.id);
  const log = page.getByTestId("ws-log");
  await expect(log).toContainText("existing agent response", {
    timeout: 30_000,
  });

  await expect
    .poll(() => fixture.readOffsets, { timeout: 30_000 })
    .toContain(0);
  await expect
    .poll(() => log.textContent(), { timeout: 30_000 })
    .toBe(fixture.getRetained());
  await expect
    .poll(() => fixture.streamOffsets, { timeout: 30_000 })
    .toContain(fixture.getRetainedLength());
});
