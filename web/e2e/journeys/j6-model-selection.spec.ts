// 매칭 단위 밖 (AC ↔ e2e 1:1): 이 디렉터리의 여정 spec 은 web/e2e 최상위가 아니므로 AC
// 매칭 단위가 아니다. 워크로드 타입·모델의 주검증은 Go e2e 의 AC-E1 전용 파일
// (`e2e_e1_workload_type_test.go`)이 소유한다. 등재: docs/test/e2e.md.
import { expect, test, type Page } from "@playwright/test";

// JRN-agent-model-selection — /new 의 claude-code 모델 선택 UI.
//
// **배포 SUT 는 실 모델 카탈로그를 낸다.** deploy/claude-code-credentials-secret.yaml 이
// `model`·`models` 를 심고, k8s/deployment.yaml 이 그것을 CLAUDE_CODE_DEFAULT_MODEL ·
// CLAUDE_CODE_MODELS 로 제어면에 투영하며, GET /api/v1/config 가 그대로 낸다. 그래서 그
// 형태(구체 기본값이 목록 안에 있는 2개짜리 카탈로그)에 걸리는 여정은 **인터셉트 없이**
// 실 SUT 위에서 돌고, 세션도 진짜로 만들어진다.
//
// 나머지는 이 배포가 **동시에 가질 수 없는 구성**이거나(빈 카탈로그 · 구체 기본값 없음)
// 요청 시점에 낼 수 없는 상태다(503 · 응답 보류). 그것만 GET /api/v1/config **한
// 엔드포인트에** 건 핸들러로 주입한다 — 승인 등재 MODEL-CONFIG-STATE(NET),
// docs/test/e2e.md 「e2e 충실도 허용목록」. 그 밖의 요청(SPA · 세션 생성 · 목록 · 조회)은
// 핸들러를 아예 지나지 않고 배포된 control-plane 이 응답한다.

// 오버레이가 SUT 에 심는 실제 카탈로그.
const SUT_DEFAULT_MODEL = "claude-e2e-model";
const SUT_ALTERNATE_MODEL = "claude-e2e-alternate";

// 주입 응답에만 쓰는 이름들 — SUT 의 실제 카탈로그와 겹치지 않으므로, 화면에 이 값이
// 보이면 그것은 주입이 렌더된 것이지 배포된 카탈로그가 아니다.
const STUB_DEFAULT_MODEL = "claude-sonnet-test";
const STUB_OTHER_MODEL = "claude-opus-test";

type ConfigState =
  | { kind: "catalog"; defaultModel: string; models: string[] }
  | { kind: "fails" };

// mock-exception: MODEL-CONFIG-STATE — 배포된 SUT 하나는 자기 오버레이가 심은 카탈로그
// 하나만 낼 수 있고, 503 이나 응답 보류를 요청 시점에 결정적으로 만들 수도 없다. 등재:
// docs/test/e2e.md 「e2e 충실도 허용목록」.
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
  // 모델 선택은 claude-code 에만 있다.
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
  // 구체 기본값은 목록 안에 있지만 한 번만 나타난다 — 첫 항목이 그것이고, 별도의
  // "Platform default" 항목이 덧붙지 않는다.
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

  // 그라운드 트루스는 배포된 control-plane 이다.
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
  // 기본 선택은 "고르지 않음"이고, 그 상태로 제출하면 model 을 보내지 않는다.
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
  // 자유 입력이라도 제출은 실 SUT 로 간다 — 배포된 카탈로그가 아는 모델을 적어 세션이
  // 진짜로 서는 것까지 본다.
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

  // 늦게 도착한 카탈로그가 이미 입력한 값을 덮지 않는다.
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
