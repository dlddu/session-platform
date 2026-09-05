// 매칭 단위 밖 (AC ↔ e2e 1:1): 이 디렉터리의 여정 spec 은 web/e2e 최상위가 아니므로 AC
// 매칭 단위가 아니다. 커서 시맨틱의 주검증은 Go e2e 의 AC-D3/AC-B3 전용 파일이 소유한다.
// 등재: docs/test/e2e.md.
import { Buffer } from "node:buffer";
import { expect, test, type Page, type Route } from "@playwright/test";

// JRN-agent-stream-recovery — 실 스트림이 원리적으로 내지 않는 사건에 대한 브라우저의 복구.
//
// 세션은 제품 API 로 배포 SUT 에 **진짜로** 만들어지고, 세션 목록·단건 조회·`write` 는
// 배포된 control-plane 이 응답한다. 가로채는 것은 그 세션의 **출력 표면 두 곳**뿐이다 —
// `/stream` 과 `/read`. 승인 등재 STREAM-RESET-REPLAY(NET), docs/test/e2e.md
// 「e2e 충실도 허용목록」.
//
// 왜 실 SUT 로 못 만드나: 에이전트의 스크롤백은 append-only 이고, `reset` 은 커서가 버퍼
// 끝을 넘었을 때만 나온다(data-plane/cmd/agent/output_stream.go 의 `offset > current`).
// 동결 전 커서가 복원 뒤에도 유효하다는 것이 AC-B3 의 계약이므로, 배포된 SUT 는 요청
// 시점에 이 상태를 만들 수 없다. 같은 이유로 "같은 바이트 범위의 재전송"도 실 스트림에는
// 없다 — 그러나 브라우저는 둘 다 견뎌야 한다.
//
// 진짜 에이전트 출력이 브라우저까지 어떻게 도달하는지는 이 파일이 아니라
// `j6-agent-prompt-loop.spec.ts` 가 인터셉트 없이 단언한다.

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

// mock-exception: STREAM-RESET-REPLAY — append-only 스크롤백을 가진 실 SUT 는 past-end
// `reset` 도 같은 범위의 재전송도 요청 시점에 낼 수 없다(output_stream.go 의 reset 조건은
// `offset > len(buf)` 뿐이고, AC-B3 이 커서 유효성을 보장한다). 등재: docs/test/e2e.md
// 「e2e 충실도 허용목록」.
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
      // 같은 범위를 두 번 보낸다. 각 이벤트는 UTF-8 경계에서 끝나므로 재전송된 첫
      // 범위도 유효한 바이트열이다 — 그래도 화면에는 한 번만 나타나야 한다.
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

  // 재전송된 범위가 커서 기반으로 중복 제거된다. 바이트 오프셋이 JS 문자열 길이가
  // 아니라는 것도 비-ASCII 청크가 함께 증명한다.
  const logText = await log.textContent();
  expect(logText?.split("new agent response").length).toBe(2);

  // 유한한 픽스처 스트림은 청크 뒤에 끊긴다. 브라우저는 **서버가 발급한** 바이트 커서로
  // 재연결한다 — 0 도, JS 문자열 길이도 아니다.
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
      // 서버가 "보관 이력이 네 커서보다 짧다"고 말한다. 클라이언트는 이 소스를 즉시
      // 닫으므로 뒤이은 이벤트를 보내 봐야 읽지 않는다 — 새 이력은 `/read` 가 낸다.
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

  // reset 은 SSE 소스를 닫고, offset=0 전체 재조회를 수행하고, 섞여 있던 낡은 스크롤백을
  // 권위 있는 재생으로 **교체한 뒤에** 그 커서에서 재연결한다.
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
