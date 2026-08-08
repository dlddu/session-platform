# J2: 자리 비움과 끊김 없는 재개 (active → idle → snapshot → 복원)

> 사용자 여정 문서의 일부입니다. 페르소나 정의·가치 커버리지·미해결 항목은 [README.md](./README.md) 참고.

- **페르소나**: P1 멀티세션 작업자
- **상황**: 작업자가 세션을 켜둔 채 한동안 자리를 비웠다가 나중에 돌아온다.
- **달성 가치**: **V2 유휴 자원 회수**, **V3 끊김 없는 세션 연속성**

## 단계

> 단계 표기: `J2-S{n}` · 각 단계 끝의 *mockup* 항목은 시각화 산출물 연결 상태입니다.

1. **J2-S1 · 작업 중 이탈** — 작업자가 세션에서 손을 떼고 다른 일을 한다. 유휴 시간은 **마지막 클라이언트 read/write**부터 잰다. 현재 reaper는 `active`/`idle` 레코드를 직접 검사해 동결하며, 별도의 operational `active→idle` producer는 아직 없다. *(mockup: 부분 — index.html 목록의 idle 상태 / workspace.html lifecycle; 전용 화면 없음)*
2. **J2-S2 · 60분 도달 시 동결** — 유휴 60분(최대 한계)에 도달하면 시스템이 workload별 snapshot(`shell`=CRIU 체크포인트, `claude-code`=filesystem archive)을 생성하고 상태를 `snapshot`으로 전이한 뒤 pod와 점유 자원(CPU/메모리)을 회수한다. 작업자는 이 과정을 직접 보지 않는다. *(관련 AC: AC-B1, AC-A3)* *(mockup: 부분 — restore.html/agent-workspace.html의 auto-freeze; 동결 진행 전용 화면 없음)*
3. **J2-S3 · 재접근** — 작업자가 나중에 그 세션에 다시 접근(read/write/전환)한다. *(mockup: 없음)*
4. **J2-S4 · 복원 후 재개** — 시스템이 새 pod에 workload별 snapshot을 복원하고 상태를 `active`로 전이한다. `shell`은 CRIU로 프로세스 트리를, `claude-code`는 archive로 대화 기록·작업 디렉터리·bounded output/cursor를 이어받는다. 작업자는 멈췄다는 사실을 거의 인지하지 못한 채 작업을 잇는다. *(관련 AC: AC-B2, AC-B3)* *(mockup: restore.html(shell), agent-workspace.html(claude-code))*
