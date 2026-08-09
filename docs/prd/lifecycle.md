# PRD: 세션 라이프사이클 & 워크로드별 스냅샷

> 대상 요구사항: ② CRIU 기술 기반, ③ 세션 maximum idle 60분 이후 스냅샷
>
> 📌 **범위 (2026-08-08 개정)**: 아래 AC-B1~B3의 **동결·복원 메커니즘은 워크로드 타입(AC-E1)에 따라 분기**한다. `shell`은 CRIU 체크포인트(AC-D4), `claude-code`는 파일시스템 아카이브(AC-E5, CRIU 비대상). 유휴 한계·상태 전이·pod 회수 등 **타이밍과 관측 가능한 계약은 두 타입이 동일**하며, 아래 CRIU 서술은 `shell` 타입 기준이다.

## 달성 가치
- **V2 유휴 자원 회수** — 유휴 세션을 동결하여 pod 자원 회수
- **V3 끊김 없는 세션 연속성** — 워크로드별 체크포인트/복원으로 상태 보존

## Acceptance Criteria

### AC-B1: 60분 유휴 후 스냅샷
- **설명**: 세션의 유휴 시간(마지막 read/write 이후 경과 시간)이 60분에 도달하면, 시스템은 세션 pod의 체크포인트(스냅샷)를 타입별 메커니즘으로 생성하고 세션 상태를 `snapshot`으로 전이한 뒤 pod를 회수한다. 60분은 **최대 유휴 한계**이다.
- **달성 가치**: V2, V3
- **구체화**: "마지막 read/write" = 마지막 클라이언트 workload I/O로 확정한다. `shell`은 쉘 read/write(AC-D5), `claude-code`는 state-branched output read와 prompt write(AC-E2/E3)다. passive SSE 연결·output/reset event·comment keepalive는 상태를 바꾸거나 `lastAccess`를 touch하지 않으며 활동이 아니다. 단, reset 뒤 전체 replay를 위한 `POST /read`는 일반 workload read이므로 활동으로 세고 idle을 승격할 수 있다. 동결 메커니즘: `shell`=CRIU(AC-D4) / `claude-code`=파일시스템 아카이브(AC-E5, `claude-code-workload.md`)
- **검증 방법**: 세션을 60분간 미사용으로 두면 스냅샷이 생성되고 상태가 `snapshot`으로 바뀌며 pod가 회수됨을 확인한다. 경계값(예: 59분)에서는 동결되지 않으며 output 없는 SSE keepalive만으로 동결이 영구 차단되지 않음을 확인한다. snapshot으로 stream이 끊긴 뒤 SPA가 자동 read/stream으로 즉시 복원하지 않고 Restore 화면으로 이동하는지도 확인한다.

### AC-B2: 스냅샷 복원
- **설명**: `snapshot` 상태의 세션에 접근(read/write/전환)하면, 시스템은 새 pod에 체크포인트를 복원하고 세션을 `active`로 전이한다. 복원 메커니즘은 타입별로 다르다 — `shell`은 CRIU 복원, `claude-code`는 아카이브 전개(AC-E5).
- **달성 가치**: V3
- **검증 방법**: `snapshot` 세션 접근 시 복원이 수행되어 `active`로 전이되고 응답 가능 상태가 됨을 확인한다.

### AC-B3: 스냅샷·복원 무결성
- **설명**: 스냅샷 생성 직전의 세션 상태가 복원 후에도 보존된다. 체크포인트/복원 과정에서 세션 데이터 손실이 발생하지 않는다.
- **달성 가치**: V3
- **구체화**: 보존 대상 상태의 정체는 타입별로 다르다 — `shell`은 **인메모리** 쉘 프로세스 트리(환경변수·작업디렉터리·쉘 변수·포그라운드 자식·열린 FD) → AC-D4 (`shell-workload.md`), `claude-code`는 **온디스크** 대화 기록·작업 디렉터리·누적 출력 버퍼 → AC-E5 (`claude-code-workload.md`). 두 경우 모두 "복원 후 이전 문맥 위에서 이어짐 + 커서 유효"라는 관측 계약은 같다.
- **검증 방법**: `shell`은 동결 전 인메모리 변수·cwd·scrollback cursor를, `claude-code`는 대화 기록·작업 디렉터리·bounded output과 cursor를 마커로 세팅 → 타입별 스냅샷 → 복원 후 동일 상태가 유지되는지 확인한다.
