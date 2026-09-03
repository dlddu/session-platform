# 여정 mockup — 페이지 규약 (`docs/journeys/`)

여정 하나 = 이 폴더의 페이지 하나. 경로는 **`docs/journeys/<journey-id>/index.html`** 이고
`<journey-id>` 는 `docs/user-journeys/JRN-<슬러그>.md` 의 여정 식별자와 글자 그대로 같다.

> **여정 ↔ 페이지 매핑의 단일 진실 원천은 이 문서가 아니라
> [`../mockups/README.md`](../mockups/README.md) 의 "여정 → 여정 mockup 페이지 매핑" 표**입니다.
> 이 문서는 **페이지가 지켜야 할 마크업 규약**만 정의합니다.
> 여정 정의는 [`../user-journeys/`](../user-journeys/), 예외 등재는
> [`../doc-structure-state.md`](../doc-structure-state.md) 의 "수용된 위험" 섹션입니다.

여기 적힌 규약은 전부 `tools/journey-prototype.test.mjs` 가 실제 브라우저에서 기계적으로 집행합니다.
규약을 바꾸려면 하네스도 같은 커밋에서 바꿔야 합니다.

---

## 0. 이 페이지들은 문서 뷰어가 아니라 제품 프로토타입이다

단계 메타를 늘어놓고 이전/다음으로 넘기는 화면은 이 폴더의 목적을 충족하지 않습니다.
**연 직후 보이는 것은 제품 화면**이어야 하고, 다음 단계로는 **그 화면 안의 행동**(버튼·폼 제출)으로
전진해야 합니다. 화면은 어딘가에서 복제해 온 스냅샷이 아니라 **그 자체가 원본**입니다 —
`../mockups/*.html` (화면 단위 mockup)의 마크업을 옮겨 적는 것은 괜찮지만, 바이트 동일하게
고정하지는 않습니다. 그렇게 하면 아래 4·5·6의 배선과 상태 변형을 넣을 길이 막힙니다.

---

## 1. 여정 선언

```html
<main data-journey="JRN-session-creation" data-step-active="STP-create-request">
```

- 페이지당 `[data-journey]` 는 **정확히 하나**. 값은 폴더 이름과 같아야 한다.
- `data-step-active` 는 현재 보이는 단계 식별자를 항상 반영한다(스크립트가 갱신).

## 2. 단계 선언

```html
<section class="step" data-step="STP-create-request" id="STP-create-request" hidden>
```

- 여정 문서 "3. 단계별 상세" 의 `### \`STP-…\`` 헤딩 집합과 **양방향으로 같아야** 한다.
- `id` 는 `data-step` 과 같은 값 — 딥링크(`#STP-…`)의 대상이다.
- 어느 시점에도 보이는 단계는 **정확히 하나**. 숨김은 CSS가 아니라 `hidden` 속성으로 한다
  (하네스가 스타일 없이도 판정할 수 있어야 한다).

## 3. 단계 메타는 접어 둔다

```html
<details class="step-meta">
  <summary>이 화면이 담는 여정 단계</summary>
  …단계 식별자 · 터치포인트 · 연결 AC · 행동 서술…
</details>
```

- `open` 속성을 달지 않는다. 문서 메타를 제품 화면과 같은 평면에 상시 노출하지 않기 위한 장치다.
- `<details>` 를 쓰는 이유: CSS 없이도 "기본으로 접혀 있다"가 기계적으로 확인되기 때문이다.

## 4. 전진은 화면 안의 행동으로

```html
<button class="btn primary" data-advance-to="STP-workspace-entry">Create session</button>
```

- 마지막 단계를 뺀 모든 단계는 `[data-advance-to]` 를 **그 단계의 화면 안에** 가진다.
- 래퍼 네비게이션(단계 목록·이전/다음 바)으로만 넘어가는 단계가 하나라도 있으면 규약 위반이다.
- 마지막 단계는 대신 `[data-journey-end]` 를 갖는다(여정의 끝을 화면에 표현).

## 5. 실제 입력 요소

- 텍스트·선택·토글은 진짜 `<input>` / `<select>` / `<textarea>` 여야 하고 포커스·타이핑·선택이 동작한다.
- 입력이 있는 단계는 그 입력에 `data-required-input` 을 달아 하네스가 반드시 굴리게 한다.
- 입력처럼 보이도록 스타일링한 비대화형 요소로 대체하지 않는다.

## 6. 분기·예외

여정 문서 "4. 분기·예외 흐름" 표의 **모든 행**이 페이지에 배선돼 있어야 한다.

```html
<button data-branch-row="3"
        data-branch-target="JRN-idle-resume#STP-auto-freeze"
        data-branch-pending>유휴 한계로 자동 동결됨</button>
```

- `data-branch-row` — 표에서의 행 번호(1부터). 행 수와 요소 수가 같아야 한다.
- `data-branch-target` — 대상. 네 형태만 허용한다.
  | 표의 "이어지는 단계" | `data-branch-target` |
  |---|---|
  | `STP-x` (같은 여정) | `STP-x` |
  | `JRN-y`의 `STP-x` | `JRN-y#STP-x` |
  | `JRN-y` (단계 없음) | `JRN-y` |
  | 해당 없음 | `none` |
- 기본 화면에서 보이지 않는 분기는 `data-branch-open="<그것을 드러내는 컨트롤의 id>"` 를 함께 단다.
  하네스는 그 컨트롤을 먼저 누른 뒤 분기 요소가 보이는지 확인한다.
- **같은 여정 안의 대상**은 누르면 실제로 그 단계로 이동해야 한다(`data-step-active` 가 바뀐다).
- **다른 여정의 대상**은 그 여정의 페이지가 이미 있으면
  `<a href="../JRN-y/index.html#STP-x">` 로 **승급**한다. 경로에 `index.html` 을 적는다 —
  `../JRN-y/` 처럼 디렉터리로 두면 `file://` 에는 디렉터리 인덱스가 없어 규약 8의
  "빌드·네트워크 없이 `file://` 로 열린다"가 깨진다(Pages 에서만 동작하는 링크가 된다).
  하네스는 그 링크를 실제로 눌러 **대상 페이지가 열리고 그 페이지의 `data-step-active` 가
  `STP-x` 인지**까지 확인한다 — 즉 `data-branch-pending` 을 걷는 것은 "링크가 살아 있다"는
  주장이고, 그 주장은 페이지 간 딥링크로 검증된다.
- 아직 페이지가 없으면 `data-branch-pending` 을 달아 **그 사실을 명시**한다 — 이때 누르면
  페이지 안에서 `[data-state-note]` 하나가 보이며 "이 갈래는 아직 페이지가 없다"를 알린다.
  대상 페이지가 생기는 순간 `data-branch-pending` 은 하네스에서 **실패**가 된다(승급해야 한다).
- **예외 등재(⚪) 여정을 향한 갈래**는 페이지가 앞으로도 생기지 않는다. `data-branch-pending` 에
  더해 `data-branch-excepted` 를 달아 *"안 만든 것"이 아니라 "그리지 않기로 한 것"* 임을 구분한다.
  하네스는 (i) 이 표시가 붙은 대상이 실제로 `../doc-structure-state.md` 에 등재된 여정인지,
  (ii) 표시 **없는** pending 의 대상이 매핑 표에서 `⏳ 예정` 인지를 함께 확인한다.
  허브(`../index.html`)가 이미 두 상태를 구분해 표기하는 것과 같은 구분을 페이지 층위에도 둔다.
- `none` 대상도 마찬가지로 `[data-state-note]` 로 결과를 표현한다.

## 7. 딥링크

`file://…/docs/journeys/<journey-id>/index.html#STP-x` 로 열면 그 단계가 바로 활성이어야 한다.

## 8. 정적 동작

- 빌드·번들·네트워크 없이 `file://` 로 열린다.
- `<script type="module">` 금지(파일 스킴에서 CORS로 막힌다). 인라인 클래식 스크립트를 쓴다.
- `fetch` / `XMLHttpRequest` / `import(` 금지.
- 웹폰트 같은 **장식용** 외부 자원은 허용하되, 없어도 모든 단계가 동작해야 한다.
  (하네스는 폰트 CDN을 제외한 모든 외부 요청을 차단한 채로 페이지를 굴린다.)

## 9. 식별자 위생

- 존재하지 않는 여정·단계 식별자를 참조하지 않는다.
- 2026-08-30 에 폐기된 순번 식별자(`J1`~`J6`, `J1-S1` 계열)를 새 대상에 재사용하지 않는다.

---

## 페이지를 새로 만들 때

1. `../user-journeys/JRN-<슬러그>.md` 의 단계 헤딩과 분기표를 그대로 읽어 온다.
   (단계 헤딩은 `### \`STP-…\`` 처럼 백틱 코드스팬 안에 있다 — `^### STP-` 로 grep 하면 0건이 나온다.)
2. 위 1~9 를 지켜 `<journey-id>/index.html` 을 만든다.
3. `../mockups/README.md` 의 매핑 표에서 그 여정 행을 `⏳ 예정` → `✅` 로 올리고 공개 경로를 적는다.
4. `../index.html` 허브의 그 여정 행 링크를 `journeys/<journey-id>/` 로 바꾼다.
5. 다른 여정 페이지가 이 여정을 `data-branch-pending` 으로 걸어 두었으면 실제 링크로 승급한다
   (하네스가 강제한다).
6. CI 워크플로 `docs-journey-mockup` 이 초록인지 확인한다.
