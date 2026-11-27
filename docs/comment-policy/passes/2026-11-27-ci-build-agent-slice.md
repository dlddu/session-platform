# 판정 패스 — 빌드 파일 · control-plane 에이전트·API 축 (2026-11-27)

범위(L 31행 400줄 — **400줄 판정, 제거 39 · 유지 361**).

모델 정의의 「판정 슬라이스」 예산(400줄)을 **정확히 채웠다**. 원장 행 순서(범위의 첫 파일 경로
사전순)를 따라 덩어리를 하나씩 가져갔고, **열린 PR이 실제로 그 파일을 손대는 덩어리만** 뺐다
(`#201`·`#272`·`#282`·`#279`·`#253`·`#213`·`#192`·`#127`·`#106`·`#274`·`#275` 가 손대는 파일).
400줄이 채워진 뒤 다음 덩어리는 예산을 넘으므로 여기서 자른다.

판정은 필요성 시험으로 한다 — 각 주석에 「지우면 이 코드를 고치러 온 사람이 틀린 판단을 하거나
확인에 큰 비용을 치르는가」를 묻는다. 코드는 바꾸지 않았다(주석 삭제와 `gofmt` 정렬만).

## 제거 — 39줄

좌표는 「이 내용을 이미 말하는 곳」이다.

| 파일 | 제거한 것 | 줄 | 좌표 |
| --- | --- | ---: | --- |
| `Makefile` | 파일 머리 2줄 | 2 | `build → embed → control-plane` 타깃 그래프가 `make build` 의 결과를 이미 말한다 |
| `control-plane/Dockerfile` | 파일 머리 2줄, stage 배너 3줄, 「overlay the freshly built SPA into the embed directory」 1줄 | 6 | 3개 `FROM` 스테이지와 `COPY --from=web /web/dist ./internal/static/dist` 한 줄 |
| `control-plane/internal/adapter/agent/lane.go` | `monitorLanePathPrefix` doc 1줄, `agentLanesPath` doc 1줄, `ReadLane` doc 1줄, `StreamLane` doc 1줄 | 4 | 식별자 이름과 바로 아래 시그니처 — 이름이 이미 말하는 것을 한 번 더 옮겨 적은 것이다 |
| `control-plane/internal/adapter/agent/stub_lane.go` | `Lanes` doc 1줄 | 1 | 식별자 이름과 바로 아래 시그니처 |
| `control-plane/internal/adapter/checkpointstore/auto_archive.go` | `AutoArchiveURLLifetime` doc 2줄, `writerMetadataKey` doc 2줄, `URLBase` doc 1줄, `ArchiveWriter` doc 2줄 | 7 | 식별자 이름과 바로 아래 시그니처 — 값의 근거나 함정이 없는 이름 재진술 |
| `control-plane/internal/adapter/k8s/auto_archive.go` | `AutoArchiveRunPath` doc 1줄, `AutoArchiveBase` doc 1줄, `WithAutoArchive` doc 1줄 | 3 | 식별자 이름과 바로 아래 시그니처 |
| `control-plane/internal/api/lane_test.go` | `readLane` 테스트 헬퍼 doc 1줄 | 1 | 헬퍼 이름과 바로 아래 시그니처 |
| `control-plane/internal/api/questions_test.go` | 테스트 머리 AC 시나리오 표제 2줄 | 2 | 테스트 이름과 단언 — **주석이 전부 사라져 원장 행 삭제** |
| `control-plane/internal/api/recover_test.go` | 테스트 머리 AC 시나리오 표제 2줄, `recover` 헬퍼 doc 1줄 | 3 | 테스트 이름과 단언, 헬퍼 이름과 시그니처 |
| `control-plane/internal/api/repo_access.go` | `RepoAccessStore` doc 1줄 | 1 | 식별자 이름과 바로 아래 타입 |
| `control-plane/internal/api/repo_access_test.go` | `helpers` 테스트 헬퍼 doc 1줄 | 1 | 헬퍼 이름과 바로 아래 구현 |
| `control-plane/internal/api/steer_test.go` | `steerServer` 테스트 헬퍼 doc 1줄, 테스트 머리 AC 시나리오 표제 2줄 | 3 | 헬퍼 이름과 구현, 테스트 이름과 단언 |
| `control-plane/internal/api/tool_approvals_test.go` | 테스트 머리 AC 시나리오 표제 3줄 | 3 | 테스트 이름과 단언 — **주석이 전부 사라져 원장 행 삭제** |
| `control-plane/internal/helperauth/helperauth.go` | `New` doc 2줄 | 2 | 식별자 이름과 바로 아래 시그니처 |

**정렬만 바뀌었다.** `lane.go` 와 `k8s/auto_archive.go` 에서 const doc 을 걷어 내자 `gofmt` 이 같은
블록의 `=` 정렬을 넓혔다(`agentLanesPath = ` → `agentLanesPath        = `,
`AutoArchiveRunPath = ` → `AutoArchiveRunPath                  = `). 주석이 아닌 줄의 변경은 이
정렬 2줄뿐이고, `gofmt -l control-plane data-plane` 은 빈 출력이다.

## 유지 — 361줄 (사유)

사유는 「유지 목록에 남길 한 문장」이다. 같은 사유를 공유하는 묶음마다 한 줄씩 적었다.

| 대상 | 줄 | 필요 사유 |
| --- | ---: | --- |
| `Makefile` — `ENVTEST_DIR` 앞 1줄 (격리된 중첩 모듈) | 1 | 왜 이 디렉터리가 따로 떨어져 있는지 — 중첩 모듈이라 본 모듈에 의존성이 붙지 않는다 |
| `Makefile` — `##` 타깃 설명 전체 (`make help` 가 읽는다) | 44 | `make help` 가 읽는 타깃 설명 — 지우면 help 출력에서 사라진다 (기계가 읽는 주석, 시험 면제) |
| `control-plane/Dockerfile` — 의존성 레이어가 캐시되도록 매니페스트를 먼저 복사하는 이유 | 3 | 소스만 바뀌어도 의존성 다운로드를 다시 하지 않게 하는 순서의 근거 — 주석 없이는 `COPY` 두 줄이 왜 저 순서인지 알 수 없다 |
| `control-plane/cmd/control-plane/memory_placement.go` — `parseNodeSelector` doc | 2 | 함수 이름에 없는 env 변수 이름(`SESSION_HIGH_MEMORY_NODE_SELECTOR`)과 빈 값이 「선택자 없음」이라는 계약을 말한다 |
| `control-plane/cmd/control-plane/memory_placement.go` — `parseTolerations` doc | 3 | env 변수 이름(`SESSION_HIGH_MEMORY_TOLERATIONS`)과 kubectl 표기의 허용 형태(`key=value:Effect`·`key:Effect`) — 호출자가 값을 어디서 읽는지 |
| `control-plane/cmd/control-plane/repo_access.go` — `repoAccessDBPath` 가 설정 불가능한 이유(영속 볼륨이 없으면 기동이 실패한다) | 4 | 볼륨 미마운트 시 목록이 컨테이너 파일시스템에 조용히 남지 않고 기동이 멈춘다 — 실패 방식의 계약 |
| `control-plane/cmd/control-plane/repo_access.go` — `repoAccessConfig` 의 opt-in·AC-H1·AC-H2(화면 로그인 없이 ingress 가 문) | 4 | 기능이 꺼진 기본값과 인증 모델이 코드 형태로는 보이지 않는다 |
| `control-plane/cmd/control-plane/repo_access.go` — `dbPath` doc | 1 | 테스트에서만 다른 경로를 쓰는 이유 — 필드가 따로 있는 이유 |
| `control-plane/cmd/control-plane/repo_access.go` — `internalURL` doc (AC-H3) | 1 | 헬퍼 파드가 이 control plane 에 닿는 경로라는 사실 |
| `control-plane/cmd/control-plane/repo_access.go` — `helperURL` doc (기본값의 정본 위치) | 1 | 기본값이 어디서 오는지(`deploy/base/service.yaml`) — Service 이름의 정본 |
| `control-plane/cmd/control-plane/repo_access.go` — typed nil 이 핸들러의 nil 검사를 무력화한다는 것 | 1 | Go 의 interface 에 nil 을 넣으면 typed nil 이 되어 nil 비교가 통과한다 — Go 의 함정 |
| `control-plane/cmd/control-plane/repo_access_test.go` — 고정 경로가 마운트되지 않은 전제 | 2 | 영속되지 않을 곳에 목록을 만들지 않으려면 기동이 실패해야 한다 — 테스트의 전제 |
| `control-plane/cmd/control-plane/repo_access_test.go` — 로그인 설정이 필요 없는 이유(AC-H2) | 1 | ingress 가 문이므로 이 화면에 로그인 설정이 없다는 사실 |
| `control-plane/internal/adapter/agent/auto_archive.go` — `AutoArchiveTargetPath` doc과 `data-plane/cmd/agent` 동기화 지시 | 3 | 경로 상수가 두 파일에 복제돼 있어 한쪽만 고치면 조용히 어긋난다 — 동기화 이유 |
| `control-plane/internal/adapter/agent/auto_archive.go` — `SetAutoArchiveTarget` doc(서명이 덮는 헤더) | 2 | 서명에 얹히는 헤더가 무엇인지 — 이름에서는 드러나지 않는다 |
| `control-plane/internal/adapter/agent/auto_archive.go` — `ClearAutoArchiveTarget` doc(스냅샷 커밋 뒤 회수 시 자동 아카이브가 안 쌓이는 이유) | 2 | 회수와 스냅샷 커밋의 순서 관계가 코드에 없다 |
| `control-plane/internal/adapter/agent/lane.go` — 파일 머리(레인=경로 접두사란 모델과 새 리스너·Service 불필요의 이유 · `data-plane` 과의 동기화 지시) | 11 | 접두사가 경계 그 자체라 리스너나 Service 를 새로 만들지 않는 설계와, 접두사·재고 경로가 두 파일에 복제돼 있다는 사실 |
| `control-plane/internal/adapter/agent/lane.go` — `lanePathPrefix` doc(부모 레인의 접두사가 빈 이유) | 3 | 레인을 모르는 클라이언트가 사람 채팅에 닿아야 한다 — 빈 접두사가 계약이다 |
| `control-plane/internal/adapter/agent/lane.go` — `WriteLane` doc(큐잉이 완료가 아니다) | 2 | 쓰기가 큐에 오른 것이지 끝난 것이 아니라는 계약 |
| `control-plane/internal/adapter/agent/lane.go` — `Lanes` doc과 404 를 「미지원」으로 다루는 절 | 7 | 「이 파드는 대화를 하나만 돈다」와 「이 파드는 대답이 없다」를 구별해야 하는 이유와, 그 구별을 못 하면 클라이언트가 빈 페이지를 「대화 없음」으로 읽는다 |
| `control-plane/internal/adapter/agent/lane_test.go` — 파일 머리(테스트가 경로 배치를 고정하는 이유) | 3 | 이 테스트들이 지키는 것이 경로 배치뿐이라는 사실 — 무엇을 검증하는지가 이름에서 드러나지 않는다 |
| `control-plane/internal/adapter/agent/lane_test.go` — `lanePod` doc | 2 | 가짜가 어떤 경로를 물으면 무엇으로 답하는지 — 테스트가 무엇을 조작하는지 |
| `control-plane/internal/adapter/agent/lane_test.go` — `TestLaneCallsLandOnTheLanesOwnPath` doc | 3 | 부모 레인이 파드 자체 경로라는 사실이 계약이라는 근거 |
| `control-plane/internal/adapter/agent/lane_test.go` — 레인별 읽기가 그 레인의 대화만 본다는 것 | 1 | 다른 레인의 대화가 섞이면 안 되는 계약 |
| `control-plane/internal/adapter/agent/lane_test.go` — 레인 없는 읽기가 파드 자체 읽기와 바이트 단위로 같다는 것 | 2 | 이 호환성이 무엇에 기대는지 — 레인 기능이 갑자기 들어와도 기존 호출이 같아야 한다 |
| `control-plane/internal/adapter/agent/lane_test.go` — 파드 포트 밖으로 나가지 않는다는 것 | 2 | 레인 추가가 노출 면을 늘리지 않아야 한다 — 경계 계약 |
| `control-plane/internal/adapter/agent/lane_test.go` — `TestLanePathPrefixRefusesWhatIsNotALane` doc | 3 | 오타가 잘못된 대화의 호출이 되기 전에 막아야 한다 — 거절 위치의 이유 |
| `control-plane/internal/adapter/agent/lane_test.go` — `TestLanesComesFromThePod` doc | 2 | 재고가 파드 자신의 답이며 그런 라우트가 없는 파드는 「미지원」이다 — 두 경우의 구분 |
| `control-plane/internal/adapter/agent/lane_test.go` — 「그런 라우트 없음」이 아닌 오류는 오류로 남는다는 것 | 2 | 일대일로 돈다는 파드가 「대화 하나인 파드」로 읽히면 안 된다 — 오류 표면의 계약 |
| `control-plane/internal/adapter/agent/stub_lane.go` — 파일 머리(부모 레인의 버퍼가 파드 자체 버퍼라는 것) | 6 | 「레인 없는 호출이 늘 닿던 곳에 닿는다」를 주장이 아니라 관찰 가능하게 만드는 설계 — 스텁의 역할 |
| `control-plane/internal/adapter/agent/stub_lane.go` — `LanePaired` doc(짝 없는 스텁의 기본값) | 3 | 짝 없는 스텁은 대화 하나를 보고한다 — 짝 있는 매니저 파드만 두 레인이다 |
| `control-plane/internal/adapter/agent/stub_lane.go` — `LaneUnreachable` doc(「미지원」으로 보고하는 이유) | 3 | 그 라우트가 없는 파드와 대화가 없는 파드를 구별해야 한다 |
| `control-plane/internal/adapter/agent/stub_lane.go` — `LaneWrites` doc | 1 | 주어진 순서가 그대로 보존된다는 계약 |
| `control-plane/internal/adapter/agent/stub_lane.go` — `laneBufKey` doc(레인 없는 쓰기와 부모 레인 쓰기가 같은 대화) | 3 | 부모 레인의 버퍼가 파드 자체 버퍼라는 사실 — 실 파드와 스텁이 같아야 한다 |
| `control-plane/internal/adapter/agent/stub_lane.go` — `StreamLane` doc(두 커서가 같은 커서가 아님을 보이게 하는 이유) | 3 | 부모 레인은 `Stream` 의 이벤트 모양을 재사용하고 다른 레인은 버퍼를 준다 — 커서 구분의 이유 |
| `control-plane/internal/adapter/agent/work.go` — 파일 머리(읽기 두 개와 쓰기 하나 · 읽기에 SQL 이 없고 404 가 「비어 있음」이 아니다 · 쓰기는 세션의 다음 턴으로만 간다 · `data-plane` 과의 동기화 지시) | 16 | 읽기가 읽기만 함(세션을 바꾸지 않는다)과 쓰기가 아무것도 깨우지 않는다는 계약, 그리고 경로 상수가 두 파일에 복제돼 있다는 사실 |
| `control-plane/internal/adapter/agent/work.go` — `WorkTasks` doc | 1 | keyset 페이지 단위라는 사실이 페이징 모델의 전제 |
| `control-plane/internal/adapter/agent/work.go` — `WorkTurns` doc | 2 | 가장 오래된 것부터 읽고 `openOnly` 가 부모가 아직 필요한 턴만 좁힌다는 계약 |
| `control-plane/internal/adapter/agent/work.go` — `CreateWorkRequest` doc(201 과 200 의 구별) | 6 | 재시도한 호출이 「방금 저장」과 「이미 저장」을 구별해야 한다는 것 — 답을 못 본 재시도가 유일한 근거 |
| `control-plane/internal/adapter/agent/work.go` — 상태가 본문의 플래그보다 우선이라는 것 | 3 | 본문만 믿으면 나중의 필드 이름 변경이 모든 저장을 첫 저장으로 보고한다 — 미래의 함정 |
| `control-plane/internal/adapter/agent/work.go` — `workWriteError` doc(오류 코드가 상태와 1:1 이 아니다) | 6 | 충돌이 무엇인지에 따라 다음 행동이 갈린다 — 409 만 보고 재시도하면 곤란한 지시를 받는다 |
| `control-plane/internal/adapter/agent/work.go` — 알지 못할 코드일 때 상태로 떨어지는 이유 | 2 | 사이드카에 닿지 못한 경우에도 상태는 정해지므로 코드가 없으면 상태가 유일한 단서 |
| `control-plane/internal/adapter/archive/agent_checkpointer_test.go` — 스냅샷이 아카이브 키 하나를 덮어쓰고 세대를 기록한다는 것 | 2 | 아카이브 키가 세션당 하나라는 사실 — 복구 대상이 결정되는 기준 |
| `control-plane/internal/adapter/archive/agent_checkpointer_test.go` — 다른 writer 가 덮어써도 그 세션 소유로 보고 복구를 진행한다는 것 | 2 | 다른 세대의 아카이브가 같은 세션의 것이라 복구 대상이 된다 — 세대 검사의 이유 |
| `control-plane/internal/adapter/checkpointstore/auto_archive_test.go` — 실제 서명의 형태(path-style · 2시간 · 체크섬 없음) | 2 | 모의 서명으로 통과시키면 업로드가 성립하는지 아무것도 검증되지 않는다 — 실제 서명이 있어야 잡힌다 |
| `control-plane/internal/adapter/checkpointstore/manifest.go` — `PutManifest` doc(항상 최신 스냅샷의 아카이브를 가리킨다) | 2 | 덮어쓰기 때문에 항상 최신 것만 가리킨다 — 매니페스트가 무엇을 약속하는지 |
| `control-plane/internal/adapter/checkpointstore/manifest.go` — `GetManifest` doc(`ErrNotFound` 조건) | 2 | 한 번도 스냅샷을 커밋하지 않은 id 가 「없음」으로 오는 경계 |
| `control-plane/internal/adapter/checkpointstore/manifest.go` — `isMissingObject` doc(S3 가 403 으로도 없는 키를 답할 수 있다) | 3 | 접근 권한에 따라 없는 키가 403 로 와도 「없음」으로 통과하면 안 된다 — 외부 시스템의 문서화되지 않은 동작 |
| `control-plane/internal/helperauth/helperauth_test.go` — `reviewing` 이 토큰으로 갈라진 표에서 TokenReview 를 답한다는 것 | 1 | 이 테스트가 「실 TokenReview」 가 아니라 가짜 답을 쓴다는 단서 |
| `control-plane/internal/adapter/agent/work.go` — `readWork` doc(409 는 재시도 · 404 는 라우트와 미존재 과제 구분) | 5 | 409 는 데이터베이스 이전이 잡고 있는 중이라 실패가 아니라 재시도고, 라우트의 404 와 없는 과제의 404 를 구별해야 호출자가 「과제 없음」을 안다 |
| `control-plane/internal/adapter/checkpointstore/auto_archive_test.go` — URL 이 서명한 자격증명보다 오래 살아남으면 안 된다는 것 | 2 | 만료가 서명보다 먼저여야 사람이 서명 재사용을 못 한다 — 수명과 서명의 순서 계약 |
| `control-plane/internal/adapter/checkpointstore/auto_archive.go` — 서명만 걸었으므로 체크섬이 나중에 업로드되는 본문(지금의 빈 요청 본문)을 덮는다 | 2 | 체크섬을 안 거는 이유 — 나중에 파드가 올리는 아카이브 본문이 아니라 지금의 빈 본문을 덮기 때문 |
| `control-plane/internal/adapter/checkpointstore/auto_archive.go` — `ArchiveRef` doc(스냅샷과 자동 아카이브가 같은 키를 덮어쓴다) | 2 | 한 세션의 아카이브 키는 하나이고 스냅샷과 파드의 자동 아카이브가 그 키를 덮어쓴다 — 복구 대상이 결정되는 기준 |
| `control-plane/internal/adapter/checkpointstore/auto_archive.go` — `AutoArchiveTarget` doc(허용 대상이 그 키 하나뿐 · 업로드가 특정 헤더를 실어야 한다) | 4 | 서명된 URL이 무엇에나 쓰이면 안 되고 그 세션의 아카이브 키로만, 그리고 특정 헤더를 실어 서야만 한다 — 둘 다 계약 |
| `control-plane/internal/adapter/checkpointstore/auto_archive.go` — 캐시가 실제 만료보다 일찍 `Expires` 를 보고한다는 것 | 1 | 캐시가 서명보다 먼저 죽는 창이 남아 있다는 사실 — 그 창이 크기의 근거 |
| `control-plane/internal/adapter/checkpointstore/auto_archive.go` — `AutoArchiveBase` doc(서명 없는 URL이 파드가 받아들이는 유일한 목적지) | 2 | 파드가 받아들이는 목적지는 이것 하나뿐이라는 계약 — 다른 주소를 넣으면 업로드가 없다 |
| `control-plane/internal/adapter/k8s/auto_archive.go` — `AutoArchiveURLBaseEnvVar` doc(파드가 받아들이는 업로드 목적지는 하나뿐) | 3 | 자동 아카이브 URL의 목적지가 하나뿐이라는 사실 — 두 개를 넣으면 어느 쪽인지 애매해진다 |
| `control-plane/internal/adapter/k8s/auto_archive.go` — `AutoArchiveGracePeriodSeconds` doc(아카이브와 업로드, spot 방해 2분) | 2 | 유예 시간의 구성(아카이브와 업로드가 그 안으로 들어와야 하고 spot 방해 2분이 남는다) — 숫자의 근거 |
| `control-plane/internal/adapter/k8s/auto_archive.go` — `AutoArchiveHoldPath` doc(헬퍼 파드가 워크로드 파드의 업로드가 끝날 때까지 중계한다) | 2 | 게이트된 세션에서는 헬퍼 파드가 중계 경로로 남아 있어야 한다 — 경유가 사라지면 업로드가 끊긴다 |
| `control-plane/internal/adapter/k8s/auto_archive.go` — `applyAutoArchive` doc(훅이 exec 인 이유) | 3 | preStop 훅이 httpGet 이면 게이트된 헬퍼 파드가 kubelet 트래픽을 받지 못해 호출되지 않는다 — 이유가 코드에 없다 |
| `control-plane/internal/adapter/k8s/drop.go` — 파일 머리(drop 파일 전달의 위치와 경계) | 2 | drop 이 어느 파드에서 닿는지(헬퍼 파드의 세션 MCP) — 워크로드 파드는 닿지 않는다 |
| `control-plane/internal/adapter/k8s/drop.go` — `data-plane/cmd/agent` 와의 동기화 지시 | 1 | env 이름이 두 파일에 복제돼 있어 한쪽만 고치면 조용히 어긋난다 |
| `control-plane/internal/adapter/k8s/drop.go` — `DropTokenAudience` doc | 2 | audience 가 apiserver 기본 audience 가 아니므로 이 토큰으로 apiserver 를 부를 수 없다 — 보안 계약 |
| `control-plane/internal/adapter/k8s/drop.go` — `WithDropURL` doc(빈 값이면 기능이 꺼진다) | 2 | 주소를 비우면 토큰이 투영되지 않아 drop 도구가 도구 목록에 오르지 않는다 — 꺼짐의 방식 |
| `control-plane/internal/adapter/k8s/drop.go` — `addDrop` doc(세션 MCP 컨테이너에만 토큰이 들어간다) | 1 | 토큰이 들어가는 컨테이너가 하나뿐이라는 사실 — 권한의 경계 |
| `control-plane/internal/adapter/k8s/memory_tier.go` — `scaledMemory` doc(tier 1 미만은 1로 센다) | 1 | 계산 클램프가 코드에 적혀 있지 않다 — 0이나 음수가 들어오면 그대로 곱해진다 |
| `control-plane/internal/adapter/k8s/memory_tier.go` — `WithHighMemoryPlacement` doc(배치 계약 · 헬퍼 파드는 옮기지 않는다) | 6 | 어느 노드에 큰 요청을 둘지·상/interruptible 을 피할지는 클러스터의 판단이고, 헬퍼 파드는 세션 상태가 없어 옮기지 않는다 — 두 결정의 근거 |
| `control-plane/internal/adapter/k8s/repo_access.go` — 파일 머리(레포 접근이 어디까지 닿는지) | 5 | 허용목록 조회·요청 등록·클론 토큰 발급의 주체가 서로 다르고 워크로드 파드까지는 닿지 않는다 — 경계 |
| `control-plane/internal/adapter/k8s/repo_access.go` — `data-plane/cmd/agent` 와의 동기화 지시 | 2 | env 이름이 두 파일에 복제돼 있어 한쪽만 고치면 조용히 어긋난다 |
| `control-plane/internal/adapter/k8s/repo_access.go` — `RepoAccessTokenAudience` doc | 3 | control plane 의 helperauth audience 와 같아야 하고 apiserver 기본 audience 가 아니므로 재사용되지 않는다 — 보안 계약 |
| `control-plane/internal/adapter/k8s/repo_access.go` — `WithRepoAccess` doc(빈 값이면 기능이 꺼진다) | 3 | 주소를 비우면 토큰이 투영되지 않아 헬퍼 파드가 레포 접근 API를 못 쓴다 — 꺼짐의 방식 |
| `control-plane/internal/adapter/k8s/repo_access.go` — `addRepoAccess` doc(루프백 주소) | 2 | 세션 MCP 컨테이너에 주는 주소가 자기 파드 안의 loopback 이라는 사실 — 밖으로 나가지 않는다 |
| `control-plane/internal/adapter/k8s/session_db_test.go` — AC-N6 raw SQL 옵트인의 경로 | 2 | 디버그 옵트인이 배포 ConfigMap 에서만 옵션으로 들어오고 세션·워크로드 컨테이너로는 절대 오지 않는다 — 경계 |
| `control-plane/internal/adapter/k8s/session_db_test.go` — AC-N11 사이드카가 깨우는 주소 | 5 | 깨우는 주소는 이 파드의 loopback 이고 레인은 경로 접두사라 리스너가 하나뿐이다 — 옆 대화만 깨울 수 있다 |
| `control-plane/internal/adapter/k8s/session_db_test.go` — 모니터 대화는 워크로드 컨테이너의 일이라는 것 | 2 | 사이드카가 그 위치를 알면 건드리지 말아야 할 트리에 쓸 수 있다 — 권한의 근거 |
| `control-plane/internal/adapter/k8s/session_db_test.go` — 두 번째 레인이 없는 타입은 깨울 주소가 없다는 것 | 1 | 주소가 없다는 것과 깨울 대화가 없다는 것이 같은 결과가 아니다 |
| `control-plane/internal/adapter/k8s/shell_security.go` — `ShellUserID` doc | 3 | `data-plane/Dockerfile` 이 만드는 `session` 계정과 같아야 `id`·프롬프트·git 이 이름 있는 사용자로 보인다 |
| `control-plane/internal/adapter/k8s/shell_security.go` — `shellSecurityContext` doc(루트 파일시스템을 쓰기 가능하게 둔다) | 4 | 홈은 state EmptyDir 에 있지만 평범한 셸 사용이 /tmp·/var/tmp 에 쓰고, root 없이 쓰기 가능한 이미지 레이어로 시스템을 바꿀 수 없게 하려는 의도 |
| `control-plane/internal/adapter/k8s/shell_security.go` — `workloadPodSecurityContext` doc(fsGroup) | 3 | state EmptyDir 와 그것을 이름 바꾸는 복구가 셸 사용자의 그룹 소유가 되어야 한다 |
| `control-plane/internal/api/lane.go` — 파일 머리(두 대화가 하나의 세션이라는 모델) | 13 | 세션 id 가 둘이고 라우트가 둘인 모델이 아니라 하나의 세션의 매개변수라는 설계 — 옛 클라이언트가 그대로 도달해야 한다 |
| `control-plane/internal/api/lane.go` — `laneParam` doc(쿼리 파라미터·본문 필드인 이유) | 3 | 레인은 경로 조각이 아니라 각 라우트가 이미 커서를 실던 자리에 실린다 — 경로 설계의 이유 |
| `control-plane/internal/api/lane.go` — `requestLane` doc(부재하면 채팅 · 매니저가 아닌 레인은 거부) | 3 | 기본값으로 떨어뜨리지 않고 거부한다는 것 — 오타가 조용히 사람 채팅을 읽으면 안 된다 |
| `control-plane/internal/api/lane.go` — `listSessionLanes` doc(수동적 · 422) | 6 | 재고 조회가 스냅샷을 복원하거나 접근으로 세지 않아야 하며 얼린 세션은 깨지 않고 422 로 답한다 |
| `control-plane/internal/api/lane_test.go` — 파일 머리(HTTP 경유로 도는 이유) | 4 | 어느 대화로 도달했는지와 무엇이 거절되는지는 진짜 라우트에서만 참이다 — 서비스 단위로는 증명이 안 된다 |
| `control-plane/internal/api/lane_test.go` — `newManagerSession` doc | 2 | 두 번째 대화가 있는 유일한 모양이 짝 있는 매니저라는 사실 |
| `control-plane/internal/api/lane_test.go` — `TestLanesRouteSaysThePairIsInOnePod` doc | 3 | 세션 밖의 독자가 확인할 수 있는 유일한 사실 — control plane 의 말을 믿지 않고 직접 본다 |
| `control-plane/internal/api/lane_test.go` — 대화 하나인 워크로드의 답 | 2 | 대화가 하나인 것과 없는 것은 다르다 — 채팅은 거기 있다 |
| `control-plane/internal/api/lane_test.go` — 그러한 라우트가 없는 파드(짝 이전에 뜬 파드) | 2 | 빈 목록이 아니라 미지원으로 보고되어야 한다 |
| `control-plane/internal/api/lane_test.go` — `TestLanesRouteIsPassive` doc | 3 | 재고를 물은 것이 파드를 깨우면 안 된다 — 얼린 세션을 깨우는 쪽이 되어서는 안 된다 |
| `control-plane/internal/api/lane_test.go` — 그대로 스냅샷이었다는 것 | 1 | 읽기가 복원을 일으키지 않았다는 확인 |
| `control-plane/internal/api/lane_test.go` — `TestWriteReachesTheLaneItNames` doc | 3 | 진입점에서의 분리 — 이름 없는 쓰기는 사람 채팅으로, 모니터를 이름한 쓰기는 모니터로 |
| `control-plane/internal/api/lane_test.go` — 부모 레인의 읽기가 두 부모 쓰기를 모두 본다는 것 | 2 | 부모 레인의 대화가 파드 자체의 대화라는 사실 |
| `control-plane/internal/api/lane_test.go` — `TestLaneCursorsAreSeparate` doc | 3 | 레인마다 출력 버퍼가 따로라 한 레인의 오프셋이 다른 레인에서 무의미하다 |
| `control-plane/internal/api/lane_test.go` — 모니터를 채팅의 커서로 읽으면 엉뚱한 이유로 비어 보이면 안 된다 | 2 | 빈 결과의 원인이 커서 밀림인지 실제 비어 있음인지 구분해야 한다 |
| `control-plane/internal/api/lane_test.go` — `TestLaneIsRefusedRatherThanDefaulted` doc | 2 | 오타가 조용히 사람 채팅을 읽으면 클라이언트가 모니터를 보고 있다고 믿는다 |
| `control-plane/internal/api/lane_test.go` — 빈 레인은 거부가 아니라 채팅이라는 것 | 3 | 짝 이전에 쓰인 모든 클라이언트가 그대로 동작하는 이유 — 공백 하나도 이름 없는 레인으로 읽는다 |
| `control-plane/internal/api/lane_test.go` — `TestLaneWriteAndToolApprovalsAreMutuallyExclusive` doc | 3 | 둘은 매니저의 선택이고 다른 쪽은 claude-code 의 선택이라 함께 주면 조용히 하나를 버리지 않고 거절한다 |
| `control-plane/internal/api/lane_test.go` — 어느 대화에도 쓰이지 않았다는 것 | 1 | 거절된 쓰기가 다른 대화로 새지 않았다는 확인 |
| `control-plane/internal/api/lane_test.go` — `TestMonitorLaneIsOnlyAManagers` doc | 3 | 두 번째 대화는 매니저의 모양이다 — 대화 하나인 워크로드에 이름을 붙이면 미지원이다 |
| `control-plane/internal/api/lane_test.go` — 자기 채팅은 그대로 쓴다는 것 | 1 | 미지원이 되어도 기존 채팅 경로는 그대로 동작한다는 확인 |
| `control-plane/internal/api/lane_test.go` — `podOf` doc(세션 이름으로 추측하지 않는다) | 2 | 파드는 재고를 보고하는 라우트로 읽는다 — 이름에서 추측하면 다른 파드를 가리킬 수 있다 |
| `control-plane/internal/api/recover_test.go` — AC-B4 with AC-K3 (복구가 파드를 띄우므로 워크로드 타입이 필요하다) | 2 | 복구에는 세션의 워크로드 타입이 소유자에게 여전히 허용돼 있어야 한다 — 테스트가 그 권한을 세우는 이유 |
| `control-plane/internal/api/repo_access.go` — `RepoAccess` doc(인증 모델 · nil store 면 503) | 5 | 관리 라우트에 자체 로그인이 없고 ingress 의 forward-auth 가 문이며, store 가 없으면 기능이 꺼져 모든 라우트가 503 이다 — 세 계약 |
| `control-plane/internal/api/repo_access.go` — `sessionLabels` doc(best effort) | 2 | 요청이 세션보다 오래 살아서 라벨 조회가 실패해도 화면의 예쁜 이름만 잃는다 — 실패의 허용 범위 |
| `control-plane/internal/api/repo_access_test.go` — 관리 라우트에 자체 로그인이 없다는 것 | 2 | ingress 의 forward-auth 가 문이라 테스트가 로그인 설정을 두지 않는다 |
| `control-plane/internal/api/repo_access_test.go` — 파일 요청에도 헬퍼 토큰이 필요하다는 것 | 1 | 요청 등록 경로도 헬퍼 토큰을 요구한다 — 테스트가 그 전제를 세우는 이유 |
| `control-plane/internal/api/steer_test.go` — 거절은 409 로 전달되고 «전달됨» 으로 보고되지 않는다 | 2 | 거절은 호출자가 다룰 수 있는 거부여야 하고 전달로 보고되면 안 된다 |
| `control-plane/internal/api/steer_test.go` — free-text 중간 메시지를 받는 것은 codex 워크로드뿐 | 2 | claude-code 의 중간 표면은 질문과 도구 승인이다 — 다른 타입에 이 라우트를 붙이면 안 되는 이유 |
| `control-plane/internal/api/steer_test.go` — `ErrInvalidState` 의 뜻 | 1 | 세션은 존재하지만 그 타입에 그 표면이 없다는 것을 어떻게 읽는지 |
| `control-plane/internal/helperauth/helperauth.go` — 패키지 doc | 4 | 이 패키지가 누구의 것인지 — 투영된 SA 토큰과 TokenReview 이 증명하는 것 |
| `control-plane/internal/helperauth/helperauth.go` — `SessionID` doc(의심은 ErrUnauthenticated) | 3 | 어떤 의심이든 인증 실패로 접고, 다른 오류는 인프라 장애다 — 오류의 경계 |
| `control-plane/internal/helperauth/helperauth.go` — `sessionOfPod` doc(UID 로 묶인다) | 2 | 같은 이름으로 교체된 파드의 토큰을 거부한다 — 토큰이 묶인 것은 UID 다 |
| `control-plane/internal/session/memory_test.go` — boolean `doubleMemory` 로 쓰인 매니페스트가 tier 2 로 복구된다 | 2 | 과거 형식이 지금의 등급으로 읽히는 경로 — 없으면 옛 세션이 복원되지 않는다 |
| `control-plane/internal/session/memory_test.go` — `SetMemoryTier` 는 tier 1 을 생략된 기본값으로 저장한다 | 1 | tier 1 이 매니페스트에 안 적히는 것 — 생략이 기본값이라는 규칙 |
## 판단이 갈린 것

없다. 이번 범위의 361줄은 모두 위 유지 목록에 사유 한 문장을 적었다.

## 남는 것

**이 범위 밖이다.** L 미판정 잔여와 D 표면 4행 168줄, E 표면 46행 132줄은 원장에 `—`로 남아
다음 판정 슬라이스가 가져간다. 이번 패스는 400줄 예산을 채웠으므로 그 다음 덩어리는 정의
「판정 슬라이스」의 다음 몫이다.
