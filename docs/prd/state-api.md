# PRD: 세션 상태 일관성 & Read/Write API

> 대상 요구사항: ④ ConfigMap(resourceVersion CAS) + Lease 기반 atomic operation, ⑥ 세션 단위 read/write API (상태별 분기), ⑦ 세션 간 자유 전환

## 달성 가치
- **V3 끊김 없는 세션 연속성** — 상태와 무관하게 read/write가 동작
- **V4 자유로운 멀티세션 전환** — 세션 간 이동 보장
- **V5 일관된 세션 상태** — ConfigMap(resourceVersion CAS) + Lease 기반 atomic operation으로 상태 일관성 확보

## Acceptance Criteria

### AC-C1: ConfigMap(resourceVersion CAS) + Lease 기반 atomic 상태 전이
- **설명**: 세션 상태 전이(active ↔ idle ↔ snapshot)와 세션 점유는 Kubernetes ConfigMap의 resourceVersion 낙관적 동시성(compare-and-swap)과 `coordination.k8s.io` Lease를 통한 atomic operation으로 처리된다. 공개 state뿐 아니라 session aggregate 전체를 한 번에 비교·교체하는 CAS를 사용하며, 동시 `Touch`가 기록한 더 최신 `lastAccess`는 merge한다. `claude-code` archive snapshot에서만 aggregate에 private owner/phase/generation transaction이 들어가고 owner-fenced `preparing→committing` 전이를 사용한다. shell CRIU에는 아직 동등한 durable post-dump transaction이 없다. 장시간 Snapshot·Restore·recovery는 기본 15초 Lease를 5초마다 갱신하고 각 Renew에도 5초 deadline을 적용해, holder 상실·hung API 호출 시 후속 side effect를 취소한다. 동일 세션에 대한 동시 요청(예: 복원과 스냅샷이 동시 발생)에서도 Lease/CAS 단일 승자만 상태를 전이하며, Claude transaction에서는 단일 owner/phase 전이만 성공한다. 상태는 control plane 다중 replica가 공유하므로 어느 replica가 처리하든 동일하게 보인다.
- **달성 가치**: V5
- **검증 방법**: 동일 세션에 복원/스냅샷/전환 요청을 동시 다발로 발생시켜, 최종 상태가 항상 유효한 단일 상태로 수렴하고 중복 pod 기동·이중 스냅샷이 발생하지 않음을 확인한다. 장시간 전이에서 Lease가 갱신되고 Renew timeout/holder 상실 시 side effect가 취소됨을 확인한다. Claude archive에서는 stale owner가 새 snapshot generation/phase를 진전·삭제하지 못하고, `Touch`와 aggregate CAS가 겹쳐도 private transaction과 최신 `lastAccess`가 모두 보존됨을 추가로 확인한다.

### AC-C2: Read API 상태별 분기
- **설명**: 세션 단위 Read API는 대상 세션을 먼저 `active`로 만든 뒤 그 pod에서 읽는다(통일 규칙: 비-active 접근은 "active 보장 후 처리"). 상태별로 active 보장 경로만 다르다.
  - `active`: pod에서 직접 읽어 즉시 응답
  - `idle`: `idle→active` atomic 승격(AC-C1) 후 pod에서 읽기 (idle은 pod를 아직 보유)
  - `snapshot`: workload별 복원(`shell`=CRIU, `claude-code`·`approval-gated`=filesystem archive)으로 `active` 전이(AC-B2) 후 읽기
  - 이는 switch(AC-C4)·snapshot 접근(AC-B2)과 동일한 "접근 시 active화" 원칙을 read에 적용한 것이다.
- **달성 가치**: V3, V4
- **구체화**: read가 반환하는 것은 워크로드 타입별로 다르다 — `shell`은 누적된 쉘 stdout/stderr → AC-D3 (`shell-workload.md`), `claude-code`는 투영된 assistant text delta와 diagnostic stderr → AC-E3 (`claude-code-workload.md`), `approval-gated`는 거기에 승인 대기·결정 in-band 마커가 섞인 같은 바이트열 → AC-F3 (`approval-gated-workload.md`). **append-only byte offset 커서 규약(비파괴·`offset=0`=전체)은 세 타입이 동일**하다.
- **검증 방법**: active/idle/snapshot 세션에 각각 read를 호출하여, 각 경로(active 직접 / idle 승격 후 / snapshot 복원 후)로 처리되고 호출 후 최종 상태가 `active`이며 올바른 결과를 반환함을 확인한다.

> 📌 **Passive live output stream**: `GET /sessions/{id}/stream`은 AC-E3의 실시간 관찰 경로이며 위 Read API처럼 “active 보장 후 처리”하지 않는다. 현재 session을 `Get`한 뒤 active 또는 pod를 보유한 idle 상태에서만 기존 pod의 같은 append-only output을 SSE로 전달한다. 상태를 승격하거나 snapshot을 복원하거나 `lastAccess`를 touch하지 않으며 snapshot에는 invalid-state 오류를 반환한다. output event id는 `nextOffset`, data는 `{offset,payloadBase64,nextOffset}`이고 decoded byte 길이는 `nextOffset-offset`이어야 한다. Base64는 정확한 byte 범위와 overlap을 보존하며, Claude의 모든 서버 발급 cursor/event 경계는 UTF-8 code-point 경계다.
>
> 최초 연결은 non-negative query `offset`을 사용하고, 재연결의 non-negative decimal `Last-Event-ID`가 있으면 query보다 우선한다. cursor가 현재 보존 길이보다 크면 `reset` event의 id와 `data.nextOffset`으로 현재 길이를 알린다. reset을 받은 SPA는 decoder의 미완성 bytes를 버리고 read(`offset=0`)로 전체 콘솔을 교체한 뒤 read가 발급한 cursor에서 stream을 재개한다. SSE 연결·output/reset event·comment keepalive 자체는 상태나 `lastAccess`를 바꾸지 않지만, 이 reset reconciliation은 일반 Read API라 idle을 active로 승격하고 `lastAccess`를 갱신할 수 있다. 연결은 workspace 수명 동안 유지되며 run/queue 완료 event는 없다. SPA는 전송 오류 시 native EventSource를 먼저 닫고 session 상태를 조회한다. active/idle만 마지막 cursor로 backoff 재연결하고, snapshot이면 read/stream 자동 재시도를 멈춘 채 Restore 화면으로 이동한다.

### AC-C3: Write API 상태별 분기
- **설명**: 세션 단위 Write API도 read와 같은 통일 규칙을 따른다. 대상 세션을 먼저 `active`로 만든 뒤 write를 적용한다.
  - `active`: pod에 직접 write
  - `idle`: `idle→active` atomic 승격(AC-C1) 후 write
  - `snapshot`: workload별 복원(`shell`=CRIU, `claude-code`·`approval-gated`=filesystem archive)으로 `active` 전이(AC-B2) 후 write — **snapshot write는 거부하지 않고 복원 후 적용한다** (AC-B2의 "접근=복원"과 일치)
- **달성 가치**: V3, V5
- **구체화**: write가 반영하는 것은 워크로드 타입별로 다르다 — `shell`은 쉘 stdin 입력(명령/키 입력) → AC-D2 (`shell-workload.md`), `claude-code`는 프롬프트 1회 실행 트리거 → AC-E2 (`claude-code-workload.md`), `approval-gated`는 같은 프롬프트 실행이되 외부 도구가 승인 게이트에서 큐 안에 블로킹된다 → AC-F3 (`approval-gated-workload.md`). **비블로킹 반환 규약은 세 타입이 동일**하다 — 승인 대기도 write의 반환을 늦추지 않는다.
- **검증 방법**: active/idle/snapshot 세션에 각각 write 요청 시 대상이 `active`로 처리되어 데이터가 일관되게 반영되고 상태 전이가 atomic하게 일어남을 확인한다.

### AC-C4: 세션 간 자유 전환
- **설명**: 사용자는 보유한 여러 세션 사이를 자유롭게 전환할 수 있다. 전환 대상이 `snapshot`이면 복원하여 `active`로, 이미 `active`면 그대로 접근시킨다. 전환은 워크로드 타입과 무관하게 동일하게 동작하며, 세 타입(`shell`·`claude-code`·`approval-gated`)이 섞여 있어도 마찬가지다(AC-E1·AC-F1). 전환은 세션 격리(AC-A2 — 워크로드 파드 1:1과 보조 파드의 세션 전속)를 깨지 않는다.
- **달성 가치**: V4, V3
- **검증 방법**: 여러 세션을 오가며 전환 시 매번 대상 세션이 올바른 상태로 활성화되고, 이전 세션의 상태가 보존됨을 확인한다.

> 📌 **HTTP body 계약**: read/write/switch는 body 생략을 허용한다(read=`offset:0`, write=빈 payload,
> switch=필드 없음). body가 있으면 JSON object 한 값만 허용하고 explicit `null`(top-level 또는
> 선언된 field 값)·unknown field·malformed JSON·trailing JSON은
> 400으로 거부한다. 따라서 existing-session route로 immutable workload/model을 바꾸려는 요청도 400이며
> metadata는 바뀌지 않는다. 모든 JSON POST wire body는 8 MiB로 제한되고 서버는 body read에 30초
> timeout을 둔다. wire 상한 초과는 413이다. `claude-code` write는 decoded UTF-8 payload 1 MiB를 추가로
> 제한하며, snapshot 세션도 이 검증을 복원보다 먼저 수행해 초과 prompt를 413으로 거부한다.
>
> stream은 JSON body와 30초 JSON I/O timeout의 대상이 아닌 request-context-bounded GET이다.
> query/header cursor가 음수·비정수이면 output side effect 없이 400으로 거부한다.
