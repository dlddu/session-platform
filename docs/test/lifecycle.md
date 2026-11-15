# 테스트 문서: 세션 라이프사이클 & 워크로드별 스냅샷

## 검증 대상 AC
- AC-B1: 60분 유휴 후 스냅샷 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)
- AC-B2: 스냅샷 복원 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)
- AC-B3: 스냅샷·복원 무결성 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)
- AC-B4: 삭제된 세션 복원 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)
- AC-B5: 파드 종료 시 자동 아카이브 (PRD: 세션 라이프사이클 & 워크로드별 스냅샷)

## 테스트 시나리오

### 시나리오 1: 60분 유휴 도달 시 스냅샷 (경계값 포함)
- **사전 조건**: active 세션 1개 존재, 유휴 타이머 측정 기준이 설정됨
- **실행 단계**: (a) 세션을 59분간 미사용 후 상태 확인 → (b) Claude 세션에는 passive output SSE를 열고 keepalive만 전달 → (c) 추가로 1분 더 미사용(누적 60분) 후 상태 확인
- **기대 결과**: 59분 시점에는 동결되지 않음. passive stream의 output/reset/keepalive 신호 자체는 `lastAccess`를 touch하지 않아 60분 시점에는 workload별 filesystem archive snapshot(*2026-11-01*: 모든 타입 공통 — 보존 대상만 `shell`=홈 디렉터리+scrollback, `claude-code`=workspace·HOME·대화+scrollback, 게이트 타입(`minimax-code`)=그것+공유 볼륨) 생성 + 상태 `snapshot` 전이 + pod 회수. reset 뒤 read(0) reconciliation을 실제 호출하면 일반 read 활동이므로 이 시나리오의 “미사용” 조건이 깨진다. 해당 workload의 snapshot gate가 꺼져 있으면 reaper는 대상을 skip/log하고 live pod를 보존하며, 명시적 Snapshot 호출은 unavailable 오류를 반환
- **검증 AC**: AC-B1

### 시나리오 2: 스냅샷 세션 접근 시 복원
- **사전 조건**: `snapshot` 상태 세션 1개 존재
- **실행 단계**: 해당 세션에 명시적 read(또는 write/전환) 요청 — passive stream은 복원 접근에서 제외
- **기대 결과**: workload별 archive 복원(모든 타입 filesystem archive, 복원 대상 pod는 `session-platform.dev/restore-archive` 주석을 가짐)으로 새 pod에 상태 복원, 상태 `active` 전이, 정상 응답
- **검증 AC**: AC-B2

### 시나리오 3: 복원 후 상태 무결성
- **사전 조건**: active 세션에 workload별 마커 상태(`shell`=홈 디렉터리 파일/output cursor — *2026-11-01 개정*, 이전 판은 인메모리 변수/cwd, `claude-code`=대화 기록/workspace/output cursor, 게이트 타입(`minimax-code`)=그것+공유 볼륨 파일) 세팅
- **실행 단계**: 세션 동결(스냅샷) → 이후 접근으로 복원 → 마커 상태 조회
- **기대 결과**: 동결 직전 마커 상태가 손실 없이 동일하게 보존됨. `shell`은 보존 대상이 아닌 환경 변수·cwd가 이어지지 않는 것(새 쉘)까지가 기대 결과다(AC-D4)
- **검증 AC**: AC-B3
- **구체 마커 구현**: `shell`의 홈 디렉터리 파일 보존·환경 변수/cwd 비보존·커서 왕복은 T-쉘워크로드 시나리오 4와 `TestScenario4_ShellArchiveIntegrity`(integration)·`TestShellArchive_HomeFilesSurviveAndTheShellStartsFresh`(배포 e2e)가 검증한다. 게이트 타입의 공유 볼륨 왕복은 T-승인게이트 시나리오 6이 검증한다. `claude-code`의 workspace·CLI home·resume flag·bounded scrollback·pre-snapshot cursor 왕복은 T-클로드코드 시나리오 6·8·9와 data-plane archive/control-plane transaction 단위 테스트가 검증한다. 실제 provider API를 호출하는 Claude 배포 smoke는 별도 opt-in 범위다.

### 시나리오 4: stream 단절이 snapshot을 자동 복원하지 않음
- **사전 조건**: workspace가 passive SSE를 보고 있는 Claude 세션과 동결 가능한 시계
- **실행 단계**: 60분 유휴 snapshot으로 pod 회수·SSE 단절 → SPA `onerror`와 후속 API 호출 관찰
- **기대 결과**: SPA가 native EventSource를 즉시 close하고 GET session으로 `snapshot`을 확인한 뒤 자동 stream/read 재시도를 하지 않음. Restore 화면으로 이동하며 사용자가 명시적으로 복원하기 전에는 새 pod가 생기지 않음
- **검증 AC**: AC-B1, AC-B2 (보조 AC-E3)

### 시나리오 5: 삭제된 세션을 id로 복원
- **사전 조건**: 아카이브 게이트가 켜진 타입(`shell`)의 active 세션 1개, 누적 출력에 마커 줄이 있음
- **실행 단계**: 세션 동결(`POST /snapshot`) → 삭제(`DELETE`) → 조회가 404인 것 확인 → `POST /sessions/{id}/recover` → 같은 id로 다시 recover → 한 번도 동결되지 않은 id로 recover
- **기대 결과**: 첫 recover가 200으로 **같은 id·이름**의 `active` 세션을 **동결 전과 다른 pod** 위에 돌려주고, 뒤이은 조회도 같다. offset 0 read가 동결 전 마커 줄을 포함한다(누적 출력 보존). 살아 있는 id의 recover는 409, 매니페스트가 없는 id는 404다
- **검증 AC**: AC-B4 (보조 AC-B2, AC-B3)
- **단위 수준 보조 검증**: 테넌트 범위(다른 테넌트 404 · 좁혀진 타입 403 · 운영자 허용)는 `control-plane/internal/api/recover_test.go`, 매니페스트 쓰기 실패 시 동결 롤백·게이트 꺼짐 503·거부 시 무기록은 `control-plane/internal/service/recover_test.go`, 매니페스트 키·버전·403과 404 구분은 `control-plane/internal/adapter/checkpointstore/manifest_test.go`가 검증한다.

### 시나리오 6: 파드 종료 시 자동 아카이브와 채택
- **사전 조건**: 아카이브 게이트와 자동 아카이브(`AUTO_ARCHIVE_ENABLED`)가 켜진 배포. 타입별(`shell`·`claude-code`·`minimax-code`) active 세션, 누적 출력에 마커 줄, 작업 디렉터리(홈)에 마커 파일. `claude-code`는 오래 도는 턴 하나와 대기 프롬프트 하나
- **실행 단계**: (a) control plane을 거치지 않고 세션 파드를 지운다(스팟 회수·노드 비우기와 같은 경로) → 파드가 사라진 뒤 read (b) 다른 세션을 `POST /snapshot`으로 동결한 뒤 그 세션의 아카이브 키(`sessions/<id>/checkpoint.tar`)와 `writer`를 확인 (c) 세 번째 세션을 `DELETE`로 지운 뒤 `POST /recover` (d) 자동 아카이브를 올릴 수 없는 세션(업로드 실패)의 파드를 지운 뒤 read
- **기대 결과**: (a) read가 200이고 세션이 **지우기 전과 다른 파드** 위의 `active`이며, 지우기 전 누적 출력 마커와 파일이 그대로다. `claude-code`는 진행 중이던 턴이 `status=canceled`로 끝나 있고 대기 프롬프트는 실행되지 않았으며 그 사실이 출력에 한 줄 남는다. `minimax-code`는 헬퍼 파드가 업로드를 중계한 뒤 사라진다 (b) 키의 `writer`가 동결의 세대(`generation:…`)다 — 동결로 회수된 파드는 자동 아카이브를 올리지 않는다. 세션마다 아카이브 객체는 하나다 (c) recover가 지우기 직전 상태로 `active`가 된다 — 한 번도 동결되지 않은 세션이어도 같다 (d) 키에 이전 동결이 남아 있어도 채택하지 않고 404이며 레코드가 지워진다(AC-C5)
- **검증 AC**: AC-B5 (보조 AC-B2, AC-B3, AC-B4, AC-C5)
- **단위 수준 보조 검증**: URL 전달·1시간 이하 갱신·10분 재전달, 채택과 정리, 이전 동결이 남은 키의 채택 거부, 리퍼의 채택, 동결 확정 시 URL 회수, 삭제 세션의 `writer`에 따른 복원 선택은 `control-plane/internal/service/auto_archive_test.go`, 세션당 키·`writer` 읽기(404와 403 구분)·2시간 미만 자격 거부·`writer` 헤더를 덮는 체크섬 없는 서명은 `control-plane/internal/adapter/checkpointstore/auto_archive_test.go`, 동결의 세션 키·세대 `writer`와 덮어쓰인 아카이브 복원(경고)은 `control-plane/internal/adapter/archive/agent_checkpointer_test.go`, 다른 주소 URL·메타데이터 밖 헤더 거부·`writer` 헤더 전달·`shell` 업로드 후 복원·URL 없음/회수됨/복원 전 건너뜀·턴 중단과 대기 프롬프트 폐기·헬퍼 중계와 대기는 `data-plane/cmd/agent/auto_archive_test.go`, 파드의 env·preStop·유예 시간은 통합 `control-plane/test/auto_archive_orchestrator_test.go`가 검증한다.
