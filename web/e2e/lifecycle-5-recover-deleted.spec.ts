// 검증 시나리오: lifecycle.md#시나리오 5
//
// docs/prd/lifecycle.md AC-B4, docs/test/lifecycle.md 시나리오 5. 테넌트 범위·롤백 분기는
// 단위 테스트의 몫이다(시나리오 5 의 「단위 수준 보조 검증」).
//
// 배포 SUT 의 아카이브 저장소는 MinIO(실물 S3 구현)라, 매니페스트 쓰기·읽기와 없는 키의 404 가
// 운영과 같은 SDK 경로를 탄다.
import { expect, test } from "@playwright/test";

const POD_BOUND = 90_000;

type Session = {
  id: string;
  name: string;
  state: string;
  pod: string;
};

test("a deleted session comes back under its id from its last snapshot", async ({ request }) => {
  test.setTimeout(420_000);

  const name = `lifecycle-5-${Date.now()}`;
  const createResponse = await request.post("/api/v1/sessions", {
    data: { name },
    timeout: POD_BOUND,
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as Session;
  expect(created.pod).not.toBe("");

  // 터미널 에코는 명령 줄 자체도 누적 출력에 남긴다 — 산술 확장을 거친 결과만 찾아야
  // 「명령이 실제로 돌았다」를 단언하게 된다.
  const stamp = String(Date.now());
  const marker = `rcv-${stamp}-42`;
  const writeResponse = await request.post(`/api/v1/sessions/${created.id}/write`, {
    data: { payload: `echo rcv-${stamp}-$((40+2))\n` },
    timeout: POD_BOUND,
  });
  expect(writeResponse.status()).toBe(200);
  await expect
    .poll(
      async () => {
        const read = await request.post(`/api/v1/sessions/${created.id}/read`, {
          data: { offset: 0 },
          timeout: POD_BOUND,
        });
        return ((await read.json()) as { payload: string }).payload;
      },
      { timeout: 60_000 },
    )
    .toContain(marker);

  const snapshotResponse = await request.post(`/api/v1/sessions/${created.id}/snapshot`, {
    timeout: POD_BOUND,
  });
  expect(snapshotResponse.status()).toBe(200);
  expect(((await snapshotResponse.json()) as Session).state).toBe("snapshot");

  expect((await request.delete(`/api/v1/sessions/${created.id}`)).status()).toBe(204);
  expect((await request.get(`/api/v1/sessions/${created.id}`)).status()).toBe(404);

  const recoverResponse = await request.post(`/api/v1/sessions/${created.id}/recover`, {
    timeout: 180_000,
  });
  expect(recoverResponse.status()).toBe(200);
  const recovered = (await recoverResponse.json()) as Session;
  expect(recovered.id).toBe(created.id);
  expect(recovered.name).toBe(name);
  expect(recovered.state).toBe("active");
  expect(recovered.pod).not.toBe("");
  expect(recovered.pod).not.toBe(created.pod);

  const after = (await (await request.get(`/api/v1/sessions/${created.id}`)).json()) as Session;
  expect(after.state).toBe("active");
  expect(after.pod).toBe(recovered.pod);

  const replay = await request.post(`/api/v1/sessions/${created.id}/read`, {
    data: { offset: 0 },
    timeout: POD_BOUND,
  });
  expect(replay.status()).toBe(200);
  expect(((await replay.json()) as { payload: string }).payload).toContain(marker);

  expect((await request.post(`/api/v1/sessions/${created.id}/recover`)).status()).toBe(409);
  expect(
    (await request.post("/api/v1/sessions/0123456789abcdef0123456789abcdef/recover")).status(),
  ).toBe(404);

  expect((await request.delete(`/api/v1/sessions/${created.id}`)).status()).toBe(204);
});
