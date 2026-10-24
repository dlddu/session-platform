# 테스트 문서: 비공개 레포 접근 — 허용 목록과 접근 요청

## 검증 대상 AC
- AC-H1: 허용 목록 저장 — control plane의 SQLite, 단일 replica opt-in (PRD: 비공개 레포 접근)
- AC-H2: 관리 API와 화면 — 배포의 인그레스 인증(Authelia forward-auth) 뒤에서 (PRD: 비공개 레포 접근)
- AC-H3: 헬퍼 파드 → control plane 내부 API — 서비스 어카운트 토큰으로 신원 확인 (PRD: 비공개 레포 접근)
- AC-H4: 접근 요청 도구 — 승인 게이트 대신 control plane의 사람 결정 (PRD: 비공개 레포 접근)
- AC-H5: 비공개 `git_clone` — 허용 목록 + 헬퍼 파드가 발급한 단명 토큰 (PRD: 비공개 레포 접근)

> ⚠️ **e2e 범위 주의**: 이 기능은 kind e2e SUT에서 **꺼져 있다** — SUT의 control plane은 replica 2개로 돌고,
> AC-H1의 SQLite 저장소는 replica 1개 배포에서만 켠다. 그래서 아래 시나리오는 전부 `e2e.md`의
> 「유예」 표에 올라 있고, 현재 검증은 단위·통합 테스트(저장소·TokenReview·핸들러·헬퍼 파드 모양)와
> 운영 배포 확인이다. 해제 조건은 각 행에 적혀 있다.

## 테스트 시나리오

### 시나리오 1: 저장소 opt-in과 영속
- **사전 조건**: control plane 두 벌 — `REPO_ACCESS_ENABLED`가 없는 배포(A), 그것을 켜고 고정 경로에 영속 볼륨을 마운트한 replica 1개·`Recreate` 배포(B)
- **실행 단계**: (a) A에서 관리 API·내부 API를 호출 → (b) A에서 `approval-gated` 세션을 만들고 헬퍼 파드를 확인 → (c) B에서 `DLDDU/Repo.git`과 `dlddu/repo`를 차례로 추가 → (d) B의 control plane 파드를 재시작한 뒤 목록 조회
- **기대 결과**: (a) 모두 `503`. (b) 헬퍼 파드에 control plane audience 토큰·`REPO_ACCESS_URL`이 없고, `approval-gated` 헬퍼 파드에 K3s MCP 프록시가 없다(이 기능 이전의 모양 그대로). 세션 MCP 도구 목록에 접근 요청 도구가 없다. (c) 첫 추가는 `201`, 두 번째는 `200`이며 항목은 `dlddu/repo` 하나다. (d) 항목이 그대로 남아 있다
- **검증 AC**: AC-H1

### 시나리오 2: 관리 API는 세션 API와 같은 문(인그레스 forward-auth) 하나 뒤에
- **사전 조건**: 기능이 켜진 배포. control plane 인그레스에 Authelia forward-auth 미들웨어가 걸려 있다
- **실행 단계**: (a) Authelia 세션 없이 공개 주소로 화면 `/access`와 관리 API를 연다 → (b) Authelia로 로그인한 뒤 화면 `/access`를 열고 레포 하나를 추가 → (c) 같은 Authelia 세션으로 세션 API(`GET /sessions`)를 호출 → (d) `/api/v1/auth/*`를 호출 → (e) 클러스터 안에서 control plane Service로 관리 API를 호출해 레포 하나를 추가
- **기대 결과**: (a) forward-auth가 Authelia 로그인으로 돌려보내고 control plane에 닿지 않는다. (b) 화면이 control plane 자체의 로그인 안내 없이 곧바로 목록을 보이고, 추가가 성공한다. (c) 세션 API도 같은 문으로 지금처럼 동작한다. (d) 그런 경로가 없다(앱 자체 로그인 없음). (e) 추가가 성공한다 — 인그레스 지나지 않는 호출자는 세션 API와 같은 범위로 닿는다
- **검증 AC**: AC-H2

### 시나리오 3: 헬퍼 파드 신원 확인
- **사전 조건**: 기능이 켜진 배포에 active 상태의 `approval-gated` 세션 S1, S2
- **실행 단계**: (a) S1 헬퍼 파드의 control plane 토큰으로 내부 조회·요청 API 호출 → (b) 토큰 없이, PocketAide audience 토큰으로, 다른 서비스 어카운트의 같은 audience 토큰으로 호출 → (c) S1 헬퍼 파드를 지우고 같은 이름으로 다시 뜬 뒤 옛 토큰으로 호출 → (d) S1 워크로드 파드에서 control plane 내부 API에 연결 시도 → (e) 내부 API로 허용 목록을 바꾸는 경로를 찾는다(추가·삭제·허락)
- **기대 결과**: (a) 성공하고, 남은 요청의 세션은 S1이다(S2가 아님). (b)·(c)는 `401`. (d) AC-F2 egress 경계로 연결되지 않는다. (e) 그런 경로가 없다 — 내부 API는 조회와 대기 요청만 만든다
- **검증 AC**: AC-H3

### 시나리오 4: 접근 요청 도구 — 게이트 없이 대기 항목 하나
- **사전 조건**: 기능이 켜진 배포에 active 상태의 `approval-gated` 세션 1개. 호출을 관찰할 수 있는 승인 게이트웨이. 허용 목록에 없는 비공개 레포 R
- **실행 단계**: (a) 세션이 R에 대해 접근 요청 도구를 부르게 함 → (b) 같은 R로 다시 부르게 함 → (c) 관리 화면에서 대기 목록 확인 → 허락 → (d) 다시 R로 접근 요청 도구를 부르게 함 → (e) 다른 비공개 레포 R2를 요청하게 한 뒤 관리 화면에서 거절
- **기대 결과**: (a) 승인 게이트웨이에 요청이 생기지 않고, 결과는 "대기 중"이며 세션 출력에 어디서 결정하는지 알려 주는 in-band 마커가 남는다. (b) 대기 항목은 여전히 하나다. (c) 대기 카드에 세션 이름·타입·이유가 보이고, 허락하면 R이 허용 목록에 "요청 허락" 경로로 오르고 대기 목록에서 빠진다. (d) 결과가 "이미 허용됨"이고 새 항목이 생기지 않는다. (e) R2는 대기 목록에서만 빠지고 허용 목록은 그대로다
- **검증 AC**: AC-H4, AC-H2

### 시나리오 5: 비공개 `git_clone`
- **사전 조건**: 기능이 켜진 배포, K3s MCP의 GitHub App 설치 토큰 도구가 닿는 헬퍼 파드, 공유 볼륨이 켜진 `approval-gated`·`minimax-code` 세션 각 1개. 호출을 관찰할 수 있는 승인 게이트웨이. 비공개 레포 P(허용 목록에 있음), Q(없음), 공개 레포 O(없음)·O'(있음)
- **실행 단계**: 각 세션에서 (a) P를 `git_clone` → (b) Q를 `git_clone` → (c) O를 `git_clone` → (d) O'을 `git_clone` → (e) `github.com` 밖의 주소를 `git_clone` → (f) control plane을 잠시 내린 채 P를 `git_clone` → (g) (a)의 클론 결과에서 `.git/config`·원격 URL·도구 결과·세션 출력·헬퍼 파드 로그·공유 볼륨을 훑어 토큰 흔적을 찾는다 → (h) K3s MCP 호출 기록에서 발급 범위를 확인한다 → (i) 워크로드 파드에서 헬퍼 파드의 K3s MCP 프록시 포트로 연결 시도(`approval-gated`)
- **기대 결과**: (a)~(f) 어느 호출도 승인 게이트웨이에 요청을 만들지 않는다. (a) 클론된다. (b)·(c) 클론되지 않고 결과가 "허용 목록에 없음 — 접근 요청 도구로 요청할 수 있다"이다(공개 레포 O도 마찬가지). (d) 클론된다(토큰을 발급할 수 없으면 익명으로). (e) `github.com` 레포만 받는다는 결과로 거절된다. (f) 클론되지 않고 허용 목록을 확인할 수 없다는 결과로 끝난다. (g) 어디에도 토큰이 없다. (h) 발급된 토큰은 P 하나·`contents: read`다. (i) `approval-gated`에서는 연결되지 않는다(`minimax-code`는 AC-G6의 K3s 도구 예외대로 그 포트가 열려 있다)
- **검증 AC**: AC-H5
