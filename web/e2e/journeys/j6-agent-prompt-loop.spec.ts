// 매칭 단위 밖 (AC ↔ e2e 1:1) — 등재: docs/test/e2e.md.
import { expect, test } from "@playwright/test";

// JRN-agent-prompt-loop — docs/user-journeys/JRN-agent-prompt-loop.md.
// 인터셉트 0 — 이 파일이 무엇을 실물로 밟는지는 CLAUDE-PROVIDER 등재가 갖는다.
//
// 출력 타이밍은 비결정적이므로(에이전트 기동, CLI 콜드 스타트, SSE 전달) 모든 단언은
// 넉넉한 타임아웃의 containment 다 — 정확 일치가 아니다.

// 트리 어디에도 이 문자열이 없으므로, 콘솔에 뜨면 그 바이트는 배포된 대역에서 온 것이다
// (deploy/e2e-anthropic-fake.yaml).
const PROVIDER_REPLY = "session-platform-e2e-provider-ok";

// 배포 오버레이가 심는 카탈로그의 둘째 항목(deploy/claude-code-credentials-secret.yaml).
const ALTERNATE_MODEL = "claude-e2e-alternate";

type CreatedSession = {
  id: string;
  state: string;
  workloadType: string;
  model?: string;
};

async function createAgentSession(
  request: import("@playwright/test").APIRequestContext,
  name: string,
  model?: string,
): Promise<CreatedSession> {
  const response = await request.post("/api/v1/sessions", {
    data: {
      name,
      workloadType: "claude-code",
      ...(model ? { model } : {}),
    },
    // 플러그인 설치가 선행하므로(PLUGIN-CRED) 기본 타임아웃으로는 모자란다.
    timeout: 180_000,
  });
  expect(response.status()).toBe(201);
  const created = (await response.json()) as CreatedSession;
  expect(created.workloadType).toBe("claude-code");
  expect(created.state).toBe("active");
  return created;
}

test("a prompt sent from the workspace runs on the deployed SUT and its reply lands in the output", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const created = await createAgentSession(
    request,
    "j6-prompt-" + Date.now(),
    ALTERNATE_MODEL,
  );

  await page.goto("/agent/" + created.id);
  await expect(page.getByTestId("ws-model")).toHaveText(
    "model=" + ALTERNATE_MODEL,
  );
  await expect(page.getByTestId("ws-workload")).toContainText("claude-code");
  await expect(page.getByTestId("ws-state")).toHaveText("active");

  const log = page.getByTestId("ws-log");
  const prompt = page.getByTestId("ws-prompt");
  await prompt.fill("j6 prompt one");
  await prompt.press("Enter");

  await expect(log).toContainText("▸ j6 prompt one");
  await expect(log).toContainText(PROVIDER_REPLY, { timeout: 180_000 });

  // 위 출력은 클릭 없이 왔다 — 아래 read 는 복구 경로이지 정상 경로가 아니다(AC-D3 비파괴).
  await page.getByTestId("ws-refresh-output").click();
  await expect(log).toContainText(PROVIDER_REPLY);
  await expect(log).toContainText("▸ j6 prompt one");
});

test("an archived claude-code session is described as an archive and restored from it", async ({
  page,
  request,
}) => {
  test.setTimeout(300_000);

  const created = await createAgentSession(request, "j6-archive-" + Date.now());

  // 실 아카이브가 MinIO 업로드와 pod 회수를 마친 뒤에야 응답하므로 타임아웃을 늘린다.
  const snapshotResponse = await request.post(
    "/api/v1/sessions/" + created.id + "/snapshot",
    { timeout: 240_000 },
  );
  expect(snapshotResponse.status()).toBe(200);
  const frozen = (await snapshotResponse.json()) as {
    state: string;
    pod?: string;
    checkpoint?: { ref?: string };
  };
  expect(frozen.state).toBe("snapshot");
  expect(frozen.pod ?? "").toBe("");
  expect(frozen.checkpoint?.ref ?? "").not.toBe("");

  await page.goto("/agent/" + created.id);
  await expect(page).toHaveURL(new RegExp("/restore/" + created.id + "$"), {
    timeout: 30_000,
  });
  const stillFrozen = await request.get("/api/v1/sessions/" + created.id);
  expect(stillFrozen.status()).toBe(200);
  expect(((await stillFrozen.json()) as { state: string }).state).toBe(
    "snapshot",
  );

  await expect(
    page.getByRole("heading", { name: "Resume from session archive" }),
  ).toBeVisible();
  await expect(page.getByText("No CRIU checkpoint is used.")).toBeVisible();
  await expect(page.getByText(/archive s3:\/\//)).toBeVisible();

  await page.getByTestId("restore-submit").click();
  await expect(page).toHaveURL(new RegExp("/agent/" + created.id + "$"), {
    timeout: 240_000,
  });
  await expect(page.getByTestId("ws-workload")).toContainText("claude-code");

  const after = await request.get("/api/v1/sessions/" + created.id);
  expect(after.status()).toBe(200);
  const restored = (await after.json()) as { state: string; pod?: string };
  expect(restored.state).toBe("active");
  expect(restored.pod ?? "").not.toBe("");
});
