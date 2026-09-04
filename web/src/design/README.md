# Design System — Session Pod Platform

> **정본은 코드입니다.** 이 디렉터리와 `../app/shell.css`에 적힌 것이 이 레포의 시각 언어이고,
> 이 문서는 그 코드를 가리키는 얇은 색인일 뿐입니다. 값을 바꾸려면 문서가 아니라 **코드를 고칩니다.**
>
> 문서 사슬(`가치 → 여정 → mockup ↔ 디자인 시스템`)에서의 위치와 위험은
> [`../../../docs/doc-structure-state.md`](../../../docs/doc-structure-state.md)에서 추적합니다.

---

## 세 축이 사는 곳

| 축 | 정본 파일 | 내용 |
|----|-----------|------|
| **토큰** | [`tokens.css`](./tokens.css) `:root` | 표면·라인·텍스트 색, 세션 상태 색, 반경, 레일/최대폭, 이징 |
| **기초 타이포·프리미티브** | [`tokens.css`](./tokens.css) `:root` 아래 | `body` 기본 타이포, `.mono`, `.eyebrow`, `h1`, `.sub`, `.btn`/`.btn-primary`/`.btn-ghost`, `:focus-visible`, `::selection`, reduced-motion |
| **컴포넌트·패턴** | [`../app/shell.css`](../app/shell.css) | 아래 목록 |
| **컴포넌트 동작** | [`../app/`](../app/) `*.tsx` | `AppShell`, `SessionCard`, `StateBadge`, `Toast`, `icons` |

폰트는 [`../../index.html`](../../index.html)에서 로드합니다 — Space Grotesk(제목) / IBM Plex Sans(본문) / IBM Plex Mono(콘솔·라벨).

---

## 상태 색은 도메인에 묶여 있다

이 시스템에서 색은 장식이 아니라 `session.State` enum의 표현입니다. 임의로 바꾸면 도메인 의미가 깨집니다.

| 상태 | 토큰 | soft 배경 |
|------|------|-----------|
| `active` | `--active` | `--active-soft` |
| `idle` | `--idle` | `--idle-soft` |
| `snapshot` (UI 표기: frozen) | `--frozen` | `--frozen-soft` |
| 복원 전이 | `--resume` | — |

---

## 컴포넌트 목록 (`shell.css`)

역할별로 묶은 것이고, 정확한 정의는 항상 파일을 봅니다.

- **셸/레이아웃** — `.app` `.rail` `.rail-btn` `.rail-me` `.rail-spacer` `.mark` `.viewport` `.pad` `.h-top` `.grid` `.panel` `.bar` `.crumbs` `.back`
- **세션 표현** — `.card` `.card-head` `.card-foot` `.c-name` `.c-id` `.badge` `.chip` `.summary` `.vit` `.lc-state` `.reclaim-lab`
- **콘솔/작업 영역** — `.console` `.console-bar` `.term` `.input-row` `.ws-body` `.steps` `.step`
- **스냅샷·복원** — `.crystal` `.ring` `.freeze` `.snap-body` `.snap-meta` `.snap-hint`
- **오버레이·피드백** — `.modal` `.modal-actions` `.scrim` `.toast` `.toast-wrap` `.error` `.empty` `.field`

---

## mockup과의 관계 (방향에 주의)

`docs/mockups/*.html`은 이 시스템을 **참조하지 않습니다.** 각 파일이 자기 `:root`에 같은 값을 인라인으로 복사해 갖고 있습니다.
역사적으로는 mockup이 먼저였고 `tokens.css`가 거기서 1:1로 이식됐습니다 — 그래서 값은 같지만 **연결된 것은 아닙니다.**

그리고 복제는 mockup에만 있지 않습니다 — 문서 포털 허브(`docs/index.html`)와 여정 페이지
(`docs/journeys/*/index.html`)도 같은 값을 자기 `:root`에 갖고 있습니다.

결과적으로 지금 토큰 하나를 바꾸려면 최소 **7**곳 · 최대 **13**곳을 고쳐야 합니다.
**하나의 숫자가 아닌 것이 핵심입니다** — 복제한 파일마다 갖고 있는 토큰이 달라서, 몇 곳을
고쳐야 하는지는 *어느 토큰을 바꾸느냐*에 달려 있습니다. `--rail`·`--idle`·`--resume`처럼 mockup만
쓰는 토큰은 7곳이고, `--ink`·`--line`·`--text`처럼 허브·여정 페이지까지 쓰는 토큰은 13곳입니다.
상한이 10에서 13으로 오른 것은 2026-09-03 (#56)이 여정 페이지 3개를 더 신설했기 때문입니다.
이 중복은 알려진 미해소 항목이며 `docs/doc-structure-state.md`의 🟢 위험으로 추적됩니다.

이 두 숫자와 아래 원장은 `scripts/check-render-fidelity.py`(R10)가 `tokens.css`를 정본으로
**레포 전수를 실측해** 대조합니다. 손으로 세지 않으므로 실제와 어긋날 수 없고, 갱신은
`python3 scripts/check-render-fidelity.py --emit`이 만들어 줍니다.

**당분간의 규칙**: 코드(`tokens.css`)를 먼저 고치고, mockup은 그 값을 따라옵니다. 반대 방향이 아닙니다.

---

## 토큰 복제 원장

`tokens.css` `:root`의 정본 토큰을 자기 `:root`에 다시 선언하는 **모든** tracked 파일입니다.
R10이 레포 전수 실측과 이 표를 **양방향으로** 대조합니다 — 표에 없는 복제 파일도, 실제로는
복제하지 않는 표 행도 실패합니다. 즉 **새 복제가 조용히 들어올 수 없습니다.**

- **처분 `DUP`** — 정본 팔레트를 그대로 복제한 파일. **값 불일치가 0이어야 하며**, 하나라도
  어긋나면 R10이 즉시 실패합니다("값은 같지만 연결은 아니므로 어느 한쪽만 바뀌면 조용히
  어긋난다"는 위험을 실제로 집행하는 자리입니다). 유지비 계산에 들어갑니다.
- **처분 `IND`** — 이름만 겹치는 **독립 팔레트**. 정본을 따라갈 이유가 없으므로 값 불일치가
  허용되고 유지비에서 빠지지만, **사유가 없으면 실패**합니다(사유 없는 예외 금지).

값 비교는 표기 차이(hex 대소문자, `cubic-bezier(.22,…)`의 선행 0·공백)를 정규화한 뒤에 합니다.
정규화가 없으면 mockup 6개가 각 19건씩 "불일치"로 잡히는데, 그건 값이 다른 게 아니라 다르게
적힌 것입니다.

| 파일 | 공유 토큰 | 값 불일치 | 처분 | 사유 |
|------|-----------|-----------|------|------|
| `docs/index.html` | 16 | 0 | DUP | 문서 포털 허브. 2026-09-03 (#51·#52)에 디자인 시스템을 적용하면서 정본 값을 인라인 복제했다. |
| `docs/journeys/JRN-idle-resume/index.html` | 17 | 0 | DUP | 여정 페이지. 2026-09-03 (#56) lifecycle 클러스터로 신설. `--frozen-soft`까지 복제해 공유 토큰이 하나 더 많다. |
| `docs/journeys/JRN-manual-freeze/index.html` | 17 | 0 | DUP | 여정 페이지. 위와 같다. |
| `docs/journeys/JRN-session-creation/index.html` | 16 | 0 | DUP | 여정 페이지. 2026-09-03 (#50) 신설 시 허브의 인라인 토큰을 복사했다. |
| `docs/journeys/JRN-session-deletion/index.html` | 17 | 0 | DUP | 여정 페이지. 위와 같다. |
| `docs/journeys/JRN-shell-interaction/index.html` | 16 | 0 | DUP | 여정 페이지. 2026-09-03 (#50) 신설 시 허브의 인라인 토큰을 복사했다. |
| `docs/mockups/agent-workspace.html` | 24 | 0 | DUP | mockup. 정본이 여기서 1:1 이식됐고 이후 방향이 뒤집혔다(코드가 정본). |
| `docs/mockups/gated-workspace.html` | 24 | 0 | DUP | mockup. `agent-workspace.html`의 인라인 토큰을 복사해 만들었다. |
| `docs/mockups/index.html` | 24 | 0 | DUP | mockup(세션 목록). 정본 `tokens.css`의 이식 원본이다. |
| `docs/mockups/new-session.html` | 24 | 0 | DUP | mockup. |
| `docs/mockups/restore.html` | 24 | 0 | DUP | mockup. |
| `docs/mockups/workspace.html` | 24 | 0 | DUP | mockup. |
| `docs/reader.html` | 2 | 2 | IND | 마크다운 리더 전용 페이지. 이 시스템과 무관한 자기 팔레트(`--bg`·`--fg`·`--accent`…)를 쓰며 `--line`(`#30363d`)·`--maxw`(`860px`)만 이름이 겹친다. 정본을 따라갈 이유가 없으므로 유지비에서 뺀다. |

> **범위 밖 — 강제하지 않는 제3 사본이 하나 있습니다.** `docs/doc-structure-state.md`도 같은
> 사실("토큰 7중복")을 말하지만 그 문서는 자매 정합성 모델 `tbm_session-platform-journey-mockup`의
> to-be이므로 여기서 고치지 않습니다(같은 파일을 두 모델이 동시에 고치면 충돌합니다).
> 그 사본의 갱신은 그 모델의 후속 task 몫입니다 — 잊힌 잔여가 아니라 **등재된 후속**입니다.

> **이 원장이 없애는 것은 중복 자체가 아니라 중복의 침묵입니다.** mockup을 `tokens.css`에
> 물릴지, 아니면 "mockup은 스케치이므로 복제를 수용"할지는 `docs/doc-structure-state.md:148`이
> **제품 결정 대기**로 둔 항목이라 게이트가 정할 수 없습니다. 결정 전까지 할 수 있는 최선은
> 복제가 늘거나 어긋날 때 **반드시 보이게** 만드는 것입니다.

---

## 이 시스템을 벗어나야 할 때

`.claude/skills/ui-with-design-system`이 UI 작업 시 이 규율을 강제합니다. 요약하면:

1. 임의 hex·px를 코드에 직접 박지 않습니다.
2. 필요한 항목이 여기 없으면 **코드에 박기 전에** `tokens.css` 또는 `shell.css`에 먼저 추가합니다.
3. 그래도 예외가 필요하면 사용자 동의를 받고 `// design-system-exception: <사유>` 주석을 답니다.

---

## 정본 이탈 원장

이 규율은 오랫동안 **선언만 있고 집행이 없었습니다** — `docs/doc-structure-state.md`가
"SPA의 정본 준수 여부 미검증(🟢)"으로 남겨둔 항목이 그것입니다. 아래는 그 항목을 실제로 측정해
**빠짐없이 등재한 원장**이고, `scripts/check-render-fidelity.py`(CI `render-fidelity` 잡)가
`web/src/screens` + `web/src/app` 스캔 결과와 **양방향 집합 동등성**으로 강제합니다.
등재되지 않은 이탈이 새로 들어오면 CI가 실패하고, 원장에만 있고 코드에 없는 행도 실패합니다.

처분은 셋 중 하나입니다:

| 처분 | 뜻 |
|------|-----|
| `DYN` | 런타임 값 바인딩. 정적 CSS로 표현할 수 없으므로 **이탈이 아닙니다.** |
| `EXC` | 수용된 예외. 코드에 `// design-system-exception: <사유>` 마커가 **반드시** 있어야 하며, 게이트가 마커와 이 원장을 양방향으로 대조합니다. 등재 시점 0건입니다. |
| `OPEN` | 미해소 위반. 제거 경로가 적혀 있어야 하고 **총량에 상한**이 걸립니다. |

미해소(OPEN) 상한 = **27**

상한은 **늘 수 없고**, 줄이면 이 숫자도 함께 내립니다(게이트가 양쪽 다 검사합니다).
즉 이탈은 한 방향으로만 — 줄어드는 쪽으로만 — 움직입니다.

| 파일 | 종류 | 값 | 건수 | 처분 |
|------|------|-----|------|------|
| `web/src/app/AppShell.tsx` | HEX | `#1c1404` | 1 | OPEN |
| `web/src/app/AppShell.tsx` | HEX | `#ffb43a` | 2 | OPEN |
| `web/src/app/SessionCard.tsx` | HEX | `#5a7686` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#0b1a10` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#101c23` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#16242c` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#163b46` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#22414e` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#2b6c7e` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#6f93a3` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#cdeaf3` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#ff9a2e` | 1 | OPEN |
| `web/src/app/shell.css` | HEX | `#ffc766` | 1 | OPEN |
| `web/src/app/SessionCard.tsx` | INLINE | `--p` | 1 | DYN |
| `web/src/app/SessionCard.tsx` | INLINE | `color` | 1 | OPEN |
| `web/src/app/SessionCard.tsx` | INLINE | `width` | 2 | OPEN |
| `web/src/screens/NewSession.tsx` | INLINE | `padding` | 1 | OPEN |
| `web/src/screens/Sessions.tsx` | INLINE | `background` | 3 | OPEN |
| `web/src/screens/Sessions.tsx` | INLINE | `boxShadow` | 2 | OPEN |
| `web/src/screens/Sessions.tsx` | INLINE | `color` | 1 | OPEN |
| `web/src/screens/Sessions.tsx` | INLINE | `display` | 1 | OPEN |
| `web/src/screens/Sessions.tsx` | INLINE | `fontSize` | 1 | OPEN |
| `web/src/screens/Sessions.tsx` | INLINE | `gap` | 1 | OPEN |

### 제거 경로 (OPEN 행)

**HEX — 값은 전부 mockup에서 1:1로 이식된 것이고, 어긋난 값이 아니라 `tokens.css`에 자리가 없어
코드에 박힌 값입니다.** 따라서 고치는 방향은 "값을 바꾼다"가 아니라 **"토큰을 신설하고 참조로
바꾼다"**입니다. 렌더 결과는 변하지 않아야 합니다.

- `#ffb43a` ×2 (`AppShell`) — `--active`와 **같은 값**입니다. SVG `fill=` 표현 속성은 `var()`를
  받지 않으므로 값 치환이 아니라 `shell.css`의 `.mark svg path` / `.mark svg circle` 규칙으로
  옮겨 `fill: var(--active)`로 참조해야 합니다.
- `#1c1404` (`AppShell`) — `--active` 위에 얹는 전경색. `--active-soft`(`#3a2b12`)와는 다른 값이라
  기존 토큰으로 대체되지 않습니다. `--on-active` 성격의 토큰 신설이 필요합니다.
- `#5a7686` (`SessionCard`, 같은 자리의 INLINE `color`와 한 쌍) — "pod reclaimed" 라벨 색.
  `--text-faint`(`#5e6e79`)와 가깝지만 같지 않습니다. 통합할지 토큰을 새로 둘지 결정이 필요합니다.
- `#ff9a2e` · `#ffc766` (`.bar i.active` 그라디언트) — `--active` 주변의 두 정점. 그라디언트 정점
  토큰(`--active-grad-from/to`)이 필요합니다.
- `#163b46` · `#2b6c7e` · `#cdeaf3` (`.rail-me` 아바타) — 배경 그라디언트 두 정점 + 전경.
- `#16242c` · `#101c23` · `#22414e` (`.card[data-state="snapshot"]`) — snapshot 카드의 그라디언트
  두 정점 + 테두리. `--frozen`/`--frozen-soft` 계열의 확장 후보입니다.
- `#0b1a10` (`.step.ok .ico`) — `--ok` 위 전경색. `--on-ok` 성격.

**INLINE — 값이 아니라 위치의 문제입니다.** 대부분 이미 토큰을 참조하고 있고
(`background: var(--active)` 등), 잘못된 것은 그 선언이 CSS가 아니라 JSX에 있다는 점입니다.

- `Sessions.tsx`의 `background` ×3 · `boxShadow` ×2 — 범례 점(`.dot`)의 상태별 스타일.
  `shell.css`에 `.dot.active` / `.dot.idle` / `.dot.frozen` 규칙을 두고 클래스만 붙이면 됩니다.
- `Sessions.tsx`의 `display` · `gap` — 툴바 flex 레이아웃. 클래스 하나로 이관.
- `Sessions.tsx`의 `fontSize` · `color` — reclaim 카운터 강조. 클래스 하나로 이관.
- `NewSession.tsx`의 `padding` — `.error`의 모달 내 변형. `.error.in-modal` 같은 변형 클래스로.
- `SessionCard.tsx`의 `width` ×2 — 값이 상수 `0`인 빈 게이지. `.bar i.empty { width: 0 }`로.
- `SessionCard.tsx`의 `color` — 위 `#5a7686`과 같은 자리.

### 범위 밖이 아니라 총량으로 묶은 것 — `rgb()`/`rgba()` 리터럴

`rgba(...)` 리터럴도 같은 성격의 이탈이지만 30건이 넘어 값 단위로 등재하면 원장이 읽히지
않습니다. 대신 **파일 단위 총량에 상한**을 걸어 늘어날 수 없게만 묶어 둡니다(줄이면 상한도
내립니다). 값 단위 분해는 후속 작업입니다.

| 파일 | rgba 상한 |
|------|-----------|
| `web/src/app/shell.css` | 30 |
| `web/src/screens/Sessions.tsx` | 1 |

특기할 1건: `Sessions.tsx`의 `rgba(127,205,234,.7)`은 `--frozen`(`#7fcdea` = `rgb(127,205,234)`)을
알파만 얹어 손으로 다시 쓴 것입니다 — 토큰이 있는데도 값이 복제된 자리라, 후속 분해 때
가장 먼저 접을 항목입니다.
