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

결과적으로 지금 토큰 하나를 바꾸려면 `tokens.css` + mockup 5개, 총 6곳을 고쳐야 합니다.
이 중복은 알려진 미해소 항목이며 `docs/doc-structure-state.md`의 🟢 위험으로 추적됩니다.

**당분간의 규칙**: 코드(`tokens.css`)를 먼저 고치고, mockup은 그 값을 따라옵니다. 반대 방향이 아닙니다.

---

## 이 시스템을 벗어나야 할 때

`.claude/skills/ui-with-design-system`이 UI 작업 시 이 규율을 강제합니다. 요약하면:

1. 임의 hex·px를 코드에 직접 박지 않습니다.
2. 필요한 항목이 여기 없으면 **코드에 박기 전에** `tokens.css` 또는 `shell.css`에 먼저 추가합니다.
3. 그래도 예외가 필요하면 사용자 동의를 받고 `// design-system-exception: <사유>` 주석을 답니다.
