// 검증 시나리오: codex-workload.md#시나리오 1
//
// docs/prd/codex-workload.md AC-J1. 프롬프트는 보내지 않는다 — 첫 프롬프트가 ChatGPT 기기 로그인을
// 열기 때문이고, 이 시나리오가 사는 것은 「고를 수 있고 에이전트 작업 화면으로 열린다」까지다.
import { expect, test } from "@playwright/test";

const POD_BOUND = 240_000;

type Session = { id: string; state: string; workloadType: string; model?: string };

test("codex is offered as an agent type and opens in the agent workspace", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);
  const name = `codex-1-${Date.now()}`;
  let createdId: string | null = null;

  try {
    await page.goto("/new");
    const option = page.getByTestId("new-session-workload-codex");
    await expect(option).toBeVisible();
    await expect(option).toContainText(
      "Codex SDK agent on your ChatGPT plan. Conversation, plugin skills and K3s MCP only.",
    );

    await option.click();
    await expect(option).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("new-session-model")).toBeVisible();
    await expect(
      page.getByText(
        "Workload type and model choice are fixed for this session. The first prompt signs the session in to ChatGPT, and the login is kept in its archive.",
        { exact: true },
      ),
    ).toBeVisible();

    await page.getByTestId("new-session-name").fill(name);
    await page.getByTestId("new-session-submit").click();

    await expect(page).toHaveURL(/\/agent\/[0-9a-f]+$/, { timeout: POD_BOUND });
    createdId = new URL(page.url()).pathname.split("/").pop() as string;

    await expect(page.getByTestId("ws-workload")).toContainText("codex");
    await expect(page.getByTestId("ws-prompt")).toHaveAttribute(
      "placeholder",
      "send a prompt — runs one Codex turn in the session pod",
    );
    await expect(page.getByTestId("ws-codex-login-hint")).toHaveText(
      "First prompt signs this session in to ChatGPT — watch the transcript for a link and code.",
    );
    await expect(page.getByTestId("ws-stream-status")).toBeVisible();

    const fetched = await request.get(`/api/v1/sessions/${createdId}`);
    expect(fetched.status()).toBe(200);
    const body = (await fetched.json()) as Session;
    expect(body.workloadType).toBe("codex");
    expect(["", "platform-default"]).toContain(body.model ?? "");
    expect(body.state).toBe("active");

    await page.goto("/");
    const card = page.locator(`[data-session-id="${createdId}"]`);
    await expect(card).toHaveAttribute("data-workload-type", "codex");
    await expect(card).toContainText("◇ codex");
  } finally {
    if (createdId) {
      const deleted = await request.delete(`/api/v1/sessions/${createdId}`);
      expect([204, 404]).toContain(deleted.status());
    }
  }
});
