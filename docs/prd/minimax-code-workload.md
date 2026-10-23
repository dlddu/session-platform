# PRD: 세션 워크로드 — MiniMax Code CLI (minimax-code)

> 대상 요구사항: 세션 워크로드 타입에 **MiniMax Code CLI(`mcode`) 에이전트** 추가
> (`shell`·`claude-code`·`approval-gated`와 병존하는 네 번째 타입, `claude-code-workload.md`와 대응 관계)

## 달성 가치
- **V8 목적에 맞는 작업 환경 선택** — 세션 생성 시 네 번째 작업 환경(MiniMax 모델로 도는 코딩 에이전트)을 고를 수 있게 하고, 선택과 무관하게 V1~V5 보장이 동일하게 성립함을 확정 (AC-G1)
- **V1 세션 격리** — 공급자 자격 증명을 도구가 도는 컨테이너 밖에 두고, 다른 타입의 자격 증명과도 섞지 않음 (AC-A1/A2 구체화)
- **V2 유휴 자원 회수** — `claude-code`와 같은 파일시스템 아카이브로 동결·회수 (AC-B1·A3의 타입별 경로)
- **V3 끊김 없는 세션 연속성** — 대화 문맥·작업 디렉터리·누적 출력이 동결·복원을 건너 보존됨 (AC-B2/B3 구체화), write/read를 프롬프트·실행 출력 시맨틱으로 확정 (AC-C2/C3 구체화)
- **V4 자유로운 멀티세션 전환** — 전환 후에도 동일한 offset 커서 규약으로 출력을 이어 읽음 (AC-C2 구체화)
- **V1 세션 격리 (도구 표면)** — `claude-code`가 플러그인으로 받는 홈랩 K3s MCP 도구를 이 타입에도 주되, 그 토큰은 CLI 컨테이너와 아카이브 밖에 둠 (AC-G5)

> 📌 **가치 연결 규칙**: `claude-code-workload.md`·`approval-gated-workload.md`와 동일하게, 각 AC의 달성 가치는
> 자신이 구체화하는 상위 AC의 가치를 상속한다. **AC-G1(타입 선택)만 예외**로 추상 가치 **V8**에 직접 연결한다
> — AC-E1·AC-F1과 같은 이유이며, 타입이 늘어도 `../values.md`는 변하지 않는다. 새 가치는 신설하지 않는다.

> 이 PRD는 **"같은 실행 모델 위에서 공급자와 CLI만 바꾼다"** 는 한 가지 성질을 세션 워크로드 타입으로 못박는다.
> 실행 모델(원샷 프롬프트·직렬 큐·커서 규약·대화 연속성·아카이브)은 `claude-code`와 같으므로 **복제하지 않고
> AC-E2~E5를 참조**하며, 이 문서는 그 위에 얹히는 차이만 적는다. 차이는 넷이다 — 실행하는 CLI와 그 출력의
> 투영(AC-G2), 자격 증명을 담는 Secret(AC-G3), 아카이브 게이트(AC-G4), K3s MCP 플러그인의 배선(AC-G5). credential-proxy는 **동작 계약도 배치도
> AC-E6 그대로**(워크로드 파드의 loopback 사이드카)이고, 읽는 Secret만 이 타입 전용이다.

## Acceptance Criteria

### AC-G1: 워크로드 타입 `minimax-code` 선택
- **설명**: 세션 생성 요청의 `workloadType` 허용값에 `minimax-code`가 추가되어 `shell`·`claude-code`·`approval-gated`·`minimax-code` 네 가지가 된다. 필드 생략만 `shell`로 해석하는 규칙, explicit empty/null/비문자열·허용값 밖 입력을 pod 생성 전에 400으로 거부하는 규칙, 타입 불변 규칙은 AC-E1과 같다. control plane은 이 타입에 대해 **워크로드 파드 하나**(주 컨테이너 + credential-proxy 사이드카 + K3s MCP 프록시 사이드카, AC-G5)만 프로비저닝하며 헬퍼 파드는 없다. 배포가 이 타입의 이미지를 설정하지 않았으면 생성은 셸로 되돌아가지 않고 실패한다. `model` 필드 계약(생략=`platform-default` 별칭, 불변, pattern 검증, 구체 값은 Secret 기본값보다 우선)은 AC-E6을 그대로 따른다. 다만 Claude Code와 달리 `mcode`는 공급자 항목을 통해서만 모델을 고르므로, `platform-default`이면서 Secret에 `model` 키가 없거나 비어 있을 때는 `--model` 생략 대신 data plane 내장 기본값 `MiniMax-M3.1-Flash`를 쓴다.
- **달성 가치**: V8, V1
- **구체화 대상**: AC-A1의 "실제 세션 워크로드" — 이제 네 갈래로 분기한다 (AC-D1=`shell`, AC-E2~E6=`claude-code`, AC-F2~F6=`approval-gated`, AC-G2~G4=`minimax-code`)
- **검증 방법**: `workloadType=minimax-code`로 세션을 생성하면 워크로드 파드 하나(컨테이너 셋)가 기동되고 조회 응답의 타입·모델이 기대값임을 확인한다. 이미지 미설정 배포에서는 pod 없이 실패함을 확인한다. 잘못된 model은 400임을 확인한다.

### AC-G2: write = `mcode exec` 1회 실행, read = assistant 메시지 텍스트
- **설명**: 큐·바운드·커서·대화 연속성은 AC-E2~E4를 그대로 따른다(1 MiB 프롬프트 413, 64개·8 MiB 큐 429, 실행당 16 MiB·세션 256 MiB 출력 한도, 첫 **성공** 실행 이후에만 `--continue`). 한 번의 실행은 다음과 같다:

  ```
  mcode exec --config <관리형 설정> \
    --model custom_provider:session-platform/<모델> \
    --permission full --effort xhigh --output-format stream-json [--continue] -- "<payload>"
  ```

  `--` 뒤 payload는 옵션이 아닌 단일 positional argv다. 권한은 `full`, 추론 강도는 `xhigh`로 **고정**한다 — 실행은 무인이므로 사람에게 묻고 멈추는 모드(`smart`는 사람 없이 대부분의 셸 명령을 거부한다)는 turn을 실패시킬 뿐이고, 도구의 경계는 실제 자격 증명을 갖지 않는 세션 파드다(AC-G3). 고정값이므로 상위 버전의 기본값 변경이 실행 방식을 조용히 바꾸지 못한다. `mcode`는 모델 정의에 추론 강도 선택지가 없으면 `--effort`를 거부하고, 토큰 한도가 없으면 응답을 16384 토큰으로 자르므로, 관리형 설정은 세션 모델에 추론 강도 선택지(항상 켜진 adaptive thinking)를 선언하고 한도가 알려진 모델(`MiniMax-M3.1-Flash`: 컨텍스트 512k·출력 128k)에는 그 한도도 선언한다. data plane은 stdout JSONL에서 `agent_message` 항목의 `contentDelta`만 순서대로 투영하고, 항목이 끝날 때(`item.completed`) 줄바꿈을 맞춘다. 델타 없이 끝난 항목은 완료 레코드의 `content`로 한 번만 투영한다. reasoning·tool_call·생명주기 레코드는 사용자 출력이 아니다. 실패한 turn은 `mcode`가 stderr에도 보고하므로 별도로 투영하지 않으며, stderr는 AC-E3대로 진단 출력으로 같은 버퍼에 합쳐진다.
- **달성 가치**: V3, V4
- **구체화 대상**: AC-C2(read)·AC-C3(write) — AC-E2/E3의 투영 규칙을 이 CLI의 스트림 형식에 맞춰 바꾼 것
- **검증 방법**: 모든 write의 argv에 `--permission full --effort xhigh`가 있고, 관리형 설정의 세션 모델 정의가 `xhigh`를 추론 강도 선택지로 담으며 `MiniMax-M3.1-Flash`에는 512k/128k 한도를 담음을 확인한다. 첫 write의 argv에 `--continue`가 없고 성공 후 두 번째 write에는 있음을 확인한다. `--`로 시작하는 프롬프트가 옵션으로 해석되지 않음을 확인한다. reasoning·tool_call이 섞인 스트림에서 assistant 텍스트만, 중복 없이, 메시지마다 줄바꿈되어 투영됨을 확인한다.

### AC-G3: 공급자 자격 증명은 이 타입 전용 Secret, 배치는 AC-E6 사이드카
- **설명**: 플랫폼 Secret `minimax-code-credentials`(이름은 control plane 설정으로 바꿀 수 있다)의 키 이름과 의미는 AC-E6과 같다 — 필수 `base-url`·`auth-token`은 credential-proxy 사이드카에만, 선택 `ca-cert`도 사이드카에만, 선택 `model`은 `platform-default` 세션의 주 컨테이너에만 주입된다. `base-url`은 MiniMax의 Anthropic 호환 엔드포인트(예: `https://api.minimax.io/anthropic`)이고 `auth-token`은 Token Plan **Subscription Key** 또는 종량제 API 키다. proxy는 AC-E6 그대로 HTTPS origin을 고정하고 허용 목록 헤더만 전달한 뒤 토큰을 `Authorization: Bearer`로 주입하는데, MiniMax는 두 종류의 키 모두 이 형식을 받는다. 주 컨테이너는 `ANTHROPIC_BASE_URL=http://127.0.0.1:8091`과 비밀이 아닌 placeholder만 보고, 시작 시 `ANTHROPIC_API_KEY`·`CLAUDE_CODE_OAUTH_TOKEN`·`MINIMAX_API_KEY`·`MCODE_PROVIDER_API_KEY`·`K3S_MCP_TOKEN` 등 두 CLI가 스스로 읽을 수 있는 자격 증명 변수가 하나라도 있으면 기동을 거부한다. control plane은 `minimax-code` Secret 이름이 `claude-code` Secret 이름과 같으면 기동을 거부한다. `claude-code`처럼 주 컨테이너가 K3s MCP 토큰을 받아 플러그인을 부트스트랩하는 경로는 이 타입에 없다 — K3s MCP 도구는 토큰을 사이드카에 두는 AC-G5로만 들어온다. `mcode`가 읽는 설정은 data plane이 기동할 때마다 **상태 트리 바깥**에 다시 쓰는 관리형 파일 하나로, loopback proxy를 가리키는 Anthropic-messages 공급자 하나와 세션 모델만 선언하고 데이터 기여(data contribution)와 자가 업데이트를 끈다. agent는 `MCODE_DISABLE_EVAL_CAPTURE=1`도 설정해 실행 단계 보고 업로드를 막는다. 이 파일은 아카이브되지 않으므로 복원이 낡은 설정을 되살릴 수 없다. **두 타입의 Secret은 서로 섞이지 않는다** — `minimax-code` 파드의 어떤 컨테이너도 `claude-code` Secret을 참조하지 않는다.
- **달성 가치**: V1
- **구체화 대상**: AC-A1/A2의 세션 격리 — AC-E6의 동작 계약을 다른 공급자 Secret으로 재사용
- **검증 방법**: 제출된 pod spec에서 주 컨테이너의 Secret 참조가 이 타입 Secret의 선택 `model`뿐이고, 사이드카의 `base-url`·`auth-token`(필수)·`ca-cert`(선택)이 이 타입 Secret을 가리키며, 어느 컨테이너도 `claude-code` Secret을 참조하지 않음을 확인한다. 주 컨테이너에 직접 자격 증명을 넣으면 agent가 기동을 거부함을 확인한다.

### AC-G4: CRIU 비대상 — 파일시스템 아카이브, 전용 게이트
- **설명**: 동결·복원은 AC-E5의 파일시스템 아카이브를 그대로 쓴다 — 입장 차단과 큐 소진, 작업 디렉터리·HOME(`mcode`의 대화 기록이 있는 `~/.minimax` 포함)·누적 출력 버퍼의 아카이브, generation 펜스, 복원 후 커서 연속성. 아카이브 무결성 검사는 상태 트리의 형태(작업 디렉터리·HOME·런타임 상태)만 본다 — 이 타입의 HOME에는 플랫폼 소유 파일이 없으므로 Claude 관리형 설정을 요구하지 않는다. 외부 스토리지로의 전송은 **이 타입 전용 명시적 게이트** `MINIMAX_CODE_ARCHIVE_ENABLED`(기본 off) 뒤에 있다. 게이트가 꺼져 있으면 동결 요청은 셸 체크포인터로 넘어가지 않고 `ErrCheckpointDisabled`로 거부되며 파드는 유지된다.
- **달성 가치**: V2, V3
- **구체화 대상**: AC-B1(동결)·AC-B2(복원)·AC-B3(무결성) — `claude-code`와 같은 메커니즘
- **검증 방법**: 게이트 on에서 동결→복원 후 타입·모델이 유지되고, 작업 디렉터리·`~/.minimax`·출력·커서가 보존되며, 복원 후 첫 write에 `--continue`가 붙음을 확인한다. 게이트 off에서는 동결이 거부되고 파드가 남음을 확인한다. 복원 대상 파드가 CRIU가 아닌 아카이브 주석을 가짐을 확인한다.

### AC-G5: K3s MCP 플러그인(`mcp-servers`) — 토큰은 loopback 사이드카에만
- **설명**: `minimax-code` 세션도 `claude-code`처럼 홈랩 K3s MCP 도구를 쓴다. 도구 정의는 `claude-code`와 같은 private `plugin-marketplace`의 `mcp-servers` 플러그인에서 오지만, 토큰 배치가 다르다. `mcode`는 플러그인 MCP 설정의 헤더에서 `${VAR}`를 치환하지 않고 OAuth 필드(`oauthClientId` 등)가 있으면 서버를 거부하므로, 플러그인을 그대로 쓰려면 토큰을 HOME 아래 파일에 평문으로 적어야 하고 HOME은 아카이브된다(AC-G4). 그래서 토큰은 **워크로드 파드의 세 번째 컨테이너 `k3s-mcp-proxy`** 에만 둔다. 이 사이드카는 AC-E6 credential-proxy와 같은 구현(고정 HTTPS origin, 허용 목록 헤더만 전달, 호출자 `Authorization` 제거 후 `Bearer` 주입, 응답 토큰 redaction, `127.0.0.1` 바인드, 같은 하드닝·컨테이너 내부 readiness probe)을 쓰되, 허용 목록은 MCP Streamable HTTP 표면(`Accept`·`Content-Type`·`Mcp-Session-Id`·`Mcp-Protocol-Version`·`Last-Event-Id`·`User-Agent`)이고 포트는 `127.0.0.1:8093`, 상류는 K3s MCP의 origin(`https://homelab-k3s-mcp.llkm.nl`)이다. 토큰은 **이 타입 Secret의 선택 키 `k3s-mcp-token`** 에서 이 사이드카에만 주입된다(AC-G3의 Secret 분리 그대로 — `claude-code` Secret은 여전히 참조하지 않는다). 키가 없거나 비면 사이드카는 기동하되 `/healthz`가 `configured:false`를 알리고 그 밖의 요청에 503을 돌려준다 — 키 없는 배포는 실패가 아니라 플러그인 없는 세션이다. 주 컨테이너의 entrypoint는 사이드카의 `/healthz`를 (기동 순서 경합을 재시도로 흡수하며) 확인하고, `configured:true`면 ① loopback 사이드카를 거쳐 K3s MCP `github_app_installation_token`(`plugin-marketplace`/`contents:read`)을 호출하고 ② 단명 GitHub token을 marketplace URL에 범위가 걸린 프로세스 범위 Git `extraheader`로만 전달해 marketplace를 얕게 클론한 뒤 ③ `mcp-servers` 항목의 MCP 설정을 **K3s MCP URL과 같은 서버만** loopback URL(`http://127.0.0.1:8093` + 원래 경로)로 바꾸고 `oauthClientId`와 `Authorization` 헤더를 지워 상태 트리 밖 seed 디렉터리에 Claude 형식 플러그인(`.claude-plugin/plugin.json`)으로 쓴다. K3s MCP가 아닌 서버가 섞여 있으면 남의 토큰으로 라우팅하지 않도록 부트스트랩을 실패시킨다. 토큰과 header는 agent exec 전에 unset한다. agent는 기동·복원마다 seed를 HOME의 `~/.minimax/plugins/`에 통째로 복사하고 플랫폼 표식(`.session-platform-managed`)을 남긴다. seed가 없으면(토큰 없는 파드) 표식 있는 플러그인만 지우므로 복원된 HOME이 이전 파드의 플러그인을 되살리지 못하고, 사용자가 세션 안에서 직접 넣은 플러그인은 남는다. 이 플러그인 파일에는 비밀이 없으므로 아카이브되어도 된다. 주 컨테이너는 AC-G3대로 `K3S_MCP_TOKEN`이 있으면 여전히 기동을 거부한다.
- **달성 가치**: V1
- **구체화 대상**: AC-A1/A2의 세션 격리 — AC-E6의 사이드카 계약을 두 번째 상류(K3s MCP)로 재사용
- **검증 방법**: 제출된 pod spec에서 `k3s-mcp-proxy` 컨테이너만 이 타입 Secret의 선택 `k3s-mcp-token`을 참조하고 `127.0.0.1:8093`에 바인드하며, 다른 어느 컨테이너도 `K3S_MCP_TOKEN`을 받지 않음을 확인한다. 사이드카가 토큰 없이 `configured:false`·503으로 동작하고, 토큰이 있으면 호출자 `Authorization`과 허용 목록 밖 헤더를 지운 채 `Bearer`를 주입해 클라이언트 경로 그대로 상류 origin에 전달하며, `Mcp-Session-Id`가 양방향으로 보존되고 응답에 토큰 원문이 없음을 확인한다. entrypoint가 미구성 사이드카에서는 부트스트랩 없이 seed 없이 agent를 실행하고, 구성된 사이드카에서는 loopback으로만 K3s MCP를 호출해(자체 `Authorization` 없음) URL 범위 header로 클론한 뒤 loopback URL·OAuth 필드 제거·헤더 제거가 된 seed를 넘기며, 다른 서버가 섞인 플러그인은 거부함을 확인한다. agent의 seed 동기화가 설치·교체·표식 플러그인만 제거·symlink 거부를 지킴을 확인한다. 핀된 `mcode`(0.6.12)가 `exec --config`의 관리형 설정 아래서도 HOME의 `~/.minimax/plugins` Claude 형식 플러그인을 읽어 MCP `initialize`·`tools/list`를 호출하고 그 도구를 `mcp__<server>__<tool>`로 모델 요청에 싣는 것은 2026-10-23 shell 세션 실측으로 확인했다.

## 남은 일
- `docs/test/minimax-code-workload.md`(테스트 시나리오)와 그 e2e 매핑. 지금 검증은 단위·통합 테스트(`control-plane/test/minimax_code_orchestrator_test.go` 등)까지이며, 시나리오를 만들면 `scripts/e2e/check-scenario-mapping.sh`의 규칙에 따라 e2e 파일이나 유예 행이 함께 필요하다.
