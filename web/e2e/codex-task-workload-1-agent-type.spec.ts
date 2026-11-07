// 검증 시나리오: codex-task-workload.md#시나리오 1
//
// docs/prd/codex-task-workload.md AC-M1·AC-M4. 프롬프트는 보내지 않는다 — 빈 슬롯의 첫 프롬프트가 ChatGPT 기기 로그인을
// 열기 때문이고, 이 시나리오가 사는 것은 「고를 수 있고, 추론 수준을 골라 만들면 에이전트 작업 화면으로 열린다」까지다.
// 세션은 풀 슬롯 하나를 쥐므로(AC-L2) finally 에서 지워 다른 스위트에 슬롯을 돌려준다.
import { expect, test } from "@playwright/test";

const POD_BOUND = 240_000;

type Session = {
  id: string;
  state: string;
  workloadType: string;
  model?: string;
  reasoningEffort?: string;
};

test("codex-task is offered with a reasoning effort and opens in the agent workspace", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  const name = `codex-task-1-${Date.now()}`;
  let createdId: string | null = null;

  try {
    await page.goto("/new");
    const option = page.getByTestId("new-session-workload-codex-task");
    await expect(option).toBeVisible();
    await expect(option).toContainText(
      "Codex coding agent with a shell, file edits and network. Uses a login slot from the platform pool.",
    );

    await option.click();
    await expect(option).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("new-session-model")).toBeVisible();
    const effort = page.getByTestId("new-session-effort");
    await expect(effort).toBeVisible();
    await expect(effort).toHaveValue("");
    await expect(effort.locator("option")).toHaveText([
      "CLI default",
      "minimal",
      "low",
      "medium",
      "high",
      "xhigh",
    ]);
    await effort.selectOption("high");

    await page.getByTestId("new-session-name").fill(name);
    await page.getByTestId("new-session-submit").click();

    await expect(page).toHaveURL(/\/agent\/[0-9a-f]+$/, { timeout: POD_BOUND });
    createdId = new URL(page.url()).pathname.split("/").pop() as string;

    await expect(page.getByTestId("ws-workload")).toContainText("codex-task");
    await expect(page.getByTestId("ws-reasoning-effort")).toHaveText("high");
    await expect(page.getByTestId("ws-prompt")).toBeVisible();
    await expect(page.getByTestId("ws-stream-status")).toBeVisible();

    const fetched = await request.get(`/api/v1/sessions/${createdId}`);
    expect(fetched.status()).toBe(200);
    const body = (await fetched.json()) as Session;
    expect(body.workloadType).toBe("codex-task");
    expect(body.reasoningEffort).toBe("high");
    expect(body.state).toBe("active");

    await page.goto("/");
    const card = page.locator(`[data-session-id="${createdId}"]`);
    await expect(card).toHaveAttribute("data-workload-type", "codex-task");
    await expect(card).toContainText("◇ codex-task");
  } finally {
    if (createdId) {
      const deleted = await request.delete(`/api/v1/sessions/${createdId}`);
      expect([204, 404]).toContain(deleted.status());
    }
  }
});
