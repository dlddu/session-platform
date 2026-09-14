// 검증 시나리오: lifecycle.md#시나리오 2
//
// docs/prd/lifecycle.md, docs/test/lifecycle.md 시나리오 2. 복원이 타는 CRIU 왕복
// 자체는 docs/criu-verification.md.
//
// 이 파일은 2026-09-12 축 개정(#132) 이후 **처음 서는 매칭 단위**다. 그 전까지 이 시나리오를
// 검증하던 것은 `control-plane/test/e2e_b2_snapshot_restore_test.go` 였고, 축 개정으로 Go 가
// 매칭 공간에서 빠지면서 유예 표의 「Playwright 이관 미완료」 행이 됐다. 같은 PR 이 그 Go 파일을
// 지운다 — 두 스위트가 같은 시나리오를 동시에 들고 있는 창을 남기지 않는 것이 그 행의 해제
// 조건이다.
//
// 브라우저를 쓰지 않고 `request` 픽스처만 쓰는 이유: 이 시나리오의 기대 결과는 **상태 전이와
// pod 교체**이고 둘 다 control-plane 의 공개 API 표면에 그대로 나온다(`state`·`pod` 는
// control-plane/api/openapi.yaml 의 세션 표현 필드다). 화면을 경유하면 SPA 의 렌더 타이밍이
// 판정에 섞여 들어올 뿐 더 사는 것이 없다.
import { expect, test } from "@playwright/test";

// 생성과 복원은 실 pod 프로비저닝(스케줄링·이미지·CRIU restore)에 블록되므로 기본 타임아웃으로는
// 모자란다. 90초는 옮겨 오기 전 Go 하네스가 쓰던 것과 같은 예산이다
// (control-plane/test/harness_shared_test.go 의 http.Client Timeout).
const POD_BOUND = 90_000;

type Session = {
  id: string;
  name: string;
  state: string;
  pod: string;
};

test("accessing a snapshot session restores it into a new pod", async ({ request }) => {
  test.setTimeout(300_000);

  const name = `lifecycle-2-${Date.now()}`;

  const createResponse = await request.post("/api/v1/sessions", {
    data: { name },
    timeout: POD_BOUND,
  });
  expect(createResponse.status()).toBe(201);
  const created = (await createResponse.json()) as Session;
  // 사전 조건이 실제로 성립했다는 대조 — pod 가 없으면 아래의 「새 pod」 단언이 vacuous 해진다.
  expect(created.pod).not.toBe("");

  // 옮겨 오기 전 Go 파일은 여기서 404 를 만나면 skip 했다(제품 snapshot endpoint 이전의 SUT 를
  // 가리킬 수 있어서다). 배포 SUT 는 그 endpoint 를 항상 갖고 deploy/ 오버레이가 CRIU 게이트를
  // 켜므로, 이관하면서 그 분기를 **단언으로 바꾼다** — skip 은 초록으로 보이지만 아무것도 사지
  // 않는다.
  const snapshotResponse = await request.post(
    `/api/v1/sessions/${created.id}/snapshot`,
    { timeout: POD_BOUND },
  );
  expect(snapshotResponse.status()).toBe(200);
  const frozen = (await snapshotResponse.json()) as Session;
  expect(frozen.state).toBe("snapshot");

  // 시나리오가 지정하는 복원 트리거는 **명시적 접근**이다(passive stream 은 제외).
  const switchResponse = await request.post(
    `/api/v1/sessions/${created.id}/switch`,
    { timeout: POD_BOUND },
  );
  expect(switchResponse.status()).toBe(200);
  const restored = (await switchResponse.json()) as Session;

  expect(restored.state).toBe("active");
  expect(restored.pod).not.toBe("");
  // 동결이 pod 를 회수했으므로 복원은 **새** pod 를 세워야 한다. 같은 이름이 돌아오면 동결이
  // 회수하지 않았거나 복원이 기존 pod 를 재사용한 것이고, 둘 다 이 시나리오가 막는 것이다.
  expect(restored.pod).not.toBe(created.pod);

  // 응답 하나만 보면 전이가 durable 한지 알 수 없다 — 뒤이은 조회가 같은 것을 말해야 한다.
  const afterResponse = await request.get(`/api/v1/sessions/${created.id}`);
  expect(afterResponse.status()).toBe(200);
  const after = (await afterResponse.json()) as Session;
  expect(after.state).toBe("active");
  expect(after.pod).toBe(restored.pod);
});
