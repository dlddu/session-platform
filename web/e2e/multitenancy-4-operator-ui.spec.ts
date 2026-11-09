// 검증 시나리오: multitenancy.md#시나리오 4
//
// docs/prd/multitenancy.md AC-K4. 브라우저는 bearer 없이 오므로 운영자다. 테넌트 셋은
// deploy/overlays/e2e/ 오버레이의 ConfigMap `session-platform-tenants` 가 심는다 — e2e-alpha(shell·codex-chat),
// e2e-beta(shell), e2e-gamma("*"). codex-chat 세션에 프롬프트는 보내지 않는다(첫 프롬프트가 ChatGPT 로그인을 연다).
import { expect, test } from "@playwright/test";

const POD_BOUND = 240_000;

type Session = { id: string; state: string; tenant?: string; workloadType: string };

test("operator picks a tenant, creates codex-chat into it, and filters the list by tenant", async ({
  page,
  request,
}) => {
  test.setTimeout(360_000);
  const name = `mt-4-${Date.now()}`;
  let createdId: string | null = null;

  try {
    await page.goto("/new");
    const picker = page.getByTestId("new-session-tenant");
    await expect(picker.locator("option")).toHaveText([
      "No tenant",
      "e2e-alpha",
      "e2e-beta",
      "e2e-gamma",
    ]);
    await expect(picker).toHaveValue("");

    await picker.selectOption("e2e-alpha");
    await expect(
      page.getByRole("radiogroup", { name: "Workload type" }).getByRole("radio"),
    ).toHaveCount(2);
    await expect(page.getByTestId("new-session-workload-shell")).toBeVisible();
    await expect(page.getByTestId("new-session-workload-codex-chat")).toBeVisible();

    await page.getByTestId("new-session-workload-codex-chat").click();
    await page.getByTestId("new-session-name").fill(name);
    await page.getByTestId("new-session-submit").click();
    await expect(page).toHaveURL(/\/agent\/[0-9a-f]+$/, { timeout: POD_BOUND });
    createdId = new URL(page.url()).pathname.split("/").pop() as string;

    const fetched = await request.get(`/api/v1/sessions/${createdId}`);
    expect(fetched.status()).toBe(200);
    const body = (await fetched.json()) as Session;
    expect(body.tenant).toBe("e2e-alpha");
    expect(body.workloadType).toBe("codex-chat");

    await page.goto("/");
    const card = page.locator(`[data-session-id="${createdId}"]`);
    await expect(card.getByTestId("session-tenant")).toHaveText("e2e-alpha");

    const filter = page.getByTestId("tenant-filter");
    await expect(filter.getByRole("button")).toHaveText([
      "All",
      "e2e-alpha",
      "e2e-beta",
      "e2e-gamma",
      "No tenant",
    ]);

    await filter.getByRole("button", { name: "e2e-alpha" }).click();
    await expect(page).toHaveURL(/[?&]tenant=e2e-alpha(&|$)/);
    await expect(card).toBeVisible();

    await filter.getByRole("button", { name: "e2e-beta" }).click();
    await expect(page).toHaveURL(/[?&]tenant=e2e-beta(&|$)/);
    await expect(card).toHaveCount(0);

    // The filter lives in the URL, so a reload (or a shared link) keeps it.
    await page.reload();
    await expect(filter.getByRole("button", { name: "e2e-beta" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await expect(card).toHaveCount(0);
  } finally {
    if (createdId) {
      const deleted = await request.delete(`/api/v1/sessions/${createdId}`);
      expect([204, 404]).toContain(deleted.status());
    }
  }
});
