# PRD: 세션 라이프사이클 & 워크로드별 스냅샷

> 대상 요구사항: ~~② CRIU 기술 기반~~ (2026-11-01 퇴역), ③ 세션 maximum idle 60분 이후 스냅샷
>
> 📌 **요구사항 ② 퇴역 (2026-11-01)**: CRIU는 모든 워크로드에서 퇴역했고 동결·복원은 **파일시스템 아카이브** 하나로 통일됐다(`../criu-verification.md` 상단 배너). 요구사항 ②는 기록으로만 남기며, 이 PRD의 AC는 그것에 기대지 않는다.
>
> 📌 **범위 (2026-08-08 개정, 2026-09-03 갱신, 2026-11-01 개정)**: 아래 AC-B1~B3의 **동결·복원 메커니즘은 모든 워크로드 타입이 같다** — 에이전트가 세션의 상태 디렉터리와 누적 출력(scrollback)을 파일시스템 아카이브로 내보내고, control plane이 generation 펜스 트랜잭션(prepare → committing → 회수, 실패 시 abort)으로 외부 스토리지에 중계한다. 타입별로 다른 것은 **아카이브에 무엇이 담기는가**(보존 대상)와 **어느 게이트 뒤에 있는가**뿐이다: `shell`은 홈 디렉터리 파일 + scrollback(AC-D4, `SHELL_ARCHIVE_ENABLED`), `claude-code`·`minimax-code`는 작업 디렉터리·HOME·대화 기록 + scrollback(AC-E5·AC-G4, 각자의 게이트), `approval-gated`는 거기에 세션 공유 볼륨 내용이 더해진다(AC-F5, `APPROVAL_GATED_ARCHIVE_ENABLED`). 게이트가 꺼진 타입은 동결할 수 없다 — 리퍼는 세션을 live로 두고 명시적 snapshot은 service unavailable로 거부된다. 유휴 한계·상태 전이·pod 회수 등 **타이밍과 관측 가능한 계약은 모든 타입이 동일**하다. *(이전 판은 `shell`을 CRIU 체크포인트로 두고 「아래 CRIU 서술은 `shell` 타입 기준」이라 적었다.)* `approval-gated`는 회수·복원 대상에 헬퍼 보조 파드(MCP·credential-proxy 컨테이너, AC-F4)와 공유 볼륨(AC-F5)이 더해지고, 승인 대기 구간에서 유휴 기준이 구체화된다(AC-F3).

## 달성 가치
- **V2 유휴 자원 회수** — 유휴 세션을 동결하여 pod 자원 회수
- **V3 끊김 없는 세션 연속성** — 워크로드별 체크포인트/복원으로 상태 보존

## Acceptance Criteria

### AC-B1: 60분 유휴 후 스냅샷
- **설명**: 세션의 유휴 시간(마지막 read/write 이후 경과 시간)이 60분에 도달하면, 시스템은 세션 pod의 체크포인트(스냅샷)를 타입별 메커니즘으로 생성하고 세션 상태를 `snapshot`으로 전이한 뒤 pod를 회수한다. 60분은 **최대 유휴 한계**이다.
- **달성 가치**: V2, V3
- **구체화**: "마지막 read/write" = 마지막 클라이언트 workload I/O로 확정한다. `shell`은 쉘 read/write(AC-D5), `claude-code`는 state-branched output read와 prompt write(AC-E2/E3)다. `approval-gated`도 같은 기준을 쓰되, **승인 대기가 진행되는 동안에는 클라이언트 I/O가 없어도 `lastAccess`를 갱신해 유휴 카운트를 멈춘다**(AC-F3 — 이 타입에서만 성립하는 의도적 예외). passive SSE 연결·output/reset event·comment keepalive는 상태를 바꾸거나 `lastAccess`를 touch하지 않으며 활동이 아니다. 단, reset 뒤 전체 replay를 위한 `POST /read`는 일반 workload read이므로 활동으로 세고 idle을 승격할 수 있다. 동결 메커니즘: 모든 타입이 파일시스템 아카이브다 — `shell`=홈 디렉터리 파일 + scrollback(AC-D4) / `claude-code`=작업 디렉터리·HOME·대화 기록 + scrollback(AC-E5, `claude-code-workload.md`) / `approval-gated`=그것에 공유 볼륨 내용(AC-F5)을 더하고 헬퍼 보조 파드를 동반 회수(AC-F4, `approval-gated-workload.md`). `shell`은 체크포인트가 진행되는 동안 쉘 프로세스 트리를 일시정지(SIGSTOP)하고 write를 거부하며, 스냅샷이 중단되면 재개(SIGCONT)한다
- **검증 방법**: 세션을 60분간 미사용으로 두면 스냅샷이 생성되고 상태가 `snapshot`으로 바뀌며 pod가 회수됨을 확인한다. 경계값(예: 59분)에서는 동결되지 않으며 output 없는 SSE keepalive만으로 동결이 영구 차단되지 않음을 확인한다. snapshot으로 stream이 끊긴 뒤 SPA가 자동 read/stream으로 즉시 복원하지 않고 Restore 화면으로 이동하는지도 확인한다.

### AC-B2: 스냅샷 복원
- **설명**: `snapshot` 상태의 세션에 접근(read/write/전환)하면, 시스템은 새 pod에 체크포인트를 복원하고 세션을 `active`로 전이한다. 복원은 모든 타입이 아카이브 전개다(복원 대상 pod에는 `session-platform.dev/restore-archive` 주석이 붙는다) — `shell`은 홈 디렉터리를 전개하고 scrollback을 preload한 뒤 그 홈에서 **새 쉘**을 기동하고(AC-D4), `claude-code`는 작업 디렉터리·HOME을 전개하며(AC-E5), `approval-gated`는 거기에 더해 새 헬퍼 보조 파드와 그 라운드의 새 공유 볼륨을 세우고 아카이브의 공유 볼륨 내용을 그 볼륨에 전개한다(AC-F4/F5).
- **달성 가치**: V3
- **검증 방법**: `snapshot` 세션 접근 시 복원이 수행되어 `active`로 전이되고 응답 가능 상태가 됨을 확인한다.

### AC-B3: 스냅샷·복원 무결성
- **설명**: 스냅샷 생성 직전의 세션 상태가 복원 후에도 보존된다. 체크포인트/복원 과정에서 세션 데이터 손실이 발생하지 않는다.
- **달성 가치**: V3
- **구체화**: 보존 대상 상태의 정체는 타입별로 다르지만 **모두 온디스크 상태 + 누적 출력**이다 — `shell`은 홈 디렉터리 파일과 누적 출력 버퍼 → AC-D4 (`shell-workload.md`, *2026-11-01 개정*: 환경 변수·cwd·쉘 변수·실행 중인 프로세스·열린 FD는 **보존되지 않는다**, 복원 쉘은 새 쉘), `claude-code`는 대화 기록·작업 디렉터리·누적 출력 버퍼 → AC-E5 (`claude-code-workload.md`), `approval-gated`는 거기에 **세션 전용 공유 볼륨의 내용**이 더해진다 → AC-F5 (`approval-gated-workload.md`). 모든 경우 "복원 후 보존 대상 위에서 이어짐 + 커서 유효"라는 관측 계약은 같다.
- **검증 방법**: `shell`은 동결 전 홈 디렉터리 파일·scrollback cursor를(그리고 환경 변수·cwd가 **이어지지 않음**을), `claude-code`는 대화 기록·작업 디렉터리·bounded output과 cursor를, `approval-gated`는 거기에 더해 공유 볼륨의 파일을 마커로 세팅 → 타입별 스냅샷 → 복원 후 동일 상태가 유지되는지 확인한다.
