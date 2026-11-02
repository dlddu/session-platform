# 테스트 문서: 세션 워크로드 — MiniMax Code CLI (minimax-code)

## 검증 대상 AC
- AC-G1: 워크로드 타입 `minimax-code` 선택 (PRD: 세션 워크로드 — MiniMax Code CLI)
- AC-G2: write = `mcode exec` 1회 실행, read = assistant 메시지 텍스트
- AC-G3: 공급자 자격 증명은 이 타입 전용 Secret(설정은 전용 ConfigMap), 배치는 AC-F6 헬퍼 파드
- AC-G4: 파일시스템 아카이브, 전용 게이트
- AC-G5: K3s MCP 도구 — 토큰은 헬퍼 파드의 K3s MCP 프록시에만

> AC-G6(헬퍼 파드·egress 경계·승인 게이트)은 `test/repo-access.md`가 함께 다룬다 —
> 그 시나리오 5·6이 `minimax-code`의 공유 볼륨과 헬퍼 파드 배치를 함께 단언한다.

> ⚠️ **e2e 범위 주의**: 이 타입의 관측 대상은 제출된 pod spec·헬퍼 파드 컨테이너·워크로드
> 파드의 파일시스템이라 브라우저나 control-plane HTTP API로 볼 수 없다. 그래서 아래 시나리오는
> 전부 `e2e.md`의 「유예」 표에 올라 있고, 현재 검증은 단위·통합 테스트
> (`control-plane/test/minimax_code_orchestrator_test.go` · `data-plane/cmd/agent/minimax_code_test.go`
> 등)와 운영 배포 확인이다. 해제 조건은 각 행에 적혀 있다.

## 테스트 시나리오

### 시나리오 1: 워크로드 타입 `minimax-code` 선택과 파드 배치
- **사전 조건**: 이 타입의 이미지를 설정한 배포(A — `DATA_PLANE_MINIMAX_CODE_IMAGE` 있음), 그 이미지를 설정하지 않은 배포(B)
- **실행 단계**: (a) A에서 `workloadType=minimax-code`로 세션을 생성 → (b) 세션 조회로 파드·타입·모델을 확인 → (c) A에서 잘못된 `model` 값으로 세션을 생성 → (d) B에서 같은 타입으로 세션을 생성 → (e) `workloadType`을 생략하고 세션을 생성 → (f) 허용값 밖의 `workloadType`으로 세션을 생성
- **기대 결과**: (a) 생성이 성공한다. (b) **워크로드 파드**(주 컨테이너 하나·사이드카 없음)와 **헬퍼 파드**(컨테이너 셋 — 세션 MCP·credential-proxy·k3s-mcp-proxy)가 함께 뜨고, 조회 응답의 타입이 `minimax-code`이며 모델이 기대값이다. (c) `400`이다. (d) **실패한다 — `shell`로 되돌아가지 않는다.** (e) 타입이 `shell`이다. (f) pod를 만들기 전에 `400`이다
- **검증 AC**: AC-G1

### 시나리오 2: write = `mcode exec` 1회 실행, read = assistant 메시지 텍스트
- **사전 조건**: `minimax-code` 세션 1개. 모델은 `platform-default`(관리형 설정의 세션 모델에 추론 강도 선택지·한도가 선언되어 있다)
- **실행 단계**: (a) write 1회 → 실행된 argv를 확인 → (b) 성공한 뒤 write 1회 → argv를 확인 → (c) `--`로 시작하는 프롬프트로 write → (d) reasoning·tool_call 항목이 섞인 스트림을 read → (e) 델타 없이 끝난 항목이 있는 스트림을 read → (f) 세션 출력 한도를 넘기는 write
- **기대 결과**: (a) argv에 `--permission full --effort xhigh`와 `--output-format stream-json`이 있고 `--continue`가 **없다**. (b) argv에 `--continue`가 **있다**. (c) 프롬프트는 `--` 뒤의 단일 positional argv로 넘어가고 옵션으로 해석되지 않는다. (d) **assistant 텍스트만**, 메시지마다 줄바꿈되어, 중복 없이 투영된다. reasoning·tool_call·수명주기 레코드는 사용자 출력이 아니다. (e) 델타 없이 끝난 항목은 완료 레코드의 `content`로 **한 번만** 투영된다. (f) 한도 초과로 거부되고 큐·출력 한도 규칙이 AC-E2~E4 그대로 적용된다
- **검증 AC**: AC-G2

### 시나리오 3: 공급자 자격 증명 배치 — Secret은 자격 증명만, ConfigMap은 설정
- **사전 조건**: `minimax-code-credentials` Secret과 `minimax-code-config` ConfigMap이 있는 배포(A), Secret·ConfigMap 이름을 `claude-code` 쪽과 같게 한 배포(B), 주 컨테이너에 자격 증명 변수를 직접 넣은 배포(C)
- **실행 단계**: (a) A에서 `minimax-code` 세션을 생성 → 제출된 두 파드의 spec에서 Secret·ConfigMap 참조를 확인 → (b) 주 컨테이너의 env를 확인 → (c) C에서 세션을 생성 → (d) B에서 control plane을 기동
- **기대 결과**: (a) **워크로드 파드는 Secret 참조 0개·ConfigMap 참조 1개**(선택 `model`)이고, **헬퍼 파드는 Secret 참조 2개**(필수 `auth-token`·선택 `k3s-mcp-token`)와 ConfigMap 참조 2개(필수 `base-url`·선택 `ca-cert`)다. 두 파드의 어느 컨테이너도 `claude-code` Secret·ConfigMap을 참조하지 않는다. (b) 주 컨테이너는 헬퍼 파드의 프록시 주소와 비밀 아닌 placeholder만 본다. (c) **agent가 기동을 거부한다.** (d) control plane이 **기동을 거부한다** — `MINIMAX_CODE_CREDENTIALS_SECRET`·`MINIMAX_CODE_CONFIG_CONFIGMAP`이 각각 `claude-code` 쪽 이름과 같으면 안 된다
- **검증 AC**: AC-G3

### 시나리오 4: 파일시스템 아카이브와 전용 게이트
- **사전 조건**: `MINIMAX_CODE_ARCHIVE_ENABLED=true`인 배포(A), 게이트가 꺼진 배포(B), 공유 볼륨 클래스를 지정한 배포(C). 각 배포에 `minimax-code` 세션 1개
- **실행 단계**: (a) A에서 동결 → 복원 → 세션의 타입·모델·커서·파일을 확인 → (b) B에서 동결 요청 → (c) C에서 공유 볼륨에 파일을 둔 뒤 동결 → 복원 후 그 볼륨을 확인 → (d) 복원 대상 파드의 주석을 확인 → (e) A에서 복원 후 첫 write의 argv를 확인
- **기대 결과**: (a) 타입·모델이 유지되고 작업 디렉터리·`~/.minimax`·누적 출력·커서가 보존된다. (b) 동결이 **거부**되고 파드가 유지된다 — 게이트가 off면 다른 타입의 체크포인터로 넘어가지 않는다. (c) 공유 볼륨의 파일 내용이 **복원 후 새 공유 볼륨에 그대로** 남는다. (d) 복원 대상 파드에 아카이브 주석(`session-platform.dev/restore-archive`)이 있다. (e) 복원 후 첫 write의 argv에 `--continue`가 붙는다
- **검증 AC**: AC-G4

### 시나리오 5: K3s MCP 도구 — 토큰은 헬퍼 파드의 K3s MCP 프록시에만
- **사전 조건**: 이 타입 Secret에 `k3s-mcp-token`이 있는 배포(A), 그 키가 없는 배포(B). 각 배포에 `minimax-code` 세션 1개
- **실행 단계**: (a) A에서 세션을 생성 → 두 파드의 `k3s-mcp-token` 참조 위치와 포트를 확인 → (b) 프록시 컨테이너의 `/healthz`를 확인 → (c) 호출자 `Authorization`과 허용 목록 밖 헤더를 붙여 K3s MCP를 호출 → (d) `X-Session-Platform-Session-Id` 헤더가 상류에 붙는지 확인 → (e) `/plugin-seed`의 부트스트랩 상태를 확인 → (f) 워크로드 파드의 `~/.minimax/mcp.json` 등록 내용을 확인 → (g) B에서 같은 절차를 확인
- **기대 결과**: (a) `k3s-mcp-token`을 참조하는 컨테이너는 헬퍼 파드의 `k3s-mcp-proxy` **하나**뿐이고, 다른 어떤 컨테이너도 `K3S_MCP_TOKEN`을 받지 않는다. 이 컨테이너는 헬퍼 파드 IP에 `:8093`으로 바인드된다. (b) `configured:true`다. (c) 호출자의 `Authorization`은 지워지고 `Bearer`가 주입되며, 허용 목록 밖 헤더는 전달되지 않고, **응답에 토큰 원문이 없다**. (d) 상류 요청마다 `X-Session-Platform-Session-Id`가 **정확히 하나** 붙고, 호출자가 보낸 같은 이름의 헤더는 상류에 닿지 않는다. (e) 토큰이 있으면 `ready`, 없으면 `unconfigured`, 실패하면 `failed`다 — 실패가 나도 프록시는 죽지 않는다. (f) K3s MCP 서버만 프록시 주소로 바뀌고 `oauthClientId`·`Authorization`이 지워지며, 다른 서버는 빠진다. 번들이 `failed`·`unconfigured`면 **세션 MCP만** 등록된다. (g) `configured:false`·`503`이고 `/plugin-seed`는 `unconfigured`다 — 키 없는 배포는 실패가 아니라 K3s 도구 없는 세션이다
- **검증 AC**: AC-G5
