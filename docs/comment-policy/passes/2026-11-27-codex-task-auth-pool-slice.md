# 판정 패스 — codex-task·에이전트 인증 풀 슬라이스 (2026-11-27)

범위(22행 · **400줄 판정, 제거 120 · 유지 280**): 이번 트리거(#207·#208·#209·#210·#211 머지)가 원장에 새로
든 2026-11-07 행들 가운데 **L 표면 20행 396줄**과, 같은 파일의 **E 표면 2행 4줄**이다.
`control-plane/internal/tenancy/tenancy_test.go`(4줄)는 열린 PR #272(형제 task `rct_20261105-0001`)가
손대는 파일이라, `data-plane/cmd/agent/codex_task_test.go`(2줄)는 열린 PR #213(`codex-task-web-search`)가
손대는 파일이라 빠졌다. 남은 L 미판정 251행 10,876줄과 D 4행 168줄·E 44행 128줄은 누적분이라 정의
「판정 슬라이스」의 다음 덩어리로 감지 단계가 잇는다.

모델 정의의 「판정 슬라이스」 예산(400줄)을 다 채웠다 — L 396줄에 같은 파일의 E 4줄을 더해 정확히 400줄이다.
덩어리는 원장 행 순서(범위의 첫 파일 경로 사전순)를 따라 채웠고, 예산을 넘기 직전에서 잘랐다.

판정은 필요성 시험으로 한다 — 각 주석에 「지우면 이 코드를 고치러 온 사람이 틀린 판단을 하거나 확인에 큰 비용을
치르는가」를 묻는다. 코드는 바꾸지 않았다(주석 삭제와 gofmt 정렬만).

## 제거 — 120줄

좌표는 「이 내용을 이미 말하는 곳」이다. AC-N1~N6·AC-M1~M4·AC-E7·AC-E8·AC-J1·AC-J6·AC-L1의 정본은
`docs/prd/agent-auth-pool.md`·`docs/prd/codex-task-workload.md`·`docs/prd/claude-code-workload.md`다.

| 파일 | 제거 | 줄 | 좌표 |
| --- | --- | ---: | --- |
| `control-plane/cmd/control-plane/codex_task.go` | `authPoolEnv`·`slots` doc, `pool` doc, `archiveEnabled` doc, `offered` doc, `objects` doc, `claudeAuthConfig` doc, `claudeCodeModelConfig` doc | 9 | 타입·필드·함수 이름과 바로 아래 코드, `docs/prd/agent-auth-pool.md` AC-N1 |
| `control-plane/internal/adapter/k8s/auth_pool.go` | `AuthSlotLeaseName`·`AuthPoolConfig`·`NewLeaseAuthPool`·`leases`·`ClaudeCodeAuthMode`·`WithClaudeCodeAuthMode`·`CodexManagerMonitorLaneConfigKey`·`CodexSessionDBRawSQLConfigKey`·`CodexChatSessionDBMCPURL` doc, 구분선 3줄 | 12 | 심볼 이름과 바로 아래 코드, `docs/prd/agent-auth-pool.md` AC-N1~N6 |
| `control-plane/internal/adapter/k8s/auth_pool_memory.go` | `NewMemoryAuthPool`·`Holder`·`HeldBy` doc | 3 | 심볼 이름과 바로 아래 코드 |
| `control-plane/test/harness_auth_pool_test.go` | `heldSlotOf` doc | 1 | 심볼 이름과 바로 아래 코드 |
| `data-plane/cmd/agent/auth_slot.go` | `authSlot`·`dir`·`file`·`homeRel`·`poll`·`homes`·`authSlotHome`·`signedIn`·`homeLocked` doc, 구분선 1줄 | 10 | 구조체·필드 이름과 바로 아래 코드, `docs/prd/agent-auth-pool.md` AC-N4 |
| `data-plane/cmd/agent/claude_plugins_test.go` | `testClaudePlugin` doc, `t.Setenv` doc, 시나리오 표제 4건 | 12 | 각 `Test*` 이름과 단언, `docs/prd/claude-code-workload.md` AC-L1 |
| `control-plane/internal/adapter/k8s/auth_pool_test.go` | 시나리오 표제 8건 | 11 | 각 `Test*` 이름과 단언, `control-plane/internal/adapter/k8s/auth_pool.go` 의 유지 주석 |
| `control-plane/internal/api/plugins_test.go` | 시나리오 표제 3건 | 6 | 각 `Test*` 이름과 단언, `docs/prd/codex-task-workload.md` AC-L1 |
| `control-plane/internal/api/reasoning_effort_test.go` | 시나리오 표제 3건 | 5 | 각 `Test*` 이름과 단언, `docs/prd/codex-task-workload.md` AC-E8·AC-J1 |
| `control-plane/internal/service/auth_pool_test.go` | 시나리오 표제 8건 | 8 | 각 `Test*` 이름과 단언, `docs/prd/agent-auth-pool.md` AC-N2·AC-N3 |
| `control-plane/internal/session/plugins_test.go` | 시나리오 표제 1건 | 3 | `Test*` 이름과 단언, `docs/prd/codex-task-workload.md` AC-L1 |
| `control-plane/internal/session/workload_catalog_test.go` | 시나리오 표제 4건 | 7 | 각 `Test*` 이름과 단언, `docs/prd/codex-task-workload.md` AC-J1 |
| `control-plane/test/auth_pool_orchestrator_test.go` | 파일 머리 블록, `assertSlotMount` doc, 시나리오 표제 2건 | 9 | 각 `Test*` 이름과 단언, `docs/prd/agent-auth-pool.md` AC-M4·AC-E7 |
| `control-plane/test/e2e_codex_task_4_effort_test.go` | `configEfforts`·`assertNoSessionNamed` doc | 2 | 헬퍼 이름과 바로 아래 코드 |
| `control-plane/test/plugins_orchestrator_test.go` | 파일 머리 블록 | 3 | 각 `Test*` 이름과 단언, `docs/prd/codex-task-workload.md` AC-L1 |
| `control-plane/cmd/control-plane/codex_task_test.go` | 시나리오 표제 2건 | 3 | 각 `Test*` 이름과 단언 |
| `data-plane/cmd/agent/auth_slot_test.go` | 시나리오 표제 8건 | 15 | 각 `Test*` 이름과 단언, `docs/prd/agent-auth-pool.md` AC-N4 |
| `data-plane/cmd/agent/claude_subscription_test.go` | 시나리오 표제 1건 | 1 | `Test*` 이름과 단언, `docs/prd/claude-code-workload.md` AC-E7 |

주석이 모두 사라진 11개 파일은 원장 행이 삭제되었다: `codex_task_test.go` · `auth_pool_test.go`(L 표) ·
`api/plugins_test.go` · `api/reasoning_effort_test.go` · `service/auth_pool_test.go` ·
`session/plugins_test.go` · `session/workload_catalog_test.go` · `test/auth_pool_orchestrator_test.go` ·
`test/e2e_codex_task_4_effort_test.go` · `data-plane/cmd/agent/auth_slot_test.go` ·
`data-plane/cmd/agent/claude_subscription_test.go`.

## 유지 — 280줄 (사유)

| 대상 | 줄 | 필요 사유 |
| --- | ---: | --- |
| `loadAuthPoolEnv` 의 잘못된 크기·개수가 부팅을 실패시키는 이유 (`control-plane/cmd/control-plane/codex_task.go`) | 4 | 조용히 0으로 줄어든 풀은 오타를 「미제안 타입」으로 보이게 한다 — 코드만으로는 왜 부팅을 죽이는지 드러나지 않는다 |
| codex-task 설정이 codex-manager와 분리된 이유 (`control-plane/cmd/control-plane/codex_task.go`) | 3 | 두 타입이 Secret·ConfigMap·게이트·풀을 공유하지 않아야 하는 이유가 이름에 없다 — 다른 타입과 겹쳐도 컴파일은 된다 |
| `validateCodexTaskConfig` 이 codex-manager의 이유로 키를 막는 이유 (`control-plane/cmd/control-plane/codex_task.go`) | 2 | 키 이름이 같다는 사실이 겹침의 원인이어서, 이유를 알지 못하면 검증이 과해 보인다 |
| 구독 모드의 모델 id 가网关·OpenRouter·구독을 가르는 이유 (`control-plane/cmd/control-plane/codex_task.go`) | 4 |网关의 모델 id(OpenRouter)는 Claude 구독에서 의미가 없다 — 외부 시스템의 뜻이 코드에 없어 읽는 사람이 같은 설정을 두 갈래로 고칠 수 있다 |
| claude-code 가 풀을 빌리지 않는 이유 (`control-plane/cmd/control-plane/codex_task.go`) | 2 | 구독은 플랫폼 OAuth 토큰으로 로그인한다 — 타입 목록만 보면 왜 예외인지 알 수 없다 |
| 로그인 슬롯·Lease·PVC 이름 규칙과 deployed 슬롯을 못 버는 이유 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 9 | Lease·PVC 이름이 곧 배포된 로그인 슬롯의 이름이라 이름을 바꾸면 ChatGPT 로그인과 세션 DB가 stranded 된다 — 이 함수는 그 이름을 부른다 |
| 동적 프로비저너가 claim 이름만으로 볼륨을 구분할 수 있다는 환경 사실 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 5 | 홈랩 EFS 클래스가 `/${.PVC.name}` 을 하나의 액세스 포인트로 매핑해 같은 이름의 프리뷰 슬롯이 프로덕션 로그인을 지울 수 있다 — 클라우드 밖에서 확인된 사실이다 |
| 슬롯 크기 상수·회수 유예·재사용 갱신의 근거 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 11 | 숫자 상수의 크기가 왜 그 값인지(로그인 파일 하나·세션 DB 포함·생성 1회보다 긴 유예)는 코드에 없다 |
| `StaleHolder`·`AuthPool`·`Acquire`·`Release` 계약 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 8 | 풀은 세션 하나에 슬롯 하나를 독점하라는 계약과 ErrAuthPoolExhausted·슬롯 없음은 오류가 아니라는 규칙이 인터페이스 이름에 없다 |
| Lease 갱신이 CAS로 되는 이유와 재시도 상한 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 6 | 두 control plane 경로가 같은 슬롯을 동시에 얻지 못하려면 읽은 resourceVersion 을 실어야 한다 — 동시성 계약이 코드 형태에 드러나지 않는다 |
| 판단·점유할 수 없는 슬롯을 건너뛰는 이유 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 3 | Lease 하나가 나빠도 그 세션만 다른 슬롯을 못 쓰게 하면 안 된다 — 오류를 언제 표면화하는지가 코드로 드러나지 않는다 |
| `foreignLease` 가 라벨 없는 Lease를 보고하는 이유 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 4 | 라벨이 없는 Lease는 목록에 안 보여 모든 생성이 그에게 지고 슬롯이 조용히 안 쓰인다 — 조용히 넘어가면 원인을 찾을 수 없다 |
| 슬롯 PVC를 지우지 않는 이유와 `free` 의 회수 조건 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 6 | 로그인은 그 슬롯을 쓴 모든 세션보다 오래 산다 — 슬롯 지우기가 정상이면 이 계약이 없다 |
| 사이드카가 어느 레인인지 따로 알려 주지 않아도 되는 이유 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 4 | 도착한 주소가 레인을 말해 준다 — 반대로 생기면 경로 우선순위를 따로 관리해야 한다 |
| 관리자 파드의 모니터 레인 위치·쓰기 주소·opt-in (`control-plane/internal/adapter/k8s/auth_pool.go`) | 25 | 모니터 레인의 주소가 무조건인 이유(파드 모양에 대한 사실), 레인 쌍의 opt-in이 세션이 아니라 배포 ConfigMap 에서 오는 이유, 아카이브 트리 밖에 두는 이유가 전부 코드에 없다 |
| `slotInitContainer` 의 두 디렉터리 준비와 chown 실패를 무시하는 이유 (`control-plane/internal/adapter/k8s/auth_pool.go`) | 6 | 서브Path 는 kubelet 이 root 0750 으로 만들고 비 root 사이드카는 못 쓴다 — 클러스터 관측 사실이라 코드에 없다 |
| `MemoryAuthPool` 의 시계 없음 (`control-plane/internal/adapter/k8s/auth_pool_memory.go`) | 3 | 실제 풀과 달리 오래된 홀더를 즉시 회수할 수 있어 테스트가 시간 경과를 흉내 내지 않는다 — 차이는 이름에 없다 |
| 세션 슬롯의 존재 이유와 copy-not-symlink (`data-plane/cmd/agent/auth_slot.go`) | 6 | CLI 가 파일을 임시 파일로 바꿔 rename 하면 심볼릭 링크가 끊기고 갱신된 토큰이 HOME 에만 남는다 — 외부 CLI 동작이 코드에 없다 |
| HOME 을 여러 개 serve 하는 관리자와 `placed`·`unpushed`·`baseline`·`hasBaseline` (`data-plane/cmd/agent/auth_slot.go`) | 21 | 두 레인이 같은 로그인을 번갈아 쓰므로 `placed` 한 개로는 한 레인의 pull 이 다른 레인의 로그아웃을 결정해 버린다 — 각 상태의 의미가 코드에 없다 |
| pull·push·pushIfPresent 의 계약 (`data-plane/cmd/agent/auth_slot.go`) | 12 | 슬롯이 시작점이고 로그아웃과 재작성을 구별해야 한다 — 없으면 로그아웃으로 읽은 파일이 슬롯의 로그인을 지운다 |
| `errLoginNotJSON`·`errLoginOverwritten`·compare-and-swap·`rebaseLocked` (`data-plane/cmd/agent/auth_slot.go`) | 14 | 다른 레인이 새 로그인을 갱신한 뒤에는 느린 레인이 그 것을 덮어쓰면 안 되고, 재작성 중인 파일과 로그인 파일을 구별해야 한다 — 실패를 재시도할지 말지가 여기서 갈린다 |
| `watch` 와 `writeLoginFile` (`data-plane/cmd/agent/auth_slot.go`) | 7 | 턴 중에 파드가 죽어도 슬롯에 제공자가 기대하는 토큰이 남아야 하고 rename 은 지속되어야 한다 — 파일 교체 방식이 계약이다 |
| `serveAuthSlot` 이 리스를 돌려주기 전에 답하는 이유 (`data-plane/cmd/agent/auth_slot.go`) | 2 | 풀이 로그인된 슬롯을 먼저 내주려면 control plane 의 질문이 먼저 도착해야 한다 — 순서가 계약이다 |
| 세션 플러그인 선택의 불변식과 marketplace 규칙 (`data-plane/cmd/agent/claude_plugins_test.go`) | 11 | 복원 시 선택하지 않은 플러그인이 되살아나지 않아야 하고 여러 개를 골라도 유효하며, 다른 marketplace 의 플러그인은 플랫폼 것이 아니다 — 외부 marketplace 규칙이 코드에 없다 |
| minimax-code 의 K3s 서버 선언 충돌 처리 (`data-plane/cmd/agent/claude_plugins_test.go`) | 4 | 플러그인마다 같은 서버를 선언하면 첫 등록이 이기고 다른 것은 경고로 밀린다 — 세션이 에이전트를 잃지 않는 조건이다 |
| staged marketplace 이름과 토큰 없는 컨테이너 (`data-plane/cmd/agent/claude_plugins_test.go`) | 6 | 플러그인 id 가 `<name>@dlddu-plugins` 로 남는 조건과 사이드카 복제본이 토큰을 못 받는다는 사실이 코드에 없다 |
| claude-code 의 두 인증 모드와 OAuth 토큰 출처 (`data-plane/cmd/agent/claude_subscription.go`) | 5 | `claude setup-token` 이 printed 하는 긴 토큰을 control plane 이 Secret 에서 `CLAUDE_CODE_OAUTH_TOKEN` 으로 투영한다 — 외부 CLI 계약이 코드에 없다 |
| 인증 모드·추론 수준 env 와 control plane 의 대응 (`data-plane/cmd/agent/claude_subscription.go`) | 2 | 두 곳의 상수가 어긋나면 에이전트가 로그인하지 못한다 — 대조 대상이 코드에 없다 |
| `claudeEfforts` 가 CLI 의 `--effort` 목록인 이유와 모드 기본값 (`data-plane/cmd/agent/claude_subscription.go`) | 5 | 모드가 없던 때와 같은 기본값(gateway)이 유지되어야 하고 목록의 정본은 CLI 쪽이다 —默认值이 코드에 없다 |
| 구독 모드의 배치 규칙 (`data-plane/cmd/agent/claude_subscription.go`) | 4 | 환경에 제공자 주소나 다른 자격 증명이 있으면 플랫폼이 고르지 않은 계정으로 바뀐다 — refusal 규칙이 코드에 없다 |
| e2e harness 의 이름·레이블을 여기 적는 이유와 cleanup 이 슬롯을 되돌리는 사실 (`control-plane/test/harness_auth_pool_test.go`) | 8 | 이 스위트는 도메인 타입이 아니라 와이어와 클러스터만 알고 있어 상수를 여기 적어야 한다 — 슬롯이 다음 테스트로 돌아가는 경로도 코드에 없다 |
| `fillCodexTaskPool` 이 빈 슬롯 수를 가정하지 않는 이유 (`control-plane/test/harness_auth_pool_test.go`) | 4 | 다른 스위트의 세션이 이미 슬롯을 쥘 수 있어 자유 슬롯 수를 가정할 수 없다 — 테스트가 격리되지 않는 사실이 코드에 없다 |
| 만료된 생성이 남긴 것이 없다는 전제 (`control-plane/test/e2e_auth_pool_2_exhausted_test.go`) | 2 | 존재하는 세션이 모두 슬롯을 쥐어야 한다는 전제 없이는 503 뒤의 검사가 아무것도 확인하지 않는다 |
| 플러그인 선택이 타입별로 전달되는 경로 (`control-plane/test/plugins_orchestrator_test.go`) | 8 | K3s MCP 사이드카가 같은 선택을 심고, 기본 플러그인이 없는 타입은 선택이 없으면 그대로 비어 있어야 한다 — 전달 경로가 테스트 이름에 없다 |

## 판단이 갈린 것

- `auth_pool.go` 의 `AuthSlotClaimName` doc 첫 줄은 제거하고 그 뒤의 환경 사실만 남겼다. claim 이름이 슬롯
  i 의 이름이라는 사실은 함수 이름이 말하고, 홈랩 EFS 클래스가 claim 이름만으로 볼륨을 구분한다는 사실은
  코드에 없다 — 앞부분만 걷었다.
- `codex_task.go` 의 `authPoolEnv`·`slots` doc은 걷었다. 슬롯이 0 이면 풀이 없다는 사실은 `pool` 과
  `offered` 가 이미 말하고, 필요한 뜻은 「그 타입이 미제안」이라는 한 줄뿐이었다 — 남길 이유가 서지
  않았다.
- 테스트 시나리오 표제 가운데 계약이 서 있는 것은 남겼다 — 「플러그인이 없는 워크로드」나 「사이드카에
  토큰이 없는 세션」처럼 단언만으로는 복원되지 않는 계약이다. 나머지 표제는 테스트 이름과 단언이 이미
  말하는 내용이라 걷었다.
- E 표면 4줄은 모두 남겼다 — 세 줄은 슬롯·리스의 nil·경합 뜻이고, 한 줄은 회수 시도가 로그인 상태를
  모르는 상태로 두는 사실이라 코드 형태만으로는 드러나지 않는다.
