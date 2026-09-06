// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { expect, test, type Page } from "@playwright/test";

// JRN-agent-model-selection — /new 의 claude-code 모델 선택 UI.
// 실 SUT 카탈로그로 도는 갈래와 주입 갈래의 경계는 MODEL-CONFIG-STATE(NET) 등재가 갖는다.

const SUT_DEFAULT_MODEL = "claude-e2e-model";
const SUT_ALTERNATE_MODEL = "claude-e2e-alternate";

// 주입 응답에만 쓰는 이름들 — SUT 의 실제 카탈로그와 겹치지 않으므로, 화면에 이 값이
// 보이면 그것은 주입이 렌더된 것이지 배포된 카탈로그가 아니다.
const STUB_DEFAULT_MODEL = "claude-sonnet-test";
const STUB_OTHER_MODEL = "claude-opus-test";

type ConfigState =
  | { kind: "catalog"; defaultModel: string; models: string[] }
  | { kind: "fails" };

// mock-exception: MODEL-CONFIG-STATE — 한 배포는 자기 오버레이가 심은 카탈로그 하나만 낼 수 있다.
async function serveRuntimeConfig(
  page: Page,
  state: ConfigState,
  gate?: Promise<void>,
) {
  let requests = 0;
  await page.route("**/api/v1/config", async (route) => {
    requests += 1;
    if (gate) await gate;
    if (state.kind === "fails") {
      await route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ error: "config unavailable" }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        claudeCode: { defaultModel: state.defaultModel, models: state.models },
      }),
    });
  });
  return { getRequests: () => requests };
}

async function openAgentForm(page: Page) {
  await page.goto("/new");
  await expect(page.getByTestId("new-session-workload-shell")).toHaveAttribute(
    "aria-checked",
    "true",
  );
  await expect(page.getByTestId("new-session-model")).toHaveCount(0);
  await page.getByTestId("new-session-workload-claude-code").click();
  return page.getByTestId("new-session-model");
}

async function submitAndOpenWorkspace(page: Page, name: string) {
  await page.getByTestId("new-session-name").fill(name);
  await page.getByTestId("new-session-submit").click();
  await expect(page).toHaveURL(/\/agent\/[0-9a-f]+$/, { timeout: 240_000 });
  const id = new URL(page.url()).pathname.split("/").pop() as string;
  return id;
}

test("offers the deployed catalog with its concrete default de-duplicated, and creates a session with the chosen model", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const model = await openAgentForm(page);
  await expect(model).toHaveRole("combobox");
  await expect(model.locator("option")).toHaveText([
    SUT_DEFAULT_MODEL + " (platform default)",
    SUT_ALTERNATE_MODEL,
  ]);
  await model.selectOption(SUT_ALTERNATE_MODEL);

  const id = await submitAndOpenWorkspace(page, "j6-model-" + Date.now());
  await expect(page.getByTestId("ws-model")).toHaveText(
    "model=" + SUT_ALTERNATE_MODEL,
  );
  await expect(page.getByTestId("ws-workload")).toContainText("claude-code");

  const created = await request.get("/api/v1/sessions/" + id);
  expect(created.status()).toBe(200);
  expect(((await created.json()) as { model?: string }).model).toBe(
    SUT_ALTERNATE_MODEL,
  );
});

test("keeping the concrete default selected creates a platform-default session", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const model = await openAgentForm(page);
  await expect(
    page.getByText(
      "Workload type and model choice are fixed for this session. Platform default resolves to the configured default at container start.",
      { exact: true },
    ),
  ).toBeVisible();
  await expect(model).toHaveValue("");

  const id = await submitAndOpenWorkspace(page, "j6-default-" + Date.now());
  await expect(page.getByTestId("ws-model")).toHaveText(
    "model=platform default",
  );

  const created = await request.get("/api/v1/sessions/" + id);
  expect(created.status()).toBe(200);
  const body = (await created.json()) as { model?: string };
  expect(["", "platform-default"]).toContain(body.model ?? "");
});

test("names the concrete default in free text mode when the catalog is empty", async ({
  page,
}) => {
  const config = await serveRuntimeConfig(page, {
    kind: "catalog",
    defaultModel: STUB_DEFAULT_MODEL,
    models: [],
  });

  const model = await openAgentForm(page);
  await expect.poll(() => config.getRequests()).toBeGreaterThan(0);
  await expect(model).toHaveRole("textbox");
  await expect(model).toHaveAttribute(
    "placeholder",
    STUB_DEFAULT_MODEL + " (platform default)",
  );
  await expect(
    page.getByText(
      "Leave blank to use " + STUB_DEFAULT_MODEL + " (platform default).",
    ),
  ).toBeVisible();
});

test("submits an explicit free-text model when no catalog is configured", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const config = await serveRuntimeConfig(page, {
    kind: "catalog",
    defaultModel: STUB_DEFAULT_MODEL,
    models: [],
  });

  const model = await openAgentForm(page);
  await expect.poll(() => config.getRequests()).toBeGreaterThan(0);
  await expect(model).toHaveRole("textbox");
  await model.fill(SUT_ALTERNATE_MODEL);

  const id = await submitAndOpenWorkspace(page, "j6-freetext-" + Date.now());
  await expect(page.getByTestId("ws-model")).toHaveText(
    "model=" + SUT_ALTERNATE_MODEL,
  );

  const created = await request.get("/api/v1/sessions/" + id);
  expect(created.status()).toBe(200);
  expect(((await created.json()) as { model?: string }).model).toBe(
    SUT_ALTERNATE_MODEL,
  );
});

test("preserves a pending free-text model when an empty catalog arrives", async ({
  page,
}) => {
  let release: () => void = () => undefined;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await serveRuntimeConfig(
    page,
    { kind: "catalog", defaultModel: STUB_DEFAULT_MODEL, models: [] },
    held,
  );

  const model = await openAgentForm(page);
  await expect(model).toHaveRole("textbox");
  await model.fill("claude-pending-test");

  const configResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/v1/config") &&
      response.request().method() === "GET",
  );
  release();
  await configResponse;

  await expect(model).toHaveValue("claude-pending-test");
  await expect(model).toHaveAttribute(
    "placeholder",
    STUB_DEFAULT_MODEL + " (platform default)",
  );
});

test("keeps the Platform default fallback when no concrete default is configured", async ({
  page,
}) => {
  await serveRuntimeConfig(page, {
    kind: "catalog",
    defaultModel: "platform-default",
    models: [STUB_DEFAULT_MODEL, STUB_OTHER_MODEL],
  });

  const model = await openAgentForm(page);
  await expect(model).toHaveRole("combobox");
  await expect(model.locator("option")).toHaveText([
    "Platform default",
    STUB_DEFAULT_MODEL,
    STUB_OTHER_MODEL,
  ]);
});

test("keeps the free-text model input when config loading fails", async ({
  page,
}) => {
  const config = await serveRuntimeConfig(page, { kind: "fails" });

  const model = await openAgentForm(page);
  await expect.poll(() => config.getRequests()).toBeGreaterThan(0);
  await expect(model).toHaveRole("textbox");
  await expect(model).toHaveAttribute("placeholder", "Platform default");
  await expect(
    page.getByText("Leave blank to use the platform default."),
  ).toBeVisible();
});
