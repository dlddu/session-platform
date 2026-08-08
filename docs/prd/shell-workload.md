# PRD: 세션 워크로드 — 인터랙티브 쉘

> 대상 요구사항: 세션 워크로드의 정체 확정 — **세션 = 전용 pod에서 실행되는 인터랙티브 쉘**
> (data plane 워크로드 미정의 상태 해소, `../../data-plane/README.md` 참고)
>
> 📌 **범위 (2026-08-08 개정)**: 아래 AC-D1~D5는 **`workloadType=shell` 세션에만** 적용된다. 워크로드 타입이 복수가 되면서(AC-E1) 쉘은 유일한 워크로드가 아니라 **기본 타입**이 되었다. `claude-code` 타입의 대응 명세는 `claude-code-workload.md`(AC-E1~E6)이며, 두 문서는 같은 상위 AC(AC-A1·B1·B2·B3·C2·C3)에 대한 타입별 구체화로 나란히 존재한다.

## 달성 가치
- **V1 세션 격리** — 쉘 프로세스 트리를 세션 전용 pod에 가둠 (AC-A1/A2 구체화)
- **V2 유휴 자원 회수** — 유휴 판정 기준을 클라이언트 쉘 I/O로 확정 (AC-B1 구체화)
- **V3 끊김 없는 세션 연속성** — CRIU 체크포인트/복원의 대상이 쉘 프로세스 트리임을 확정하고(AC-B3 구체화), read/write를 쉘 입출력 시맨틱으로 확정 (AC-C2/C3 구체화)
- **V4 자유로운 멀티세션 전환** — 전환 후에도 동일한 offset 커서 규약으로 출력을 이어 읽음 (AC-C2 구체화)

> 📌 **가치 연결 규칙 (2026-08-08)**: 이 PRD는 새 가치를 제공하는 것이 아니라 상위 AC를 **타입별로 구체화**한다. 따라서 각 AC의 달성 가치는 자신이 구체화하는 상위 AC의 가치를 그대로 상속한다 (예: AC-D3 → AC-C2 → V3·V4). 이전 판에서 참조하던 "V6 인터랙티브 쉘 세션"은 워크로드 타입을 가치로 올린 것이어서 `../values.md`에서 삭제되었다.

> 이 PRD는 새 메커니즘을 추가하기보다, 기존 PRD들이 추상적으로 다루던 **"세션 워크로드"·"인메모리 상태"·"read/write"**를 인터랙티브 쉘의 구체 개념으로 못박는다. 아래 각 AC의 "구체화 대상"이 그 연결이다.

## Acceptance Criteria

### AC-D1: 세션 워크로드 = 인터랙티브 쉘 프로세스
- **설명**: `workloadType=shell` 세션의 data plane pod가 Ready가 되면, 그 pod 안에서 정확히 하나의 **인터랙티브 쉘**이 PTY(pseudo-terminal)에 연결되어 기동된다. 기본 쉘은 `/bin/bash`이며 `DATA_PLANE_SHELL` 환경변수로 오버라이드할 수 있다. 이 쉘 프로세스와 그 자식 프로세스 트리가 세션 워크로드의 전부다. control plane은 쉘을 직접 실행하지 않고 pod 오케스트레이션만 담당한다(AC-A1 유지).
- **달성 가치**: V1
- **구체화 대상**: AC-A1의 "실제 세션 워크로드", AC-A2의 "전용 pod"
- **검증 방법**: 세션 생성 → pod Ready 후 해당 pod 안에 PTY에 연결된 쉘 프로세스가 정확히 1개 존재함을 확인한다. control plane 프로세스에는 쉘이 없음을 확인한다.

### AC-D2: write = 쉘 입력(stdin)
- **설명**: `POST /sessions/{id}/write`의 `payload`는 대상 세션 쉘의 stdin(PTY master)에 그대로 주입된다. 개행이 포함된 입력은 쉘이 명령으로 해석·실행한다. write는 입력을 전달하고 즉시 반환하며, 명령 완료나 출력 수집을 블로킹하지 않는다(출력은 AC-D3의 read로 회수). 여기서 "비블로킹"은 **명령 완료를 기다리지 않는다**는 의미다 — 단일 write의 payload가 커널 PTY 입력 버퍼 한계를 넘는 극단 상황의 거동은 이 스펙의 범위 밖이다.
- **달성 가치**: V3
- **구체화 대상**: AC-C3(Write API 상태별 분기)의 "write" 시맨틱
- **검증 방법**: active 세션에 `payload="echo hello\n"`로 write 후 read(`offset=0`)하면 출력에 `hello`가 포함됨을 확인한다. write가 명령 실행 완료를 기다리지 않고 반환함을 확인한다.

### AC-D3: read = 쉘 출력(stdout/stderr), offset 커서 기반 델타
- **설명**: `POST /sessions/{id}/read`는 요청의 `offset`(직전 read 응답이 발급한 `nextOffset` 커서, 생략 시 0) **이후에** 쉘 PTY에 누적된 출력(stdout·stderr 병합)을 순서 보존하여 `payload`로 반환하고, 다음 read에 쓸 새 `nextOffset`(현재 누적 길이)을 함께 발급한다. read는 비파괴적(non-consuming)이다 — 서버는 어떤 출력도 버리지 않으므로 `offset=0`으로 read하면 언제든 **세션 시작 이후 전체 출력**이 반환되고, 같은 `offset`으로 여러 번 read해도 같은 구간이 반복 반환된다. `offset`이 현재 누적 길이보다 크면 빈 `payload`와 현재 누적 길이의 `nextOffset`을 반환한다. 아직 출력이 없으면 빈 `payload`를 반환한다.
- **달성 가치**: V3, V4
- **구체화 대상**: AC-C2(Read API 상태별 분기)의 "read" 시맨틱
- **검증 방법**: 여러 명령을 write한 뒤 read(`offset=0`)하면 모든 명령의 출력이 실행 순서대로 전부 포함됨을 확인한다. 그 응답의 `nextOffset`으로 곧바로 다시 read하면 (새 출력이 없는 한) 빈 `payload`가 반환되고, 그 사이 새 명령을 write했다면 커서 이후의 신규 출력만 반환됨을 확인한다. `offset=0` 재조회는 여전히 전체 출력을 반환함을 확인한다(비파괴성).

> 📌 **설계 노트 (버퍼 증가)**: 페이지네이션은 `offset` 커서로 해소되어 반복 read의 `payload` 크기는 델타만큼으로 유지된다. shell 누적 버퍼 자체의 상한/ring buffer는 계속 보류 항목이다. snapshot 때는 `/checkpoint`가 scrollback을 CRIU images와 같은 archive에 별도 직렬화하고 `/restore`가 preload한다. `../doc-tracker.md`의 열린 항목 참고.

> 📌 **설계 노트 (offset과 복원)** — *2026-08-08 갱신 (J5-S4/CRIU)*: snapshot→restore(AC-B2/AC-D4)를 거친 세션에서 복원 전 발급된 `nextOffset` 커서는 **유효하게 유지된다**. scrollback은 에이전트 메모리에 있지만 CRIU 대상은 쉘 프로세스 트리이므로, `/checkpoint`가 버퍼를 archive에 별도 직렬화하고 `/restore`가 CRIU restore 전에 동일 바이트열로 preload한다. 따라서 복원 후 클라이언트는 복원 전 `nextOffset`으로 read하면 델타만 받고(전체 재전송 아님), `offset=0`은 여전히 동결 전·후 전체 이력을 반환한다. (컨테이너 *재시작*(RestartPolicy Always)은 빈 버퍼의 새 에이전트로 시작하므로 이와 다르다 — 복원은 이어지고, 재시작은 이어지지 않는다.) 누적 버퍼 자체의 상한/ring buffer는 계속 열린 항목(`../doc-tracker.md`).

### AC-D4: 쉘 프로세스 트리 = 보존 대상 상태
- **설명**: 세션이 snapshot으로 동결될 때 CRIU 체크포인트의 대상은 이 쉘 프로세스 트리이며, 보존되는 "인메모리 상태"(AC-B3)는 구체적으로 다음을 포함한다: 환경 변수, 현재 작업 디렉터리, 쉘 변수·함수·alias, 실행 중인 포그라운드 자식 프로세스, 열린 파일 디스크립터. 복원(AC-B2) 후 쉘은 동결 직전의 프롬프트·작업 맥락 그대로 재개되어, 이어서 write하면 동결 이전 문맥 위에서 실행된다.
- **달성 가치**: V3
- **구체화 대상**: AC-B3(스냅샷·복원 무결성)의 "인메모리 상태"
- **검증 방법**: 동결 전 세션에서 `export MARKER=42`, `cd /tmp`를 write → 스냅샷 → 복원 후 `echo $MARKER; pwd`를 write·read하여 `42`와 `/tmp`가 반환됨을 확인한다(AC-B3 검증의 구체 마커).

### AC-D5: 유휴 판정 = 클라이언트 쉘 I/O 부재
- **설명**: 세션의 유휴 카운트 기준 시점(`lastAccess`, AC-B1)은 **해당 세션에 대한 마지막 read 또는 write**, 즉 마지막 클라이언트 쉘 I/O 시점으로 정의된다. 쉘이 클라이언트 입력 없이 자체적으로 출력을 내는 것(예: 백그라운드 작업 로그)은 유휴 판정에 영향을 주지 않는다 — 유휴는 쉘의 바쁨(busyness)이 아니라 **클라이언트 접근** 부재로 측정된다.
- **달성 가치**: V2
- **구체화 대상**: AC-B1(60분 유휴 후 스냅샷)의 "마지막 read/write 이후 경과 시간"
- **검증 방법**: write 없이 read만 반복해도 `lastAccess`가 갱신됨을 확인한다. 반대로 read/write 없이 쉘이 자체 출력만 내는 상황에서는 `lastAccess`가 갱신되지 않아 유휴 카운트가 진행됨을 확인한다.

> ⚠️ **미해결 (트리거 정책과 연동)**: 장시간 실행되는 포그라운드 작업(예: `make build`)이 도는 도중 60분 클라이언트 유휴가 도달하면, AC-D5 정의상 스냅샷 대상이 된다. CRIU는 실행 중 프로세스도 체크포인트하므로 기술적으로는 가능하나, "바쁜 쉘을 동결해도 되는가"는 AC-B1의 **스냅샷 트리거 정책**(grace/override, `session.go`의 `TODO(policy)`)과 함께 결정해야 할 별개의 열린 항목이다. `../doc-tracker.md` 참고.
