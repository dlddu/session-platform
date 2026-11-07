// 검증 시나리오: codex-workload.md#시나리오 7
//
// docs/prd/codex-workload.md AC-J7. kind SUT 의 codex 사이드카에는 마켓플레이스 토큰이 없으므로
// 새로고침 결과는 「새로 받을 마켓플레이스가 없다」다 — 이 시나리오가 사는 것은 버튼 → control plane
// → 세션 파드 에이전트 → 사이드카까지 한 번 도는 경로와, 그 버튼이 codex 세션에만 있다는 것이다.
import { expect, test, type APIRequestContext } from "@playwright/test";

const POD_BOUND = 240_000;

type Session = { id: string; state: string; workloadType: string };

async function create(request: APIRequestContext, name: string, workloadType: string): Promise<Session> {
  const created = await request.post("/api/v1/sessions", { data: { name, workloadType } });
  expect(created.status()).toBe(201);
  return (await created.json()) as Session;
}

test("a codex session refreshes its plugin skills from the workspace", async ({ page, request }) => {
  test.setTimeout(300_000);
  const stamp = Date.now();
  const ids: string[] = [];

  try {
    const codex = await create(request, `codex-7-${stamp}`, "codex-chat");
    ids.push(codex.id);
    const shell = await create(request, `codex-7-shell-${stamp}`, "shell");
    ids.push(shell.id);

    await page.goto(`/agent/${codex.id}`);
    const button = page.getByTestId("ws-refresh-plugins");
    await expect(button).toBeVisible({ timeout: POD_BOUND });
    await expect(button).toHaveText("Refresh plugin skills");
    await expect(button).toBeEnabled({ timeout: POD_BOUND });

    await button.click();
    await expect(page.locator("#toasts")).toContainText(
      "This session has no plugin marketplace to refresh",
      { timeout: 60_000 },
    );
    await expect(page.getByTestId("ws-log")).toContainText(
      "[session-platform: this session has no plugin marketplace to refresh]",
      { timeout: 60_000 },
    );
    await expect(page.getByTestId("ws-state")).toHaveText("active");

    const refused = await request.post(`/api/v1/sessions/${shell.id}/plugins/refresh`);
    expect(refused.status()).toBe(422);

    await page.goto(`/session/${shell.id}`);
    await expect(page.getByTestId("ws-state")).toBeVisible({ timeout: POD_BOUND });
    await expect(page.getByTestId("ws-refresh-plugins")).toHaveCount(0);
  } finally {
    for (const id of ids) {
      const deleted = await request.delete(`/api/v1/sessions/${id}`);
      expect([204, 404]).toContain(deleted.status());
    }
  }
});
