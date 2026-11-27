# 테스트 문서: 세션 워크로드 — MiniMax Code CLI (minimax-code)

> 📌 **제안(2026-11-25)**: 이 문서는 reconciler task `rct_20261105-0001`(모델 `tbm_session-platform-docs-impl`)이
> 남긴 gap 「AC-G1~G6의 대응 테스트 문서 부재」를 닫기 위한 **안**이다. 테스트 문서는 AC의 검증 기준을
> 정하므로 대상 레포의 수동 승인 정책(MA1 — `docs/review-policy.md`)에 걸린다 — 사람이 읽고 고칠 수 있다.

## 검증 대상 AC
- AC-G1: 워크로드 타입 `minimax-code` 선택 — 워크로드 파드와 세션 전용 헬퍼 파드가 함께 선다 (PRD: MiniMax Code 워크로드)
- AC-G2: write = `mcode exec` 1회 실행, read = assistant 메시지 텍스트 (PRD: MiniMax Code 워크로드)
- AC-G3: 공급자 자격 증명은 이 타입 전용 Secret(설정은 전용 ConfigMap), 배치는 헬퍼 파드 (PRD: MiniMax Code 워크로드)
- AC-G4: 파일시스템 아카이브, 전용 게이트 (PRD: MiniMax Code 워크로드)
- AC-G5: K3s MCP 도구 — 토큰은 헬퍼 파드의 K3s MCP 프록시에만 (PRD: MiniMax Code 워크로드)
- AC-G6: 헬퍼 파드·egress 경계·승인 게이트 — 승인 게이트의 AC-F2~F6을 그대로, K3s MCP만 예외 (PRD: MiniMax Code 워크로드)

> ⚠️ **e2e 범위 주의**: 이 타입은 세션 파드 안에 `mcode` CLI가 돌아야 하는 워크로드다. kind SUT에는
> MiniMax 공급자 대역도 워크로드 파드 안으로 들어가는 단계도 없다 — 그래서 아래 시나리오는 전부
> `e2e.md`의 「유예」 표에 올라 있고, 현재 검증은 단위·통합 테스트다
> (`control-plane/test/minimax_code_orchestrator_test.go`·`control-plane/internal/service/minimax_code_test.go`·
> `data-plane/cmd/agent/minimax_code_test.go`·`minimax_mcp_registration_test.go`·`minimax_stream_contract_test.go`).
> 해제 조건은 각 행에 적혀 있다.

## 테스트 시나리오

### 시나리오 1: `minimax-code` 타입을 골라 세션을 만들면 워크로드 파드와 헬퍼 파드가 함께 선다
- **사전 조건**: 이 타입의 이미지를 설정한 배포(워크로드 이미지·헬퍼 이미지 모두). 세션 생성 API에 접근할 수 있는 운영자
- **실행 단계**: (a) 세션 생성 요청의 `workloadType`에 `minimax-code`를 넣고 세션을 만든다 → (b) 조회 응답의 타입·모델을 본다 → (c) 워크로드 파드와 헬퍼 파드를 조회한다 → (d) `model`을 빈 문자열·금지 문자로 보내 다시 생성한다 → (e) `model`을 생략하고 ConfigMap에 `model` 키가 없는 배포에서 다시 생성한다 → (f) 이 타입의 이미지를 설정하지 않은 배포에서 다시 생성한다 → (g) `workloadType`을 생략하고, 다시 빈 문자열·금지 값으로 보낸다
- **기대 결과**: (a) 세션이 만들어진다. (b) 타입이 `minimax-code`이고 모델은 요청값이거나 기본값이다. (c) 워크로드 파드에 컨테이너가 하나(주)뿐이고 사이드카가 없다. 헬퍼 파드가 따로 서고 컨테이너가 셋이다 — 세션 MCP·credential-proxy·k3s-mcp-proxy. (d) `400`이며 파드가 생기지 않는다. (e) 모델이 data plane 내장 기본값 `MiniMax-M3.1-Flash`다. (f) 셸로 되돌아가지 않고 **실패**한다 — 파드가 없다. (g) 생략이면 `shell`로 해석되고, 빈 문자열·금지 값·허용값 밖은 pod 생성 전에 `400`이다
- **검증 AC**: AC-G1

### 시나리오 2: write 한 번은 `mcode exec` 한 번이고, read는 assistant 메시지 텍스트뿐이다
- **사전 조건**: `minimax-code` 세션 1개. 실행 argv와 관리형 설정을 관측할 수 있는 환경. 공급자 대역(요청 본문을 기록하는 가짜 서버)
- **실행 단계**: (a) 프롬프트 하나를 보내 argv를 기록한다 → (b) 같은 세션으로 두 번째 프롬프트를 보내 argv를 기록한다 → (c) `--`로 시작하는 프롬프트를 보낸다 → (d) reasoning·tool_call 항목과 항목 끝의 `content` 델타 없는 완결 레코드가 섞인 스트림을 흘린다 → (e) 첫 실행이 실패한 뒤 다시 보낸다 → (f) 관리형 설정 파일을 읽는다
- **기대 결과**: (a) argv에 `--config <관리형 설정>` `--model custom_provider:session-platform/<모델>` `--permission full` `--effort xhigh` `--output-format stream-json`이 있고 `--continue`가 **없다**. (b) `--continue`가 붙는다(첫 **성공** 실행 이후에만). (c) 프롬프트는 옵션으로 해석되지 않는다 — `--` 뒤의 한 positional argv로 전달된다. (d) 세션 출력에는 assistant 텍스트만 순서대로 나오고, 각 메시지에 줄바꿈이 있으며 델타 없이 끝난 항목은 한 번만 투영된다. reasoning·tool_call·생명주기 레코드는 출력에 없다. (e) 첫 실행이 실패했으면 `--continue`가 붙지 않는다. (f) 관리형 설정은 Anthropic-messages 공급자 하나와 세션 모델만 담고, 그 모델 정의가 `xhigh`를 추론 강도 선택지로 갖고 `MiniMax-M3.1-Flash`에 컨텍스트 512k·출력 128k 한도가 담긴다
- **검증 AC**: AC-G2

### 시나리오 3: 자격 증명은 이 타입 전용 Secret·ConfigMap에만 있고 주 컨테이너는 비어 있다
- **사전 조건**: `minimax-code` Secret(`minimax-code-credentials`)·ConfigMap(`minimax-code-config`)이 있는 배포. Secret에 `auth-token`·`k3s-mcp-token`, ConfigMap에 `base-url`·`model`·`ca-cert`이 있다. `claude-code` 쪽 Secret·ConfigMap도 따로 있다
- **실행 단계**: (a) 세션을 만들고 제출된 pod spec을 본다 → (b) 주 컨테이너의 env와 Secret·ConfigMap 참조를 본다 → (c) 헬퍼 파드의 credential-proxy 컨테이너의 Secret·ConfigMap 참조와 env를 본다 → (d) 두 파드의 모든 컨테이너를 훑어 `claude-code` 쪽 Secret·ConfigMap 참조가 있는지 본다 → (e) 주 컨테이너에 자격 증명 변수를 넣어 기동을 시도한다 → (f) 주 컨테이너에 loopback 프록시 주소를 넣어 기동을 시도한다 → (g) Secret·ConfigMap 이름을 `claude-code` 쪽 이름과 같게 두고 기동을 시도한다
- **기대 결과**: (a) 워크로드 파드와 헬퍼 파드가 선다. (b) 주 컨테이너에 Secret 참조가 **없다** — ConfigMap 참조는 이 타입 ConfigMap의 선택 `model`뿐이다. 주 컨테이너에는 자격 증명 변수가 없다. (c) credential-proxy 컨테이너가 이 타입 Secret의 필수 `auth-token`과 이 타입 ConfigMap의 필수 `base-url`·선택 `ca-cert`을 참조한다. (d) 어느 컨테이너도 `claude-code` Secret·ConfigMap을 참조하지 않는다. (e) 주 컨테이너가 기동을 거부한다. (f) 주 컨테이너가 기동을 거부한다(loopback 프록시 주소). (g) control plane이 기동을 거부한다
- **검증 AC**: AC-G3

### 시나리오 4: 아카이브 게이트가 꺼져 있으면 동결이 거부되고, 켜면 파일시스템 아카이브가 왕복한다
- **사전 조건**: `minimax-code` 세션 1개. `MINIMAX_CODE_ARCHIVE_ENABLED`가 꺼진 배포(A)와 켜진 배포(B). B의 세션에는 작업 디렉터리 파일·`~/.minimax`의 대화 기록·누적 출력이 있다
- **실행 단계**: (a) A에서 동결을 요청한다 → (b) A의 파드가 남아 있는지 본다 → (c) B에서 동결을 요청한다 → (d) 복원한다 → (e) 복원된 파드와 세션을 조회한다 → (f) 복원 후 프롬프트를 보내 argv를 본다 → (g) B의 동결 대상 파드 주석을 본다 → (h) 공유 볼륨을 켠 배포에서 동결 전 볼륨에 파일을 두고 동결·복원을 반복한다
- **기대 결과**: (a) 동결이 거부된다 — 다른 타입의 체크포인터로 넘어가지 않는다. (b) 파드가 그대로 남아 있다. (c) 동결이 성공하고 헬퍼 파드도 함께 회수된다. (d) 복원이 성공한다. (e) 새 워크로드 파드와 새 헬퍼 파드가 함께 서고 타입·모델이 유지된다. 작업 디렉터리·`~/.minimax`·누적 출력·커서가 보존된다. (f) 복원 후 첫 write의 argv에 `--continue`가 붙는다. (g) 복원 대상 파드에 `session-platform.dev/restore-archive` 주석이 있다. (h) 공유 볼륨의 파일이 복원 후 새 공유 볼륨에 같은 내용으로 남는다
- **검증 AC**: AC-G4

### 시나리오 5: K3s MCP 토큰은 헬퍼 파드의 K3s MCP 프록시 컨테이너에만 있고, 세션 id 헤더가 붙는다
- **사전 조건**: `minimax-code` 세션 1개. Secret의 선택 `k3s-mcp-token`이 있는 배포(P)와 없는 배포(Q). K3s MCP 상류를 대신할 기록용 서버
- **실행 단계**: (a) P에서 세션과 파드를 만들어 제출된 spec을 본다 → (b) 두 파드의 모든 컨테이너 env를 훑는다 → (c) P의 프록시에 K3s MCP 요청을 보내 기록용 상류의 요청 헤더를 본다 → (d) 호출자가 같은 이름의 헤더를 다시 넣어 보낸다 → (e) 프록시의 `/healthz`를 본다 → (f) Q에서 같은 조회를 한다 → (g) 복원 뒤 새 헬퍼 파드의 `SESSION_ID`와 프록시가 붙이는 헤더를 본다
- **기대 결과**: (a) 헬퍼 파드의 `k3s-mcp-proxy` 컨테이너만 이 타입 Secret의 `k3s-mcp-token`을 참조하고, 헬퍼 배치로 `:8093`에 바인드하며 자기만 마운트하는 emptyDir를 갖는다. (b) 다른 어느 컨테이너도 `K3S_MCP_TOKEN`을 받지 않는다. (c) 호출자의 `Authorization`과 허용 목록 밖 헤더는 지워지고 `Bearer`만 주입되며, `Mcp-Session-Id`가 양방향으로 보존되고, 상류 요청마다 `X-Session-Platform-Session-Id`가 정확히 하나 붙는다. (d) 호출자가 보낸 같은 이름의 헤더는 상류에 닿지 않는다. (e) `configured: true`다. (f) Q는 `/healthz`가 `configured: false`를 알리고 그 밖의 요청에 `503`을 돌려준다 — 실패가 아니라 K3s 도구 없는 세션이다. (g) 새 헬퍼 파드도 평문 `SESSION_ID`를 받고 같은 헤더를 붙인다
- **검증 AC**: AC-G5

### 시나리오 6: 헬퍼 파드는 컨테이너 셋이고, 워크로드 파드의 egress는 헬퍼 포트로만 묶인다
- **사전 조건**: `minimax-code` 세션 1개. 클러스터의 파드·정책을 볼 수 있는 권한. 공유 볼륨 클래스를 지정한 배포와 하지 않은 배포
- **실행 단계**: (a) 세션과 파드를 만들어 라벨·컨테이너·서비스 어카운트 토큰 마운트를 확인한다 → (b) 워크로드 파드의 env를 본다 → (c) 워크로드 egress 정책과 헬퍼 ingress 정책을 본다 → (d) 게이트웨이 컨테이너가 어디 있는지 본다 → (e) 공유 볼륨 클래스가 있는 배포에서 두 파드의 볼륨 마운트를 비교한다 → (f) 외부 목적지에 워크로드 파드에서 나가 본다 → (g) 헬퍼 파드의 K3s MCP 프록시를 경유하는 요청을 본다
- **기대 결과**: (a) 헬퍼 파드가 세션 라벨과 `minimax-code` 타입 라벨을 갖고 컨테이너가 셋이다(세션 MCP·credential-proxy·k3s-mcp-proxy)이며 서비스 어카운트 토큰을 마운트하지 않는다. 워크로드 파드에 사이드카가 없다. (b) 워크로드 파드는 헬퍼 주소 세 개(`:8091`·`:8092`·`:8093`)를 env로 받는다. (c) 워크로드 egress는 kube-dns와 헬퍼 포트 셋만 허용하고 헬퍼 ingress는 자기 세션 워크로드 파드로부터만 온다. (d) 게이트웨이 3종은 세션 MCP 컨테이너에만 있다. (e) 공유 볼륨 클래스가 있으면 워크로드 파드와 세션 MCP 컨테이너가 같은 claim을 같은 경로에 마운트한다. (f) 외부 목적지로 나가지 않는다. (g) K3s MCP 프록시 경유만 성립한다 — K3s MCP 도구는 이 타입에서 세션 승인 게이트를 지나지 않는다(자체 게이트가 막는다)
- **검증 AC**: AC-G6
