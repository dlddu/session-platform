# PRD: 세션 워크로드 — MiniMax Code CLI (minimax-code)

> 대상 요구사항: 세션 워크로드 타입에 **MiniMax Code CLI(`mcode`) 에이전트** 추가
> (`shell`·`claude-code`·`approval-gated`와 병존하는 네 번째 타입, `claude-code-workload.md`와 대응 관계)

## 달성 가치
- **V8 목적에 맞는 작업 환경 선택** — 세션 생성 시 네 번째 작업 환경(MiniMax 모델로 도는 코딩 에이전트)을 고를 수 있게 하고, 선택과 무관하게 V1~V5 보장이 동일하게 성립함을 확정 (AC-G1)
- **V1 세션 격리** — 공급자 자격 증명을 도구가 도는 컨테이너 밖에 두고, 다른 타입의 자격 증명과도 섞지 않음 (AC-A1/A2 구체화)
- **V2 유휴 자원 회수** — `claude-code`와 같은 파일시스템 아카이브로 동결·회수 (AC-B1·A3의 타입별 경로)
- **V3 끊김 없는 세션 연속성** — 대화 문맥·작업 디렉터리·누적 출력이 동결·복원을 건너 보존됨 (AC-B2/B3 구체화), write/read를 프롬프트·실행 출력 시맨틱으로 확정 (AC-C2/C3 구체화)
- **V4 자유로운 멀티세션 전환** — 전환 후에도 동일한 offset 커서 규약으로 출력을 이어 읽음 (AC-C2 구체화)
- **V1 세션 격리 (도구 표면)** — `claude-code`가 플러그인으로 받는 홈랩 K3s MCP 도구를 이 타입에도 주되, 그 토큰은 워크로드 파드와 아카이브 밖에 둠 (AC-G5)
- **V1 세션 격리 (네트워크 경계)** — 워크로드 파드의 나가는 트래픽을 그 세션 헬퍼 파드로만 묶고, 외부에 닿는 세션 MCP 도구는 사람의 승인을 거침 (AC-G6 — `approval-gated`의 AC-F2~F5 재사용)

> 📌 **가치 연결 규칙**: `claude-code-workload.md`·`approval-gated-workload.md`와 동일하게, 각 AC의 달성 가치는
> 자신이 구체화하는 상위 AC의 가치를 상속한다. **AC-G1(타입 선택)만 예외**로 추상 가치 **V8**에 직접 연결한다
> — AC-E1·AC-F1과 같은 이유이며, 타입이 늘어도 `../values.md`는 변하지 않는다. 새 가치는 신설하지 않는다.

> 이 PRD는 **"같은 실행 모델 위에서 공급자와 CLI만 바꾼다"** 는 한 가지 성질을 세션 워크로드 타입으로 못박는다.
> 실행 모델(원샷 프롬프트·직렬 큐·커서 규약·대화 연속성·아카이브)은 `claude-code`와 같으므로 **복제하지 않고
> AC-E2~E5를 참조**하고, 파드 배치(헬퍼 파드·egress 경계·승인 게이트·공유 볼륨)는 `approval-gated`와 같으므로
> **AC-F2~F5를 참조**하며(AC-G6), 이 문서는 그 위에 얹히는 차이만 적는다. 차이는 다섯이다 — 실행하는 CLI와 그
> 출력의 투영(AC-G2), 자격 증명을 담는 Secret(AC-G3), 아카이브 게이트(AC-G4), K3s MCP 도구의 배선(AC-G5),
> 그리고 K3s MCP 도구가 세션 승인 게이트를 지나지 않는다는 것(AC-G6). credential-proxy는 **동작 계약은 AC-E6,
> 배치는 AC-F6 그대로**(헬퍼 파드의 컨테이너)이고, 읽는 Secret만 이 타입 전용이다.

## Acceptance Criteria

### AC-G1: 워크로드 타입 `minimax-code` 선택
- **설명**: 세션 생성 요청의 `workloadType` 허용값에 `minimax-code`가 추가되어 `shell`·`claude-code`·`approval-gated`·`minimax-code` 네 가지가 된다. 필드 생략만 `shell`로 해석하는 규칙, explicit empty/null/비문자열·허용값 밖 입력을 pod 생성 전에 400으로 거부하는 규칙, 타입 불변 규칙은 AC-E1과 같다. control plane은 이 타입에 대해 **워크로드 파드**(주 컨테이너 하나, 사이드카 없음)와 **그 세션 전용 헬퍼 파드 하나**(세션 MCP·credential-proxy·K3s MCP 프록시 컨테이너 셋, AC-G6)를 함께 프로비저닝한다. 배포가 이 타입의 이미지를 설정하지 않았으면 생성은 셸로 되돌아가지 않고 실패한다. `model` 필드 계약(생략=`platform-default` 별칭, 불변, pattern 검증, 구체 값은 Secret 기본값보다 우선)은 AC-E6을 그대로 따른다. 다만 Claude Code와 달리 `mcode`는 공급자 항목을 통해서만 모델을 고르므로, `platform-default`이면서 Secret에 `model` 키가 없거나 비어 있을 때는 `--model` 생략 대신 data plane 내장 기본값 `MiniMax-M3.1-Flash`를 쓴다.
- **달성 가치**: V8, V1
- **구체화 대상**: AC-A1의 "실제 세션 워크로드" — 이제 네 갈래로 분기한다 (AC-D1=`shell`, AC-E2~E6=`claude-code`, AC-F2~F6=`approval-gated`, AC-G2~G6=`minimax-code`)
- **검증 방법**: `workloadType=minimax-code`로 세션을 생성하면 워크로드 파드(컨테이너 하나)와 헬퍼 파드(컨테이너 셋)가 함께 기동되고 조회 응답의 타입·모델이 기대값임을 확인한다. 이미지 미설정 배포에서는 pod 없이 실패함을 확인한다. 잘못된 model은 400임을 확인한다.

### AC-G2: write = `mcode exec` 1회 실행, read = assistant 메시지 텍스트
- **설명**: 큐·바운드·커서·대화 연속성은 AC-E2~E4를 그대로 따른다(1 MiB 프롬프트 413, 64개·8 MiB 큐 429, 실행당 16 MiB·세션 256 MiB 출력 한도, 첫 **성공** 실행 이후에만 `--continue`). 한 번의 실행은 다음과 같다:

  ```
  mcode exec --config <관리형 설정> \
    --model custom_provider:session-platform/<모델> \
    --permission full --effort xhigh --output-format stream-json [--continue] -- "<payload>"
  ```

  `--` 뒤 payload는 옵션이 아닌 단일 positional argv다. 권한은 `full`, 추론 강도는 `xhigh`로 **고정**한다 — 실행은 무인이므로 사람에게 묻고 멈추는 모드(`smart`는 사람 없이 대부분의 셸 명령을 거부한다)는 turn을 실패시킬 뿐이고, 도구의 경계는 실제 자격 증명을 갖지 않고(AC-G3) 헬퍼 파드 밖으로 나갈 수 없는(AC-G6) 세션 파드다. 고정값이므로 상위 버전의 기본값 변경이 실행 방식을 조용히 바꾸지 못한다. `mcode`는 모델 정의에 추론 강도 선택지가 없으면 `--effort`를 거부하고, 토큰 한도가 없으면 응답을 16384 토큰으로 자르므로, 관리형 설정은 세션 모델에 추론 강도 선택지(항상 켜진 adaptive thinking)를 선언하고 한도가 알려진 모델(`MiniMax-M3.1-Flash`: 컨텍스트 512k·출력 128k)에는 그 한도도 선언한다. data plane은 stdout JSONL에서 `agent_message` 항목의 `contentDelta`만 순서대로 투영하고, 항목이 끝날 때(`item.completed`) 줄바꿈을 맞춘다. 델타 없이 끝난 항목은 완료 레코드의 `content`로 한 번만 투영한다. reasoning·tool_call·생명주기 레코드는 사용자 출력이 아니다. 실패한 turn은 `mcode`가 stderr에도 보고하므로 별도로 투영하지 않으며, stderr는 AC-E3대로 진단 출력으로 같은 버퍼에 합쳐진다.
- **달성 가치**: V3, V4
- **구체화 대상**: AC-C2(read)·AC-C3(write) — AC-E2/E3의 투영 규칙을 이 CLI의 스트림 형식에 맞춰 바꾼 것
- **검증 방법**: 모든 write의 argv에 `--permission full --effort xhigh`가 있고, 관리형 설정의 세션 모델 정의가 `xhigh`를 추론 강도 선택지로 담으며 `MiniMax-M3.1-Flash`에는 512k/128k 한도를 담음을 확인한다. 첫 write의 argv에 `--continue`가 없고 성공 후 두 번째 write에는 있음을 확인한다. `--`로 시작하는 프롬프트가 옵션으로 해석되지 않음을 확인한다. reasoning·tool_call이 섞인 스트림에서 assistant 텍스트만, 중복 없이, 메시지마다 줄바꿈되어 투영됨을 확인한다.

### AC-G3: 공급자 자격 증명은 이 타입 전용 Secret, 배치는 AC-F6 헬퍼 파드
- **설명**: 플랫폼 Secret `minimax-code-credentials`(이름은 control plane 설정으로 바꿀 수 있다)의 키 이름과 의미는 AC-E6과 같다 — 필수 `base-url`·`auth-token`은 헬퍼 파드의 credential-proxy 컨테이너에만, 선택 `ca-cert`도 그 컨테이너에만, 선택 `model`은 `platform-default` 세션의 주 컨테이너에만 주입된다. `base-url`은 MiniMax의 Anthropic 호환 엔드포인트(예: `https://api.minimax.io/anthropic`)이고 `auth-token`은 Token Plan **Subscription Key** 또는 종량제 API 키다. proxy는 AC-E6 그대로 HTTPS origin을 고정하고 허용 목록 헤더만 전달한 뒤 토큰을 `Authorization: Bearer`로 주입하는데, MiniMax는 두 종류의 키 모두 이 형식을 받는다. 주 컨테이너는 헬퍼 파드의 프록시 주소(`ANTHROPIC_BASE_URL=http://<헬퍼 파드 IP>:8091`, loopback이면 기동 거부)와 비밀이 아닌 placeholder만 보고, 시작 시 `ANTHROPIC_API_KEY`·`CLAUDE_CODE_OAUTH_TOKEN`·`MINIMAX_API_KEY`·`MCODE_PROVIDER_API_KEY`·`K3S_MCP_TOKEN` 등 두 CLI가 스스로 읽을 수 있는 자격 증명 변수가 하나라도 있으면 기동을 거부한다. control plane은 `minimax-code` Secret 이름이 `claude-code` Secret 이름과 같으면 기동을 거부한다. `claude-code`처럼 주 컨테이너가 K3s MCP 토큰을 받아 플러그인을 부트스트랩하는 경로는 이 타입에 없다 — K3s MCP 도구는 토큰을 헬퍼 파드에 두는 AC-G5로만 들어온다. `mcode`가 읽는 설정은 data plane이 기동할 때마다 **상태 트리 바깥**에 다시 쓰는 관리형 파일 하나로, 헬퍼 파드의 proxy를 가리키는 Anthropic-messages 공급자 하나와 세션 모델만 선언하고 데이터 기여(data contribution)와 자가 업데이트를 끈다. agent는 `MCODE_DISABLE_EVAL_CAPTURE=1`도 설정해 실행 단계 보고 업로드를 막는다. 이 파일은 아카이브되지 않으므로 복원이 낡은 설정을 되살릴 수 없다. **두 타입의 Secret은 서로 섞이지 않는다** — `minimax-code` 파드의 어떤 컨테이너도 `claude-code` Secret을 참조하지 않는다.
- **달성 가치**: V1
- **구체화 대상**: AC-A1/A2의 세션 격리 — AC-E6의 동작 계약을 AC-F6의 배치로, 다른 공급자 Secret에 재사용
- **검증 방법**: 제출된 pod spec에서 주 컨테이너의 Secret 참조가 이 타입 Secret의 선택 `model`뿐이고, 헬퍼 파드 credential-proxy 컨테이너의 `base-url`·`auth-token`(필수)·`ca-cert`(선택)이 이 타입 Secret을 가리키며, 두 파드의 어느 컨테이너도 `claude-code` Secret을 참조하지 않음을 확인한다. 주 컨테이너에 직접 자격 증명이나 loopback 프록시 주소를 넣으면 agent가 기동을 거부함을 확인한다.

### AC-G4: CRIU 비대상 — 파일시스템 아카이브, 전용 게이트
- **설명**: 동결·복원은 AC-E5의 파일시스템 아카이브를 그대로 쓴다 — 입장 차단과 큐 소진, 작업 디렉터리·HOME(`mcode`의 대화 기록이 있는 `~/.minimax` 포함)·누적 출력 버퍼의 아카이브, generation 펜스, 복원 후 커서 연속성. 아카이브 무결성 검사는 상태 트리의 형태(작업 디렉터리·HOME·런타임 상태)만 본다 — 이 타입의 HOME에는 플랫폼 소유 파일이 없으므로 Claude 관리형 설정을 요구하지 않는다. 외부 스토리지로의 전송은 **이 타입 전용 명시적 게이트** `MINIMAX_CODE_ARCHIVE_ENABLED`(기본 off) 뒤에 있다. 게이트가 꺼져 있으면 동결 요청은 셸 체크포인터로 넘어가지 않고 `ErrCheckpointDisabled`로 거부되며 파드는 유지된다. 동결은 헬퍼 파드도 함께 회수하고(AC-F4), 복원은 새 헬퍼 파드와 새 워크로드 파드를 함께 세운다. **헬퍼 파드와 공유하는 볼륨(AC-F5)의 내용은 이 아카이브에 들어가지 않는다** — `approval-gated`의 AC-F5가 요구하는 공유 볼륨 아카이브는 아직 어느 타입에도 구현되지 않았고, 이 타입도 같은 미구현을 공유한다(아래 남은 일).
- **달성 가치**: V2, V3
- **구체화 대상**: AC-B1(동결)·AC-B2(복원)·AC-B3(무결성) — `claude-code`와 같은 메커니즘
- **검증 방법**: 게이트 on에서 동결→복원 후 타입·모델이 유지되고, 작업 디렉터리·`~/.minimax`·출력·커서가 보존되며, 복원 후 첫 write에 `--continue`가 붙음을 확인한다. 게이트 off에서는 동결이 거부되고 파드가 남음을 확인한다. 복원 대상 파드가 CRIU가 아닌 아카이브 주석을 가짐을 확인한다.

### AC-G5: K3s MCP 도구 — 토큰은 헬퍼 파드의 K3s MCP 프록시에만
- **설명**: `minimax-code` 세션도 `claude-code`처럼 홈랩 K3s MCP 도구를 쓴다. 토큰 배치가 다르다. `mcode`는 플러그인 MCP 설정의 헤더에서 `${VAR}`를 치환하지 않고 OAuth 필드(`oauthClientId` 등)가 있으면 서버를 거부하므로, 토큰을 CLI가 직접 쓰게 하려면 HOME 아래 파일에 평문으로 적어야 하고 HOME은 아카이브된다(AC-G4). 그래서 토큰은 **헬퍼 파드의 세 번째 컨테이너 `k3s-mcp-proxy`** 에만 둔다. 이 컨테이너는 AC-E6 credential-proxy와 같은 구현(고정 HTTPS origin, 허용 목록 헤더만 전달, 호출자 `Authorization` 제거 후 `Bearer` 주입, 응답 토큰 redaction, 같은 하드닝·컨테이너 내부 readiness probe)과 AC-F6의 헬퍼 배치(파드 IP 바인드, loopback 바인드 거부)를 쓰되, 허용 목록은 MCP Streamable HTTP 표면(`Accept`·`Content-Type`·`Mcp-Session-Id`·`Mcp-Protocol-Version`·`Last-Event-Id`·`User-Agent`)이고 포트는 `8093`, 상류는 K3s MCP의 origin(`https://homelab-k3s-mcp.llkm.nl`)이다. 토큰은 **이 타입 Secret의 선택 키 `k3s-mcp-token`** 에서 이 컨테이너에만 주입된다(AC-G3의 Secret 분리 그대로 — `claude-code` Secret은 여전히 참조하지 않는다). 키가 없거나 비면 프록시는 기동하되 `/healthz`가 `configured:false`를 알리고 그 밖의 요청에 503을 돌려준다 — 키 없는 배포는 실패가 아니라 K3s 도구 없는 세션이다.

  **프록시는 상류로 가는 모든 요청에 `X-Session-Platform-Session-Id: <세션 id>`를 붙인다.** 세션 id는 control plane이 이 컨테이너에 `SESSION_ID`(Secret이 아닌 평문 값, 세션 MCP 컨테이너와 같은 이름)로 넣고, 복원이 새로 만드는 헬퍼 파드에도 같은 값이 들어간다. 헤더는 허용 목록 필터가 호출자 헤더를 지운 **뒤에** 설정되므로 워크로드가 같은 이름의 헤더를 보내도 상류에 닿지 않고 덮어쓰이며, 빼게 할 수도 없다. `SESSION_ID`가 없거나 비거나 헤더 값으로 쓸 수 없는 문자를 담으면 헤더를 생략할 뿐 프록시는 그대로 동작한다. 이 값은 **표시용**이다 — homelab-k3s-mcp가 PocketAide 승인 요청에 실어, PocketAide가 승인 상세 화면에 세션 제목(session-platform 세션 `name`)을 보여 주는 데 쓴다. 인가 판단에는 쓰지 않는다. repo access를 켠 `approval-gated` 헬퍼 파드의 같은 컨테이너(`docs/prd/repo-access.md` AC-H5)도 같은 구현이라 헤더를 붙인다 — 그쪽 호출자는 세션 MCP의 토큰 발급뿐이라 무해하다. homelab-k3s-mcp는 모르는 헤더를 무시하므로 이 변경은 그쪽 배포 순서와 무관하게 나갈 수 있다.

  **plugin-marketplace 부트스트랩은 이 프록시 컨테이너가 한다.** 워크로드 파드는 `github.com`에 닿지 못하지만(AC-G6) 헬퍼 파드의 egress는 열려 있고 이 컨테이너는 이미 K3s 토큰을 쥐고 있기 때문이다. 토큰이 있으면 컨테이너는 기동 직후 ① 자기 프록시 핸들러를 **프로세스 안에서** 거쳐 K3s MCP `github_app_installation_token`(`plugin-marketplace`/`contents:read`)을 부르고 — 부트스트랩 코드는 K3s 토큰을 직접 다루지 않는다 — ② 단명 GitHub token을 marketplace URL에 범위가 걸린 프로세스 범위 Git `extraheader`로만 넘겨(credential helper 비움, 프롬프트 끔) 전용 emptyDir에 얕게 클론하고 ③ 선택된 플러그인(`MINIMAX_CODE_PLUGINS`, 기본 `mcp-servers`)마다 manifest와 그 경로 필드(`skills`·`commands`·`agents`·`mcpServers`)가 가리키는 파일만 `source` 안에서 읽어 **비밀 없는 번들**로 만든다. 경로가 marketplace 밖으로 나가거나 symlink면 거부하고, hook은 CLI가 스스로 실행하는 명령이라 싣지 않으며, 번들은 크기·파일 수 상한을 갖는다. 클론은 번들을 만든 뒤 지운다. 번들은 같은 포트의 `GET /plugin-seed`로 내놓는다 — 부트스트랩 중에는 503, 끝나면 `ready`, 몇 번 재시도해도 실패하면 `failed`, 토큰이 없으면 `unconfigured`. 실패는 프록시를 죽이지 않는다.

  워크로드 파드의 agent는 기동·복원마다 상태 트리 밖 staging 디렉터리에 `/plugin-seed`의 번들(부트스트랩이 끝날 때까지 기다린다)을 Claude 형식 플러그인으로 쓰고 HOME의 `~/.minimax/plugins/`에 통째로 복사한 뒤 플랫폼 표식(`.session-platform-managed`)을 남긴다. **MCP 서버는 플러그인에 남기지 않고 `mcode`의 서버 목록 `~/.minimax/mcp.json`에 등록한다** — 세션 MCP(`session-platform-session-mcp` → 세션 MCP `/mcp`, AC-G6)와 번들이 선언한 서버 전부다. 번들에서 꺼낸 서버 중 URL이 K3s MCP(`K3S_MCP_URL`)와 같은 서버는 프록시 주소 + 원래 경로로 바꾸고 `oauthClientId`와 `Authorization` 헤더를 지운다. 그 밖의 서버는 워크로드 파드가 닿지 못하고 남의 토큰으로 라우팅해서도 안 되므로 경고와 함께 뺀다. 플러그인에서는 서버 파일과 manifest의 `mcpServers` 선언을 지우고, 스킬 같은 나머지 파일은 그대로 설치한다 — marketplace에 플러그인이 늘면 `MINIMAX_CODE_PLUGINS`에 이름만 더하면 된다. 서버 이름은 marketplace 정의를 따르므로 `claude-code`와 같은 `mcp__k3s__*` 도구 이름이 된다. 번들이 `failed`·`unconfigured`면 플러그인 없이 세션 MCP만 등록하고, 프록시에 끝내 닿지 못하면(헬퍼 파드가 Ready인 뒤에야 워크로드 파드가 생기므로 정상 경로에서는 일어나지 않는다) 기동을 실패시킨다 — 키 없음과 고장을 구분한다. 표식 있는 플러그인만 교체·제거하므로 복원된 HOME이 이전 파드의 플러그인(이전 헬퍼 주소)을 되살리지 못하고, 사용자가 세션 안에서 직접 넣은 플러그인은 남는다. `mcp.json`도 같은 규칙이다 — 플랫폼 항목은 이름으로 병합하며 각 항목에 표식 키(`sessionPlatformManaged`)를 달고, 이번 기동이 등록하지 않는 표식 항목(이전 파드의 서버)은 지우며, 표식 없는 사용자 항목과 파일의 다른 키는 그대로 둔다. 플러그인 파일과 `mcp.json`에는 비밀이 없으므로 아카이브되어도 된다. 주 컨테이너는 AC-G3대로 `K3S_MCP_TOKEN`이 있으면 여전히 기동을 거부한다.

  > ✅ **확정 (2026-10-23, marketplace 부트스트랩의 자리)**: 초안(#156)은 주 컨테이너 entrypoint가 loopback 사이드카를
  > 거쳐 marketplace를 클론했다. AC-G6으로 워크로드 파드의 egress가 헬퍼 파드로 묶이면서 그 자리에서는 클론할 수
  > 없게 되어, 클론을 **K3s 토큰을 이미 쥔 헬퍼 파드의 프록시 컨테이너**로 옮겼다. marketplace에 나중에 더할
  > 플러그인이 그대로 따라오게 하려고 플러그인을 직접 생성하는 대안은 택하지 않았다. 초안의 「K3s MCP가 아닌 서버가
  > 섞이면 부트스트랩 실패」는 「그 서버만 빼고 경고」로 바꿨다 — 플러그인이 늘어날 때 하나의 낯선 서버가 나머지
  > 전부를 막지 않게 하려는 것이고, 남의 토큰으로 라우팅하지 않는다는 성질은 그대로다.

  > ✅ **확정 (2026-10-24, MCP 서버의 등록 자리)**: 처음에는 세션 MCP도 플러그인(`session-platform-session-mcp`)으로,
  > K3s MCP도 번들 플러그인 안에서 등록했다. 운영 세션에서 모델 요청에 **MCP 도구가 하나도 실리지 않았고** 오류는
  > 어디에도 없었다. 핀된 `mcode`(0.6.12)는 플러그인 MCP 서버를 백그라운드에서 조회하고, turn은 시작 시점에 준비된
  > 목록만 쓴다 — `tools/list`가 아직 답하지 않았으면 빈 목록이다. `exec` 하나가 프로세스 하나·turn 하나라 모든
  > 프롬프트가 자기 조회와 경주하고, loopback에서는 이기지만(2026-10-23 실측이 통과한 이유) 왕복이 수백 ms만 되어도
  > 진다. `~/.minimax/mcp.json`의 서버는 turn이 직접 조회하며 끝날 때까지 기다린다(http는 고정 2초 한도). 그래서
  > 서버만 그 파일로 옮겼다. 이전 HOME에서는 `mcode`의 영구 이름 예약(`~/.minimax/mcp-runtime-names.json`)이
  > 플러그인 시절 이름을 쥐고 있어 같은 서버가 `<이름>_2`로 보일 수 있다 — 기능은 같고, 새 세션에는 해당하지 않는다.
- **달성 가치**: V1
- **구체화 대상**: AC-A1/A2의 세션 격리 — AC-E6의 프록시 계약을 두 번째 상류(K3s MCP)로, AC-F6의 헬퍼 배치로 재사용
- **검증 방법**: 제출된 pod spec에서 헬퍼 파드의 `k3s-mcp-proxy` 컨테이너만 이 타입 Secret의 선택 `k3s-mcp-token`을 참조하고 헬퍼 배치로 `:8093`에 바인드하며 자기만 마운트하는 emptyDir에 클론 경로를 가지고, 두 파드의 다른 어느 컨테이너도 `K3S_MCP_TOKEN`을 받지 않음을 확인한다. 프록시가 토큰 없이 `configured:false`·503(`/plugin-seed`는 `unconfigured`)으로 동작하고, 토큰이 있으면 호출자 `Authorization`과 허용 목록 밖 헤더를 지운 채 `Bearer`를 주입해 클라이언트 경로 그대로 상류 origin에 전달하며, `Mcp-Session-Id`가 양방향으로 보존되고 응답에 토큰 원문이 없음을 확인한다. 헬퍼 파드의 `k3s-mcp-proxy` 컨테이너(새 세트와 복원 세트 모두)가 평문 `SESSION_ID`를 받고, 프록시가 상류 요청마다 그 값으로 `X-Session-Platform-Session-Id`를 정확히 하나 붙이며, 호출자가 보낸 같은 이름의 헤더는 상류에 닿지 않고, `SESSION_ID`가 없으면 헤더도 없음을 확인한다. 부트스트랩이 자체 `Authorization` 없이 프록시 핸들러로 GitHub token을 받고, URL 범위 header로 클론하며(실패 메시지에 header 없음), 선택 플러그인의 경로 필드 파일만 번들에 담고 hook·symlink·marketplace 밖 경로를 거부하며, 실패 시 `failed`를 알림을 확인한다. agent가 번들의 플러그인을 설치하면서 MCP 서버를 플러그인에서 꺼내 `mcp.json`에 등록하고, 그중 K3s MCP 서버만 프록시로 바꾸고 인증 필드를 지우며 다른 서버는 빼고, `failed`·`unconfigured`에서는 세션 MCP만 등록하고, 닿지 않는 프록시에서는 기동을 실패시키며, 플러그인 밖 경로·세션 MCP와 같은 서버 이름을 거부함을 확인한다. 플러그인 동기화가 설치·교체·표식 플러그인만 제거·symlink 거부를 지키고, `mcp.json` 병합이 사용자 항목·다른 키를 보존하고 이전 파드의 표식 항목만 지우며 읽을 수 없는 파일(비JSON·symlink 등)에 쓰지 않음을 확인한다. 핀된 `mcode`(0.6.12)가 `exec --config`의 관리형 설정 아래서 `~/.minimax/mcp.json`의 서버(표식 키 포함)를 turn마다 조회해 `mcp__<server>__<tool>`로 모델 요청에 싣고, 요청당 300 ms 지연을 넣어도 실으며, 도구 45개짜리 서버도 지연 노출 없이 inline으로 싣는 것은 2026-10-24 로컬 실측(가짜 공급자로 요청 본문 기록)으로 확인했다. 같은 조건에서 플러그인 등록은 요청당 150 ms 지연부터 MCP 도구를 하나도 싣지 않았다.

### AC-G6: 헬퍼 파드·egress 경계·승인 게이트 — `approval-gated`의 AC-F2~F5를 그대로, K3s MCP만 예외
- **설명**: 이 타입의 파드 배치는 `approval-gated`와 같다. **AC-F2**(워크로드 파드 egress는 kube-dns와 자기 세션 헬퍼 파드의 포트로만, 헬퍼 파드 ingress는 자기 세션 워크로드 파드로부터만 — 둘 다 헬퍼 파드가 소유하는 세션별 네트워크 정책), **AC-F3**(세션 MCP의 외부 도구는 호출마다 승인 게이트를 지나고, 대기·결정은 같은 출력 바이트열에 in-band 마커로 남으며, 승인 대기 중에는 유휴 카운트가 진행되지 않는다), **AC-F4**(헬퍼 파드는 세션 전속 보조 파드이고 동결·삭제 때 워크로드 파드와 함께 회수되며 복원 때 새로 선다), **AC-F5**(배포가 클래스를 지정하면 워크로드 파드와 세션 MCP 컨테이너가 세션 전용 RWX 볼륨을 같은 경로에 마운트한다 — 아카이브 포함 여부는 AC-G4)를 문면 그대로 따른다. 게이트웨이 3종은 AC-F6대로 세션 MCP 컨테이너에만 들어간다. 다른 것은 셋이다.

  1. **헬퍼 파드의 컨테이너가 셋이다** — 세션 MCP, credential-proxy(이 타입 Secret, AC-G3), K3s MCP 프록시(AC-G5). 그래서 네트워크 정책의 헬퍼 포트도 `8091`·`8092`에 **`8093` 하나가 더해진다.** `approval-gated`의 경계는 두 포트 그대로다.
  2. **에이전트가 세션 MCP를 등록하는 방식** — `claude-code` CLI의 관리형 설정이 아니라 AC-G5의 `~/.minimax/mcp.json` 등록(서버 이름 `session-platform-session-mcp`)으로 한다. 승인 대기 마커는 같은 notice 피드를 같은 방식으로 읽어 쓴다.
  3. **K3s MCP 도구는 세션 승인 게이트를 지나지 않는다.** 워크로드 파드는 헬퍼 파드의 K3s MCP 프록시를 거쳐 K3s MCP에 직접 MCP 요청을 보낸다. 근거: K3s MCP는 자체 승인 게이트(`dlddu/homelab-k3s-mcp`의 `prd-approval-gate`)로 쓰기 도구를 이미 사람의 승인 뒤에 둔다 — 같은 행동을 두 게이트가 연달아 묻는 것은 승인 피로만 늘리고 판정 정보는 늘리지 않는다(2026-10-23 사용자 결정). 이것은 `approval-gated`의 **AC-F6 2026-09-21 확정 항목의 C2(에이전트가 게이트 밖에서 그 자격 증명의 힘에 닿지 못함)를 이 타입에서는 적용하지 않는다는 뜻**이며, 범위는 K3s MCP 프록시 하나다. C1(헬퍼 파드의 그 컨테이너에만)·C3(상시 권한을 넓히지 않음 — 토큰은 `claude-code`가 이미 쓰는 것과 같은 범위)·C4(노출 금지)는 그대로 성립한다. 프록시의 상류는 시작할 때 고정된 K3s MCP origin 하나뿐이라, 이 예외가 「임의 외부 목적지」로 번지지는 않는다 — AC-F2가 지키는 「에이전트가 스스로 밖에 닿을 경로가 없다」는 성질에서 빠지는 것은 **K3s MCP라는 목적지 하나**이고, 그 목적지의 쓰기는 자체 게이트가 막는다. `approval-gated`의 AC-F6·`../session-mcp-tool-surface.md`의 「K3s MCP 재노출」(세션 게이트를 씌운 도구별 위임)은 그 타입의 계약으로 그대로 남는다.
- **달성 가치**: V1, V2
- **구체화 대상**: AC-A1의 "실제 세션 워크로드"가 가질 수 있는 네트워크 경계·보조 파드(AC-A2)·유휴 기준(AC-B1) — AC-F2~F5의 재사용
- **검증 방법**: 제출된 오브젝트에서 헬퍼 파드가 세션 라벨·`minimax-code` 타입 라벨을 갖고 컨테이너 셋(세션 MCP·credential-proxy·k3s-mcp-proxy)을 가지며 서비스 어카운트 토큰을 마운트하지 않음, 워크로드 파드에 사이드카가 없고 세 헬퍼 주소(`:8091`·`:8092`·`:8093`)를 env로 받음, 워크로드 egress·헬퍼 ingress 정책이 헬퍼 포트 셋을 정확히 허용하고 `approval-gated`의 정책은 두 포트 그대로임, 게이트웨이 3종이 세션 MCP 컨테이너에만 있음, 공유 볼륨 클래스가 있으면 워크로드 파드와 세션 MCP가 같은 claim을 같은 경로에 마운트함을 확인한다. 승인 대기를 보고하는 agent에 대해 유휴 회수가 세션을 동결하지 않고 `lastAccess`를 갱신하며, 대기가 끝나면 갱신이 멈춤을 확인한다. AC-F2의 실클러스터 egress 실측(외부 목적지 차단, K3s MCP 프록시 경유만 성립)은 `approval-gated`와 같은 미검증 상태를 공유한다.

## 남은 일
- **공유 볼륨 아카이브** — AC-F5가 요구하는 「공유 볼륨 내용의 아카이브 포함」이 이 타입에도 없다(AC-G4). 승인된 `git_clone`·큰 `web_fetch_get` 결과물은 동결 뒤 사라진다.
- **웹 화면** — SPA의 게이트 전용 표면(`isGatedWorkload`)과 새 세션 카드 설명은 아직 `approval-gated`만 게이트로 그린다. 승인 대기 마커는 출력 스트림에 그대로 보인다.
- **marketplace 스킬 로드 실측** — 핀된 `mcode`가 MCP 서버를 뺀 marketplace 플러그인의 스킬을 읽는지는 운영 반영 뒤 세션에서 확인한다.
- **K3s MCP 조회 2초 한도** — `mcp.json`의 http 서버는 turn마다 `initialize`·`tools/list`가 2초 안에 끝나야 그 turn에 실린다(`mcode`에 조정 수단 없음). K3s MCP는 헬퍼 파드 프록시를 거쳐 외부 origin까지 가므로, 상류가 느린 turn에는 K3s 도구만 빠질 수 있다(세션 MCP는 별개로 조회된다).
- `docs/test/minimax-code-workload.md`(테스트 시나리오)와 그 e2e 매핑. 지금 검증은 단위·통합 테스트(`control-plane/test/minimax_code_orchestrator_test.go` 등)까지이며, 시나리오를 만들면 `scripts/e2e/check-scenario-mapping.sh`의 규칙에 따라 e2e 파일이나 유예 행이 함께 필요하다.
