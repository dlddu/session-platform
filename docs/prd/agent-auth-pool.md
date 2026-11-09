# PRD: 에이전트 인증 풀 (agent auth pool)

> 대상 요구사항: 구독 계정으로 도는 작업용 에이전트 타입(`codex-task`·`claude-code`, 2026-11-09부터 채팅용 `codex-chat`도)이 세션마다 로그인하지 않도록,
> 플랫폼이 **로그인 슬롯 풀**을 두고 세션에 슬롯을 하나씩 **독점 임대**한다 (2026-11-07 사용자 결정).
> 슬롯은 StatefulSet의 `volumeClaimTemplates`처럼 이름이 고정된 PVC이고, 세션 파드가 그 PVC를 직접 마운트한다.
>
> 📌 **개정 (2026-11-09, 사용자 결정)**: 채팅용 `codex-chat`도 풀을 쓴다(AC-N6). 이 풀의 슬롯은 로그인과 함께
> **세션 DB**(SQLite)를 담고, 사이드카가 그 DB를 MCP로 내놓는다. 채팅은 장기 단일 세션 기준이지만, 같은 타입의
> 세션이 늘어날 때를 위해 처음부터 슬롯 단위로 둔다.

## 달성 가치
- **V3 끊김 없는 세션 연속성** — 작업 세션이 동결·복원되거나 새로 만들어져도 다시 로그인하지 않는다. 로그인은 세션이 아니라 슬롯에 남는다 (AC-N1·L4)
- **V1 세션 격리** — 한 슬롯을 동시에 두 파드가 쓰지 않는다. 구독 계정의 refresh token은 쓸 때마다 바뀌어, 두 파드가 같은 계정을 갱신하면 한쪽 로그인이 깨진다 (AC-N2)
- **V2 유휴 자원 회수** — 로그인 전용 작업 세션은 동결 시 슬롯을 반납한다. DB가 있는 codex-chat은 삭제까지 유지한다 (AC-N3·N6)
- **V3 끊김 없는 세션 연속성(장기 세션)** — 채팅 세션이 쌓은 데이터가 동결·복원과 파드 재시작을 건너 슬롯에 남는다 (AC-N6)

> 📌 **가치 연결 규칙**: 다른 PRD와 같이 각 AC는 자신이 구체화하는 상위 AC의 가치를 상속한다.

> 이 문서는 **풀 자체**(슬롯·임대·반납·로그인 동기화)만 정한다. 어떤 타입이 풀을 쓰는지와 슬롯 안 파일 이름은
> 타입 PRD가 정한다 — `codex-task`는 `codex-task-workload.md`의 AC-M2, `claude-code`는 `claude-code-workload.md`의 AC-E7,
> `codex-chat`은 `codex-workload.md`의 AC-J3과 이 문서의 AC-N6(슬롯 안 배치와 세션 DB).

## Acceptance Criteria

### AC-N1: 슬롯 = 이름이 고정된 PVC, control plane이 만들고 지우지 않는다
- **설명**: 풀은 타입마다 하나다. 풀 `<pool>`(`codex-task`·`claude-code`·`codex-chat`)의 슬롯 `i`(0 ≤ i < N)는 PVC `<네임스페이스>-<pool>-auth-<i>`다. PVC 이름에 네임스페이스를 넣는 것은 동적 프로비저너가 PVC 이름만으로 볼륨을 정할 수 있어서다 — 운영 클러스터의 `efs` 클래스는 `/${.PVC.name}` 경로와 재사용 access point를 써, 이름이 같으면 프리뷰 네임스페이스의 슬롯이 운영 슬롯과 같은 디렉터리를 쓰고 프리뷰를 지울 때 운영 로그인까지 지운다(2026-11-07 프리뷰 점검에서 발견). N은 control plane 설정(`CODEX_TASK_AUTH_SLOTS`·`CLAUDE_CODE_AUTH_SLOTS`·`CODEX_CHAT_AUTH_SLOTS`)이고, 0이면 그 타입은 풀이 없어 `GET /config`에 나오지 않으며 생성은 pod 없이 실패한다. PVC는 처음 임대될 때 control plane이 만든다(`ReadWriteOnce`, 크기 `<POOL>_AUTH_STORAGE_SIZE` 기본 `16Mi` — `codex-chat`은 세션 DB를 담아 `1Gi`, 클래스 `<POOL>_AUTH_STORAGE_CLASS` 비면 클러스터 기본 — 운영은 기본 클래스가 없어 `efs`를 지정한다). 라벨은 `session-platform.dev/auth-pool=<pool>`, `session-platform.dev/auth-slot=<i>`다. control plane은 슬롯 PVC를 **지우지 않는다** — 세션이 지워져도, N을 줄여도 남는다(StatefulSet의 PVC 보존과 같다). N을 줄이면 범위 밖 슬롯은 더 임대하지 않을 뿐이다. 노드 국소 스토리지(local-path, kind e2e)에서는 PV의 노드 친화성 때문에 그 슬롯을 받은 세션 파드가 그 노드에 뜬다. 운영의 `efs`는 RWX라 노드에 묶이지 않는다.
- **달성 가치**: V3
- **구체화 대상**: AC-B3(무결성) — 동결·복원·재생성을 건너 보존되는 상태에 「슬롯의 로그인」을 더한 것
- **검증 방법**: 풀 크기 2의 배포에서 첫 세션 생성 뒤 PVC `<네임스페이스>-<pool>-auth-0`이 라벨과 함께 생기고 세션 파드가 그것을 마운트함을 확인한다. 세션을 지운 뒤에도 PVC가 남음을 확인한다. 풀 크기 0이면 `GET /config`에 타입이 없고 생성이 pod 없이 실패함을 확인한다.

### AC-N2: 독점 임대 = 슬롯마다 Lease 하나, holder는 세션 id
- **설명**: 슬롯 `i`의 임대는 `coordination.k8s.io` Lease `<pool>-auth-<i>`다(PVC와 같은 라벨, 이름은 네임스페이스 접두 없이 — Lease는 네임스페이스 안에서만 보인다). `holderIdentity`가 세션 id이면 그 세션이 슬롯을 쥔 것이다. 임대·반납은 resourceVersion으로 CAS하므로 두 control plane 경로가 같은 슬롯을 동시에 잡지 못한다. 로그인 전용 작업 세션은 슬롯을 **파드가 있는 동안** 쥔다(codex-chat은 AC-N6에 따라 삭제까지 유지) — 생성(AC-A1)과 복원(AC-B2)이 파드를 만들기 **전에** 임대하고, 파드 생성이 실패하면 반납한다. 고를 슬롯은 비어 있는 슬롯 가운데 로그인 상태(AC-N4)가 `signed-in` → 모름 → `signed-out` 순, 같은 순위에서는 번호가 작은 것이다. 비어 있는 슬롯이 없으면 생성·복원은 파드 없이 **503**(`auth pool exhausted`)으로 실패한다 — 복원 실패는 세션을 `snapshot`에 남긴다.

  holder가 남았지만 실제로는 쥐고 있지 않은 임대(control plane이 반납 전에 죽은 경우)는 다음 임대가 회수한다. 회수 조건은 **둘 다**다: ① holder 세션이 레코드에 없거나 DB 없는 타입의 `snapshot` 상태이고, ② 임대의 `renewTime`이 10분(생성·복원 한 번보다 긴 유예)보다 오래됐다. ②가 없으면 레코드를 쓰기 전인 생성이나 아직 `snapshot`인 복원의 임대를 다른 세션이 빼앗는다.
- **달성 가치**: V1
- **구체화 대상**: AC-A1/A2의 세션 격리 — 세션 전용 자원에 「세션 전용 로그인 슬롯」을 더한 것
- **검증 방법**: 풀 크기 1에서 첫 세션이 슬롯을 쥔 동안 두 번째 생성이 pod 없이 503임을 확인한다. holder가 지워진 세션이고 `renewTime`이 오래된 임대는 다음 생성이 회수하고, 유예 안의 임대는 회수하지 않음을 단위 테스트로 확인한다. 로그인된 슬롯이 먼저 배정됨을 확인한다.

### AC-N3: 반납 = 로그인 전용 타입 동결 완료·모든 타입 삭제 시
- **설명**: codex-chat은 AC-N6의 DB 보존을 위해 삭제까지 슬롯을 유지한다. 나머지 타입은 동결이 커밋되어 파드가 지워진 뒤(AC-B1) 슬롯을 반납하고, 삭제(`DELETE /sessions/{id}`)도 파드를 지운 뒤 반납한다. 반납은 holder가 자기 세션일 때만 지운다(CAS) — 이미 회수된 슬롯을 남의 것까지 풀지 않는다. 반납 실패는 동결·삭제를 실패시키지 않는다; 남은 임대는 AC-N2의 회수 조건이 처리한다. 복원은 이전 슬롯을 고집하지 않는다 — 대화 기록은 세션 아카이브에 있고 로그인은 슬롯에 있으므로, 다른 계정의 슬롯으로 복원해도 대화가 이어진다.
- **달성 가치**: V2
- **구체화 대상**: AC-B1(동결)·AC-A3(자원 회수)
- **검증 방법**: 풀 크기 1에서 세션 A를 동결하면 슬롯이 풀리고 세션 B 생성이 성공함을 확인한다. 이어서 A에 접근하면 B가 슬롯을 쥔 동안 503이고 A는 `snapshot`에 남음을 확인한다. 삭제한 세션의 슬롯이 풀림을 확인한다.

### AC-N4: 로그인은 슬롯 파일, 에이전트가 HOME과 동기화하고 아카이브에서 뺀다
- **설명**: 슬롯 PVC는 주 컨테이너의 `/var/run/session-platform/auth-slot`에 마운트되고(`AGENT_AUTH_SLOT_DIR`), 그 안에 타입이 정한 로그인 파일 하나가 있다. CLI는 그 파일을 HOME 아래 자기 자리(타입 PRD가 정함)에서 읽고 쓰므로, 에이전트가 두 자리를 **복사로** 맞춘다 — symlink를 두지 않는다(CLI가 임시 파일 + rename으로 바꿔 쓰면 symlink가 끊겨 갱신된 토큰을 잃는다).
  1. 턴 시작 전: 슬롯 → HOME. 슬롯에 파일이 없으면 HOME의 것도 지운다.
  2. 턴 중: 1초마다 HOME의 파일이 바뀌었으면 HOME → 슬롯. 턴 중에 파드가 죽어도 갱신된 refresh token이 슬롯에 남게 한다.
  3. 턴 끝: HOME → 슬롯. HOME에서 파일이 사라졌으면(로그아웃) 슬롯의 것도 지운다.

  쓰기는 임시 파일 + fsync + rename, 권한 0600이다. HOME의 로그인 파일은 **세션 아카이브에 넣지 않는다** — 로그인은 슬롯의 것이고, 아카이브에 넣으면 복원이 낡은 토큰으로 슬롯의 새 토큰을 덮을 수 있다. 에이전트는 `GET /auth-slot`으로 `{"signedIn": <슬롯 파일이 있고 비지 않음>}`을 알리고, control plane은 반납 직전에 그것을 읽어 Lease의 `session-platform.dev/auth-state`(`signed-in`·`signed-out`)에 적는다(AC-N2의 순위). 읽지 못하면 적지 않는다.

  빈 슬롯을 받은 세션은 첫 프롬프트에서 그 타입의 로그인 흐름을 돈다(타입 PRD). 로그인 결과는 3번으로 슬롯에 남아 다음 세션은 로그인하지 않는다.
- **달성 가치**: V3, V1
- **구체화 대상**: AC-E5·AC-J4의 파일시스템 아카이브 — 아카이브 대상에서 로그인 파일을 빼고 그 자리를 슬롯이 맡은 것
- **검증 방법**: 에이전트 단위 테스트로 턴 전·중·후 동기화(슬롯 없음 → HOME 삭제, HOME 갱신 → 슬롯 갱신, HOME 삭제 → 슬롯 삭제)와 아카이브에 로그인 파일이 없음을 확인한다. `GET /auth-slot`이 슬롯 상태를 돌려주고 반납이 Lease 주석에 적음을 확인한다.

### AC-N6: `codex-chat` 슬롯 = 로그인 + 세션 DB, 컨테이너마다 제 몫만 마운트
- **설명**: `codex-chat` 풀(`CODEX_CHAT_AUTH_SLOTS`·`_STORAGE_CLASS`·`_STORAGE_SIZE` 기본 `1Gi`)의 슬롯은 두 디렉터리로 나뉜다 — `auth/`(로그인 파일 `auth.json`)와 `db/<sessionID>/`(세션 DB `session.db`). 같은 PVC를 subPath로 나눠 마운트한다:
  1. 주 컨테이너는 `auth/`만 `/var/run/session-platform/auth-slot`에 받는다. 로그인 동기화·아카이브 제외는 AC-N4 그대로다.
  2. `k3s-mcp-proxy` 사이드카는 `db/<sessionID>/`만 `/var/run/session-platform/session-db`에 받고(`SESSION_DB_DIR`), 같은 포트의 `/session-db/mcp`에서 **세션 DB MCP**를 서빙한다. 주 컨테이너는 그 주소만 받는다(`SESSION_DB_MCP_URL`). 사이드카는 로그인 파일에, 주 컨테이너는 DB 파일에 닿지 않는다 — 그래서 AC-N4의 「사이드카에 슬롯을 마운트하지 않는다」는 「사이드카에 로그인을 마운트하지 않는다」로 읽는다.
  3. 초기화 컨테이너 `slot-init`(root, `CHOWN`·`FOWNER`만)이 슬롯 전체를 마운트해 두 디렉터리를 만들고 `db/<sessionID>/`를 사이드카 사용자(65532)에게 넘긴다 — kubelet이 만드는 subPath는 root 소유 0750이라 non-root 사이드카가 쓰지 못한다. `efs` access point처럼 소유자를 하나로 매핑하는 볼륨에서는 chown이 실패하지만, 그 볼륨은 이미 사이드카가 쓸 수 있으므로 실패를 무시한다.

  세션 DB MCP의 raw 도구는 둘이다: `raw_query`(행을 돌려주는 문장, 최대 1000행·약 1MiB, 넘으면 `truncated`)와 `raw_exec`(바꾸는 문장, `rowsAffected`·`lastInsertId`; 매개변수가 없으면 `;`로 이은 여러 문장). 둘 다 `{sql, params?}`를 받고 `params`는 `?` 자리에 순서대로 묶인다. SQLite가 거부한 문장은 `isError` 도구 결과로 메시지를 돌려준다. 한 문장은 30초로 끊는다. DB는 `journal_mode=DELETE`로 연다 — 운영 `efs`는 NFS라 WAL의 공유 메모리가 동작하지 않고, 파일을 여는 프로세스는 사이드카 하나다. 사이드카가 뜰 때 디렉터리가 준비되지 않았으면 그 호출만 실패하고 다음 호출이 다시 연다(사이드카는 K3s MCP도 맡으므로 죽지 않는다). DB는 아카이브에 들어가지 않는다 — 슬롯에 있으므로 동결·복원·재생성을 건너 남는다.
  DB가 있는 codex-chat은 스냅샷/복구/복원 실패에도 슬롯을 유지하며 frozen holder 재회수 대상이 아니다. 삭제 시 반납하고, 동결 중에도 풀 용량을 차지한다. 다른 타입의 스냅샷 반납 규칙은 유지한다. 상위 작업 관리 도구와 raw 트랜잭션 경계·슬롯 재사용 격리·기존 DB 이관 제약은 [세션 원장 계약](../session-ledger.md)을 따른다. 슬롯 공용 `db/session.db`는 자동 이관하지 않고 보존한다.
- **달성 가치**: V3(장기 세션), V1
- **구체화 대상**: AC-N1(슬롯)·AC-N4(로그인 배치) — 슬롯에 세션 DB를 더하고 컨테이너별로 나눠 마운트한 것, `codex-workload.md` AC-J5(도구 표면)·AC-J6(사이드카)
- **검증 방법**: 제출된 `codex-chat` pod spec에서 PVC가 자기 슬롯 하나이고, 주 컨테이너는 subPath `auth`, 사이드카는 subPath `db/<sessionID>`, `slot-init`만 슬롯 전체를 마운트함을 확인한다(`control-plane/test/codex_orchestrator_test.go`). 세션 DB MCP 단위 테스트로 `raw_exec` → `raw_query` 왕복, SQLite 오류가 `isError`, 행 상한과 `truncated`, 늦게 생긴 디렉터리를 다음 호출이 여는 것, `/session-db/mcp` 밖의 요청은 K3s MCP 프록시로 가는 것을 확인한다(`data-plane/cmd/agent/session_db_mcp_test.go`).

### AC-N5: 권한 — control plane은 PVC·Lease만, 세션은 자기 슬롯만
- **설명**: control plane Role은 이미 가진 `persistentvolumeclaims` create/get/list/watch와 `leases` 전체 동사로 풀을 다룬다 — 새 권한이 없다. 세션 파드는 자기 슬롯 PVC 하나만 마운트하고, 다른 슬롯에 닿는 Kubernetes 자격 증명을 받지 않는다(타입별 SA 권한은 바뀌지 않는다). 슬롯 파일은 셸이 있는 주 컨테이너가 읽을 수 있다 — 프록시로 숨기지 않기로 했다(2026-11-07 사용자 결정): 세션 안 에이전트가 그 계정의 토큰을 읽을 수 있다는 것이 이 풀의 알려진 대가다.
- **달성 가치**: V1
- **구체화 대상**: AC-A1/A2의 세션 격리
- **검증 방법**: 제출된 pod spec에 PVC 볼륨이 자기 슬롯 하나뿐임을 확인한다. `k8s/rbac.yaml`이 바뀌지 않음을 확인한다.

## 결정 사항 (2026-11-07, 사용자)
- **독점 임대**: 공유(슬롯 파드 하나가 혼자 갱신하고 세션은 프록시로 부름)는 로그인 파일을 세션 컨테이너에 두는 방식과 함께 쓸 수 없어 고르지 않았다.
- **로그인 파일은 세션 컨테이너에 마운트**: 프록시로 숨기지 않는다(AC-N5의 대가).
- **슬롯 저장소는 PVC**: Secret에 되쓰는 사이드카와 RBAC가 필요 없다. 운영은 `efs`(RWX)라 노드에 묶이지 않는다.
- **빈 슬롯은 첫 세션이 로그인**: 운영자 전용 로그인 API를 두지 않는다.

## 결정 사항 (2026-11-09, 사용자)
- **`codex-chat`도 풀을 쓴다**: 세션 DB를 둘 PVC가 필요했고, 따로 PVC 종류를 만들지 않고 로그인과 함께 슬롯에 담는다. 채팅은 장기 단일 세션 기준이지만 확장을 생각해 슬롯 단위로 둔다(AC-N6).
- **세션 DB는 슬롯에 남는다**: 슬롯을 다음 세션이 받으면 DB도 이어받는다 — 새 세션이라고 비우지 않는다.
- **기존 세션은 다시 로그인**: 슬롯 도입 전 `codex-chat` 세션의 로그인은 아카이브된 HOME에 있지만 슬롯으로 옮기지 않는다. 빈 슬롯은 AC-N4 1번대로 HOME의 로그인을 지우고, 다음 프롬프트가 로그인 흐름을 돈다.
- **슬롯 0이면 타입을 내놓지 않는다**: `codex-task`와 같은 규칙. 운영은 1이다.
- **DB 도구는 raw 두 개**: `raw_query`·`raw_exec`만 둔다. 도메인 도구는 쓰임이 정해지면 그때 더한다.

## 남은 일
- 슬롯 이용률·로그인 상태를 보여 주는 운영 화면은 없다. Lease 주석과 `kubectl get lease -l session-platform.dev/auth-pool`로 본다.
- 실 계정 로그인과 그 뒤의 토큰 갱신은 e2e로 검증하지 않는다 — 사람의 브라우저 승인이 필요하다.
