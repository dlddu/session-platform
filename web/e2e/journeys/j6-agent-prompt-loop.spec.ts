// 매칭 단위 밖 (AC ↔ e2e 1:1): 이 디렉터리의 여정 spec 은 web/e2e 최상위가 아니므로 AC
// 매칭 단위가 아니다. 각 AC 의 주검증은 Go e2e 의 전용 파일이 소유한다
// (AC-E1 `e2e_e1_workload_type_test.go`, AC-E2 `e2e_e2_prompt_invocation_test.go`).
// 등재: docs/test/e2e.md.
import { expect, test } from "@playwright/test";

// JRN-agent-prompt-loop — 브라우저에서 claude-code 세션에 프롬프트를 보내고 응답을 따라간다.
//
// 여기에는 네트워크 인터셉트가 **하나도 없다**. 세션은 제품 API 로 배포 SUT 에 실제로
// 만들어지고, 프롬프트는 세션 pod 안에서 실 `claude` 프로세스를 기동시키며, 응답은
// 인클러스터 provider 대역(CLAUDE-PROVIDER 등재)이 낸다. 세션 목록·상태·pod·아카이브는
// 전부 배포된 control-plane 의 실 응답이다 — docs/test/e2e.md 「e2e 충실도 허용목록」.
//
// 출력 타이밍은 비결정적이므로(에이전트 기동, CLI 콜드 스타트, SSE 전달) 모든 단언은
// 넉넉한 타임아웃의 containment 다 — 정확 일치가 아니다.

// 인클러스터 provider 대역만이 낼 수 있는 마커. 트리 어디에도 이 문자열이 없으므로,
// 이것이 콘솔에 뜨면 그 바이트가 배포된 그 대역에서 왔다는 뜻이다
// (deploy/e2e-anthropic-fake.yaml 의 `REPLY`; Go 쪽 소유자는 AC-E2 전용 파일).
const PROVIDER_REPLY = "session-platform-e2e-provider-ok";

// 배포 오버레이가 SUT 에 심는 모델 카탈로그의 두 번째 항목
// (deploy/claude-code-credentials-secret.yaml 의 `models`).
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
    // claude-code pod 는 에이전트가 뜨기 전에 플러그인을 설치하므로 기본 타임아웃보다
    // 한참 느리다.
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

  // 제출한 프롬프트는 즉시 콘솔에 반향된다(로컬 에코). 그 뒤 실 invocation 의 응답이
  // 세션의 append-only 출력에 실려 스트림으로 도착한다.
  await expect(log).toContainText("▸ j6 prompt one");
  await expect(log).toContainText(PROVIDER_REPLY, { timeout: 180_000 });

  // 명시적 커서 read 는 복구 경로이지 정상 경로가 아니다 — 위 출력은 클릭 없이 왔다.
  // 그리고 read 는 비파괴다(AC-D3): 눌러도 이미 보고 있던 이력이 사라지지 않는다.
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

  // 제품 아카이브 트리거. 실 아카이브는 워크스페이스를 인클러스터 MinIO 로 올리고 pod 를
  // 회수한 뒤에야 응답하므로 기본 타임아웃보다 한참 느리다.
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

  // 동결된 세션의 워크스페이스로 들어가면 SPA 는 Restore 로 넘긴다 — 그리고 그 관찰은
  // 수동적이다: 화면을 열었다는 이유만으로 아카이브가 되살아나지 않는다.
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
  // claude-code 세션의 동결은 CRIU 프로세스 이미지가 아니라 워크스페이스 아카이브다.
  await expect(page.getByText("No CRIU checkpoint is used.")).toBeVisible();
  await expect(page.getByText(/archive s3:\/\//)).toBeVisible();

  await page.getByTestId("restore-submit").click();
  await expect(page).toHaveURL(new RegExp("/agent/" + created.id + "$"), {
    timeout: 240_000,
  });
  await expect(page.getByTestId("ws-workload")).toContainText("claude-code");

  // 그라운드 트루스는 픽스처가 아니라 배포된 control-plane 이다: 세션이 정말 다시 서고
  // 새 pod 를 얻었다.
  const after = await request.get("/api/v1/sessions/" + created.id);
  expect(after.status()).toBe(200);
  const restored = (await after.json()) as { state: string; pod?: string };
  expect(restored.state).toBe("active");
  expect(restored.pod ?? "").not.toBe("");
});
