# 테스트 문서: 멀티테넌시 — 테넌트별 워크로드 타입과 세션 목록

## 검증 대상 AC
- AC-K1: 호출자 식별 — ServiceAccount 토큰, 없으면 운영자 (PRD: 멀티테넌시)
- AC-K2: 세션 소속과 가시성 (PRD: 멀티테넌시)
- AC-K3: 테넌트별 워크로드 타입 (PRD: 멀티테넌시)
- AC-K4: 운영자 화면과 운영자 전용 경로 (PRD: 멀티테넌시)
- AC-K5: 테넌트 설정과 하위 호환 (PRD: 멀티테넌시)

> 📌 **SUT 설정**: kind e2e 오버레이(`deploy/overlays/e2e/e2e-tenancy.yaml`)가 테넌트 셋을 둔다 — `e2e-alpha`(계정 `default/tenant-alpha`,
> 타입 `shell`·`codex-chat`), `e2e-beta`(계정 `default/tenant-beta`, 타입 `shell`), `e2e-gamma`(계정 `default/tenant-gamma`, 타입 `"*"` — 제한 없음). 어느 테넌트도 주장하지 않는 계정
> `default/tenant-outsider`도 있다. 스위트는 실물 TokenRequest로 audience `session-platform` 토큰을 발급받는다 —
> 대역이 없다. control plane의 TokenReview 권한도 같은 파일이 붙인다.

## 테스트 시나리오

### 시나리오 1: 테넌트는 자기 테넌트로 만들고 자기 세션만 본다
- **사전 조건**: 테넌트가 설정된 SUT. `e2e-alpha`·`e2e-beta` 계정의 audience `session-platform` 토큰
- **실행 단계**: (a) alpha 토큰으로 이름만 담아 세션 생성 → (b) beta 토큰으로 세션 생성 → (c) alpha 토큰으로 목록 조회 → (d) alpha 토큰으로 자기 세션 조회·write
- **기대 결과**: (a) `201`, 응답의 `tenant`가 `e2e-alpha`, 상태 `active`. (c) alpha 세션은 있고 beta 세션은 없으며, 목록의 모든 세션 `tenant`가 `e2e-alpha`. (d) 둘 다 `200`
- **검증 AC**: AC-K1, AC-K2

### 시나리오 2: 다른 테넌트와 무소속 세션은 없는 세션과 같다
- **사전 조건**: beta 토큰으로 만든 세션 B, 토큰 없이(운영자) 만든 세션 O
- **실행 단계**: (a) alpha 토큰으로 B·O 각각에 조회·read·write·switch·snapshot·delete → (b) 운영자로 B·O 조회 → (c) 운영자로 목록, `?tenant=e2e-beta` 목록 조회
- **기대 결과**: (a) 전부 `404`. (b) 둘 다 `200` — (a)의 시도가 아무것도 바꾸지 않았다. O의 `tenant`는 없다. (c) 전체 목록에 B가 있고, 필터 목록에는 B가 `tenant: e2e-beta`로 있다
- **검증 AC**: AC-K2

### 시나리오 3: 허용 타입만 만들고, 설정 응답도 허용 타입만 보인다
- **사전 조건**: beta 토큰(`shell`만 허용), gamma 토큰(`"*"`)
- **실행 단계**: (a) beta 토큰으로 `workloadType: codex` 생성 → (b) beta 토큰으로 `tenant: e2e-alpha` 생성 → (c) 운영자로 `tenant: e2e-beta, workloadType: codex` 생성 → (d) beta 토큰으로 `GET /config` → (e) 운영자로 `GET /config` → (f) gamma 토큰으로 `GET /config` → (g) gamma 토큰으로 알 수 없는 타입 생성
- **기대 결과**: (a)~(c) `403`, pod가 생기지 않는다. (d) `tenant.name`이 `e2e-beta`, `tenants`는 빈 배열, `workloadTypes`는 `shell` 하나. (e) `tenant`는 null, `tenants`는 세 테넌트. (f) `workloadTypes`와 `tenant.workloadTypes`가 둘 다 (e)의 `workloadTypes`와 같다 — `"*"`는 펼쳐져 나온다. (g) `400`
- **검증 AC**: AC-K3, AC-K4

### 시나리오 4: 운영자 화면 — 테넌트 필터·태그·테넌트로 생성
- **사전 조건**: 테넌트가 설정된 SUT를 운영자(토큰 없음)로 연 브라우저
- **실행 단계**: (a) 새 세션 화면을 연다 → (b) 테넌트 `e2e-alpha`를 고른다 → (c) `codex-chat`를 골라 세션을 만든다 → (d) 세션 목록에서 필터를 `e2e-alpha`로, 다시 `e2e-beta`로 바꾼다
- **기대 결과**: (a) 테넌트 선택이 있고 기본은 무소속이다. (b) 타입 카드가 `shell`·`codex-chat` 둘로 좁혀진다. (c) 만든 세션 카드에 `e2e-alpha` 태그가 붙는다. (d) `e2e-alpha`에서는 그 세션이 보이고 `e2e-beta`에서는 보이지 않는다
- **검증 AC**: AC-K4

### 시나리오 5: 거절된 호출자는 운영자가 되지 않는다
- **사전 조건**: 테넌트가 설정된 SUT
- **실행 단계**: (a) 토큰 아닌 문자열 Bearer로 `GET /sessions`·`GET /config` → (b) alpha 계정의 **apiserver audience** 토큰으로 같은 호출 → (c) `tenant-outsider` 계정의 `session-platform` audience 토큰으로 같은 호출 → (d) alpha 토큰으로 레포 접근 관리 API `GET /repo-access`
- **기대 결과**: (a)·(b) `401`. (c) `403`. (d) `403` — 기능이 꺼진 SUT라도 문에서 먼저 막힌다(운영자의 같은 호출은 `503`)
- **검증 AC**: AC-K1, AC-K4

### 시나리오 6: 설정 검증과 하위 호환
- **사전 조건**: control plane 기동 환경 셋 — `SESSION_PLATFORM_TENANTS`가 잘못된 것(대문자 이름·한 계정을 두 테넌트가 주장·알 수 없는 타입·알 수 없는 필드·`"*"`와 다른 타입의 혼합), 빈 것, 테넌시 이전에 만든 세션이 남아 있는 것
- **실행 단계**: (a) 잘못된 설정으로 기동 → (b) 빈 설정으로 기동해 운영자로 세션 API, Bearer로 `GET /sessions` → (c) 이전 세션을 운영자와 테넌트로 조회
- **기대 결과**: (a) 기동 실패. (b) 운영자 동작은 테넌시 이전과 같고 Bearer 호출은 `403`. (c) 이전 세션은 `tenant`가 없고 운영자만 본다(테넌트는 `404`)
- **검증 AC**: AC-K5
