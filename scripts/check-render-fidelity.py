#!/usr/bin/env python3
"""목업 ↔ 구현 렌더링 정합성 게이트 (화면 매핑 · 정본 이탈 · 패널 구조).

이 레포의 시각 SSOT는 `docs/mockups`이고 **디자인 토큰만은 코드**(`web/src/design/tokens.css`)가
정본이다. 그 두 방향을 문서 낭독이 아니라 **집합 동등성**으로 강제한다:

  R1   `docs/mockups/README.md`에 화면 매핑 표가 존재하고 서식이 맞다.
  R2   매핑 표가 가리키는 구현 파일·mockup 파일이 실재한다.
  R3   스캔한 구현 파일 집합 == 매핑 표의 행 집합. 양방향 완전 동등(고아·누락 없음).
  R4   각 구현 파일 헤더의 `mockup:` 선언 == 매핑 표의 mockup 집합. 양방향 동등.
  R5   `docs/mockups/*.html` 전부가 표에 등장하고, `구현 없음`으로 등재된 mockup은
       선언한 **구현 신호**가 실제로 0 hit이어야 한다(구현이 착지하면 게이트가 갱신을 강제한다).
  R6   `web/src/screens`+`web/src/app`의 하드코딩 hex·인라인 style 스캔 == 정본 이탈 원장
       (`web/src/design/README.md`). 양방향 완전 동등.
  R7   코드의 `design-system-exception:` 마커는 원장의 `EXC` 행만 쓰고, 마커 파일 집합이
       `EXC` 행의 파일 집합과 정확히 같다.
  R8   패널(`.panel` > `<h4>`) 차집합 실측 == 패널 구조 원장. 그리고 1:N 화면은
       **쌍별 선언의 합집합 == 그 파일에서 추출한 패널 전체 집합**이어야 한다(조용한 누락 방지).
  R9   집계·상한 줄의 숫자가 실제와 일치하고, 미해소(OPEN) 건수와 rgba 건수가 선언한 상한을
       넘지 않는다(늘면 실패, 줄이면 상한도 내린다).
  R10  **토큰 복제 원장** — `tokens.css` `:root`의 정본 토큰을 재선언하는 파일 집합을 레포
       전수로 실측해 `web/src/design/README.md`의 원장과 양방향 동등 비교한다. `DUP`(정본
       팔레트 복제) 행은 값 불일치가 0이어야 하고, `IND`(독립 팔레트) 행은 사유가 있어야 한다.
       "토큰 1개 변경 시 최소 N곳 · 최대 M곳" 문구는 게이트가 DUP 행만으로 계산한 값과
       일치해야 하며, 그 문구를 담은 두 문서를 함께 강제한다.

       ★ 이 규칙은 원래 `1 + docs/mockups/*.html 개수`라는 **하드코딩 공식 + 손으로 쓴 스칼라**
       였다. 그 형태는 두 가지로 틀렸다. ① mockup 밖의 복제(`docs/index.html` ·
       `docs/journeys/*/index.html`)를 세지 않아, 복제가 늘어도 공식은 그대로였다 — 그래서
       **참값을 적으면 CI가 거부하는** 상태가 됐다. ② 애초에 "몇 곳"은 **토큰마다 다르다**
       (여기서는 7~10). 어떤 스칼라를 넣어도 어떤 토큰에 대해서는 거짓이다. 그래서 숫자를
       고치는 대신 **측정을 코드로 옮겨** 정의상 참일 수밖에 없게 만든다.

경계: **e2e 충실도**(테스트가 무엇을 치환하는가)는 자매 게이트 `check-fidelity-allowlist.py`와
`docs/test/e2e.md`가 소유한다. 여기는 **렌더링 정합성**(화면이 목업과 같은가)만 본다. 두 원장을
섞지 않는다. 정합성 모델은 각각 `tbm_session-platform-mockup-render`와
`tbm_session-platform-e2e-mock-policy`다.

표준 라이브러리만 쓰고, 파일 목록은 `git ls-files`로 얻는다. 툴체인이 필요 없다.

사용법:
    python3 scripts/check-render-fidelity.py            # 검사 (CI)
    python3 scripts/check-render-fidelity.py --emit     # 실측을 원장 서식으로 출력 (원장 갱신용)
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MAP_DOC = ROOT / "docs" / "mockups" / "README.md"
LEDGER_DOC = ROOT / "web" / "src" / "design" / "README.md"
MOCKUP_DIR = ROOT / "docs" / "mockups"
TOKENS_CSS = ROOT / "web" / "src" / "design" / "tokens.css"
TOKENS_CSS_REL = "web/src/design/tokens.css"

# --- 스캔 스코프: 정합성 모델 tbm_session-platform-mockup-render 의 as-is 와 같게 유지한다.
#     (web/src/design 은 *정본*이므로 이탈 스캔 대상이 아니다 — 매핑·이탈의 기준점이다.)
SCAN_DIRS = ["web/src/screens", "web/src/app"]

HEX_RE = re.compile(r"#[0-9a-fA-F]{3,8}\b")
RGBA_RE = re.compile(r"rgba?\(\s*[0-9]")
DECL_RE = re.compile(r"mockup:\s*(?P<val>[^\n*]+)")
EXC_MARKER_RE = re.compile(r"design-system-exception:\s*(?P<reason>[^\n*]+)")

errors: list[str] = []


def fail(rule: str, msg: str) -> None:
    errors.append(f"[{rule}] {msg}")


def git_files(*pathspecs: str) -> list[str]:
    out = subprocess.run(
        ["git", "-C", str(ROOT), "ls-files", "-z", *pathspecs],
        check=True, capture_output=True, text=True,
    ).stdout
    return sorted(p for p in out.split("\0") if p)


# --------------------------------------------------------------------------
# 스캐너
# --------------------------------------------------------------------------

def scan_hex(files: list[str]) -> dict[tuple[str, str], int]:
    """(파일, 정규화된 hex) -> 건수. 대소문자는 소문자로 정규화한다."""
    hits: dict[tuple[str, str], int] = {}
    for f in files:
        for m in HEX_RE.finditer((ROOT / f).read_text(encoding="utf-8")):
            key = (f, m.group(0).lower())
            hits[key] = hits.get(key, 0) + 1
    return hits


def scan_rgba(files: list[str]) -> dict[str, int]:
    """파일 -> rgb()/rgba() 리터럴 건수. 값 단위가 아니라 파일 단위 총량만 상한으로 묶는다."""
    hits: dict[str, int] = {}
    for f in files:
        n = len(RGBA_RE.findall((ROOT / f).read_text(encoding="utf-8")))
        if n:
            hits[f] = n
    return hits


def _style_props(src: str) -> list[str]:
    """`style={{ ... }}` 안의 **최상위** 속성 키를 순서대로 뽑는다.

    중괄호 깊이를 세어 중첩 객체 안의 키를 최상위로 오인하지 않는다.
    계산된 키(`["--p" as string]`)는 그 문자열 리터럴로 정규화한다.
    """
    props: list[str] = []
    i = 0
    while True:
        start = src.find("style={{", i)
        if start < 0:
            return props
        j = start + len("style={{")
        depth = 1  # `{{` 의 안쪽 중괄호
        body_start = j
        while j < len(src) and depth > 0:
            if src[j] == "{":
                depth += 1
            elif src[j] == "}":
                depth -= 1
            j += 1
        body = src[body_start:j - 1]
        # 최상위 키만: 깊이 0 인 위치의 `key:` 또는 `["--x" ...]:`
        depth = 0
        k = 0
        token = ""
        while k < len(body):
            c = body[k]
            if c in "{(":
                depth += 1
            elif c in "})":
                depth -= 1
            elif c == "[" and depth == 0:
                close = body.find("]", k)
                lit = re.search(r'"([^"]+)"', body[k:close + 1] if close > 0 else "")
                if lit and close > 0 and body[close + 1:close + 2] == ":":
                    props.append(lit.group(1))
                    k = close + 1
                    token = ""
                    k += 1
                    continue
            elif c == ":" and depth == 0:
                name = token.strip().strip(",").strip()
                if re.fullmatch(r"[A-Za-z][A-Za-z0-9]*", name):
                    props.append(name)
                token = ""
                k += 1
                continue
            elif c == "," and depth == 0:
                token = ""
                k += 1
                continue
            token += c
            k += 1
        i = j
    return props


def scan_inline(files: list[str]) -> dict[tuple[str, str], int]:
    """(파일, style 속성명) -> 건수."""
    hits: dict[tuple[str, str], int] = {}
    for f in files:
        if not f.endswith((".tsx", ".ts")):
            continue
        for p in _style_props((ROOT / f).read_text(encoding="utf-8")):
            key = (f, p)
            hits[key] = hits.get(key, 0) + 1
    return hits


ROOT_BLOCK_RE = re.compile(r":root\s*\{(.*?)\}", re.S)
CSS_VAR_RE = re.compile(r"--([a-z0-9-]+)\s*:\s*([^;]+);")


def norm_value(v: str) -> str:
    """CSS 값을 표기 차이만 지운 형태로 정규화한다.

    같은 값이 다르게 **적힌** 것을 불일치로 오인하지 않기 위한 것이고, 값 자체는 바꾸지 않는다.
    실제로 이 레포에 있는 두 가지 표기 차이를 흡수한다:
      - hex 대소문자 — mockup 은 `#0B1116`, 정본은 `#0b1116`
      - 선행 0 과 공백 — mockup 은 `cubic-bezier(.22,.61,.36,1)`,
        정본은 `cubic-bezier(0.22, 0.61, 0.36, 1)`
    """
    s = re.sub(r"\s+", "", v.strip().lower())
    return re.sub(r"(?<![0-9a-z])\.(?=[0-9])", "0.", s)


def canonical_tokens() -> dict[str, str]:
    """정본 `tokens.css` 의 `:root` 토큰 이름 -> 정규화된 값."""
    m = ROOT_BLOCK_RE.search(TOKENS_CSS.read_text(encoding="utf-8"))
    if not m:
        return {}
    return {n: norm_value(v) for n, v in CSS_VAR_RE.findall(m.group(1))}


def scan_token_dupes(canon: dict[str, str]) -> dict[str, dict[str, str]]:
    """정본 토큰을 재선언하는 파일 -> {토큰: 정규화된 값}.

    스코프는 레포 전수의 tracked `*.html`/`*.css` 다(정본 자신은 제외). **모델의 as-is 스코프보다
    넓은 것이 의도**다 — 복제는 as-is 밖(`docs/index.html` · `docs/journeys/`)에서 늘어났고,
    좁은 스코프가 바로 이 규칙이 참값을 거부하게 된 원인이었다. 여기서 세는 것은 "정합성 위반"이
    아니라 **"정본을 바꿀 때 함께 고쳐야 하는 자리"**이므로 위치를 가리지 않는다.
    """
    found: dict[str, dict[str, str]] = {}
    for f in git_files("*.html", "*.css"):
        if f == TOKENS_CSS_REL:
            continue
        decls: dict[str, str] = {}
        for block in ROOT_BLOCK_RE.findall((ROOT / f).read_text(encoding="utf-8")):
            for name, val in CSS_VAR_RE.findall(block):
                if name in canon:
                    decls[name] = norm_value(val)
        if decls:
            found[f] = decls
    return found


def dupe_row(f: str, decls: dict[str, str], canon: dict[str, str]) -> tuple[int, int]:
    """(공유 토큰 수, 값 불일치 수)."""
    return len(decls), sum(1 for n, v in decls.items() if canon[n] != v)


def dupe_cost(dup_files: dict[str, dict[str, str]], canon: dict[str, str]) -> tuple[int, int]:
    """DUP 파일만으로 계산한 (최소 곳수, 최대 곳수).

    "토큰 1개를 바꾸면 몇 곳을 고쳐야 하는가"는 **토큰마다 다르다** — 그래서 하나의 스칼라가
    아니라 범위로 말한다. `+1` 은 정본 `tokens.css` 자신이다. 어느 정본 토큰도 복제하지 않는
    상태(=목표)면 (1, 1) 이 된다.
    """
    per_token = [
        sum(1 for decls in dup_files.values() if name in decls)
        for name in canon
    ]
    if not per_token:
        return 1, 1
    return 1 + min(per_token), 1 + max(per_token)


def scan_exception_markers(files: list[str]) -> set[str]:
    return {
        f for f in files
        if EXC_MARKER_RE.search((ROOT / f).read_text(encoding="utf-8"))
    }


def _strip_tags(html: str) -> str:
    return " ".join(re.sub(r"<[^>]+>", " ", html).split())


def mockup_panels(path: Path) -> list[str]:
    """mockup HTML 의 `<div class="panel">` 안 `<h4>` 제목을 문서 순서대로."""
    src = path.read_text(encoding="utf-8")
    titles: list[str] = []
    for m in re.finditer(r'<div class="panel[^"]*"[^>]*>', src):
        rest = src[m.end():]
        nxt = re.search(r'<div class="panel[^"]*"[^>]*>', rest)
        seg = rest[: nxt.start()] if nxt else rest
        h = re.search(r"<h4[^>]*>(.*?)</h4>", seg, re.S)
        if h:
            titles.append(_strip_tags(h.group(1)))
    return titles


def impl_panels(path: Path) -> list[str]:
    """구현 TSX 의 `className=...panel...` 직후 `<h4>` 제목을 문서 순서대로."""
    src = path.read_text(encoding="utf-8")
    titles: list[str] = []
    for m in re.finditer(r'className=(?:"|\{")([^"]*)"', src):
        classes = m.group(1).split()
        if "panel" not in classes:
            continue
        h = re.search(r"<h4[^>]*>(.*?)</h4>", src[m.end():], re.S)
        if h:
            titles.append(_strip_tags(h.group(1)))
    return titles


# --------------------------------------------------------------------------
# 문서 파서 (표 = | a | b | ... | 형태)
# --------------------------------------------------------------------------

def section(doc: str, heading: str) -> str:
    """`## <heading>` 부터 다음 같은 수준 헤딩 전까지."""
    m = re.search(rf"^##\s+{re.escape(heading)}\s*$", doc, re.M)
    if not m:
        return ""
    rest = doc[m.end():]
    nxt = re.search(r"^##\s+", rest, re.M)
    return rest[: nxt.start()] if nxt else rest


def _is_sep(cells: list[str]) -> bool:
    return bool(cells) and all(set(c) <= set("-:") and c for c in cells)


def rows(block: str, ncols: int) -> list[list[str]]:
    """열 수가 `ncols`인 표들의 **본문** 행만 셀 리스트로 돌려준다.

    연속한 `|…|` 줄을 하나의 표로 묶고, 구분선(`|---|`) 바로 앞 줄을 헤더로 보아 함께 버린다.
    헤더를 걸러내지 않으면 헤더 텍스트가 데이터 행으로 새어 들어온다.
    """
    out: list[list[str]] = []
    table: list[list[str]] = []

    def flush() -> None:
        if len(table) >= 2 and _is_sep(table[1]):
            body = table[2:]
        else:
            body = [r for r in table if not _is_sep(r)]
        out.extend(r for r in body if len(r) == ncols and not _is_sep(r))
        table.clear()

    for line in block.splitlines():
        line = line.strip()
        if line.startswith("|") and line.endswith("|") and len(line) > 1:
            table.append([c.strip() for c in line[1:-1].split("|")])
        else:
            flush()
    flush()
    return out


def code_cell(cell: str) -> str:
    """``` `web/src/x.tsx` ``` -> web/src/x.tsx (여러 개면 콤마 분리 후 리스트는 호출부에서)."""
    return cell.replace("`", "").strip()


def code_list(cell: str) -> list[str]:
    if code_cell(cell) in ("—", "-", ""):
        return []
    return [x.strip() for x in code_cell(cell).split(",") if x.strip()]


def declared_int(doc: str, label: str) -> int | None:
    m = re.search(rf"{re.escape(label)}\s*=\s*\*\*(\d+)\*\*", doc)
    return int(m.group(1)) if m else None


# --------------------------------------------------------------------------
# 검사
# --------------------------------------------------------------------------

def main() -> int:
    emit = "--emit" in sys.argv

    scan_files = git_files(*SCAN_DIRS)
    impl_files = [f for f in scan_files if f.endswith((".tsx", ".css"))]
    mockups = sorted(p.name for p in MOCKUP_DIR.glob("*.html"))

    hexes = scan_hex(scan_files)
    inlines = scan_inline(scan_files)
    rgbas = scan_rgba(scan_files)
    markers = scan_exception_markers(scan_files)

    canon = canonical_tokens()
    token_dupes = scan_token_dupes(canon)

    if emit:
        print("### 화면 매핑 후보 (구현 파일)")
        for f in impl_files:
            src = (ROOT / f).read_text(encoding="utf-8")
            d = DECL_RE.search(src)
            print(f"| `{f}` | {'선언 있음: ' + d.group('val').strip() if d else '선언 없음'} |")
        print("\n### 정본 이탈 실측 — HEX")
        for (f, v), n in sorted(hexes.items()):
            print(f"| `{f}` | HEX | `{v}` | {n} |")
        print("\n### 정본 이탈 실측 — INLINE")
        for (f, p), n in sorted(inlines.items()):
            print(f"| `{f}` | INLINE | `{p}` | {n} |")
        print("\n### 정본 이탈 실측 — RGBA (파일 단위 총량)")
        for f, n in sorted(rgbas.items()):
            print(f"| `{f}` | {n} |")
        print("\n### 패널 실측 — mockup")
        for m in mockups:
            print(f"| `{m}` | {' · '.join(mockup_panels(MOCKUP_DIR / m)) or '—'} |")
        print("\n### 패널 실측 — 구현")
        for f in impl_files:
            ps = impl_panels(ROOT / f)
            if ps:
                print(f"| `{f}` | {' · '.join(ps)} |")
        print(f"\n### 마커 있는 파일: {sorted(markers) or '—'}")
        print(f"\n### 토큰 복제 실측 (정본 토큰 {len(canon)}개 기준) — 처분은 손으로 정한다")
        for f in sorted(token_dupes):
            s, m_ = dupe_row(f, token_dupes[f], canon)
            print(f"| `{f}` | {s} | {m_} | {'IND' if m_ else 'DUP'} | … |")
        lo, hi = dupe_cost(token_dupes, canon)
        print(f"\n### 유지비 (전부 DUP 로 가정): 최소 **{lo}**곳 · 최대 **{hi}**곳")
        return 0

    map_doc = MAP_DOC.read_text(encoding="utf-8")
    ledger_doc = LEDGER_DOC.read_text(encoding="utf-8")

    # ---- R1 -------------------------------------------------------------
    map_sec = section(map_doc, "화면 ↔ mockup 매핑 (SPA 구현 대조)")
    panel_sec = section(map_doc, "패널 구조 차집합 (구현 대조)")
    if not map_sec:
        fail("R1", "docs/mockups/README.md 에 '## 화면 ↔ mockup 매핑 (SPA 구현 대조)' 섹션이 없다")
    if not panel_sec:
        fail("R1", "docs/mockups/README.md 에 '## 패널 구조 차집합 (구현 대조)' 섹션이 없다")
    if not map_sec or not panel_sec:
        return report()

    map_rows = rows(map_sec, 5)
    if not map_rows:
        fail("R1", "화면 매핑 표에 행이 없다(열 5개: 구현 파일 | 종류 | 대응 mockup | 근거 | 상태)")
        return report()

    # ---- R2/R3 ----------------------------------------------------------
    declared_impl: dict[str, list[str]] = {}
    # 구현이 없는 mockup 은 별도 표(3열: mockup | 구현 신호 | 비고)로 등재한다.
    unimplemented: dict[str, str] = {}
    for cells in rows(map_sec, 3):
        unimplemented[code_cell(cells[0])] = code_cell(cells[1])

    for cells in map_rows:
        target, kind, mock_cell, _basis, status = cells
        name = code_cell(target)
        if not (ROOT / name).exists():
            fail("R2", f"매핑 표의 구현 파일이 실재하지 않는다: {name}")
            continue
        mocks = code_list(mock_cell)
        for mk in mocks:
            if not (ROOT / mk).exists():
                fail("R2", f"매핑 표가 가리키는 mockup 이 실재하지 않는다: {mk} (행: {name})")
        if name in declared_impl:
            fail("R3", f"매핑 표에 같은 구현 파일이 두 번 등재됐다: {name}")
        declared_impl[name] = mocks
        if not mocks and "없음" not in status:
            fail("R1", f"대응 mockup 이 비어 있는데 상태가 '없음'을 말하지 않는다: {name} (상태: {status})")

    scanned = set(impl_files)
    declared = set(declared_impl)
    for miss in sorted(scanned - declared):
        fail("R3", f"구현 파일이 매핑 표에 없다(누락): {miss}")
    for orphan in sorted(declared - scanned):
        fail("R3", f"매핑 표에만 있고 스캔 스코프에 없다(고아): {orphan}")

    # ---- R4 -------------------------------------------------------------
    for f in sorted(scanned & declared):
        src = (ROOT / f).read_text(encoding="utf-8")
        d = DECL_RE.search(src)
        if not d:
            fail("R4", f"헤더에 `mockup:` 선언 주석이 없다: {f}")
            continue
        val = d.group("val").strip().rstrip("*/").strip()
        if val.startswith("none"):
            head: list[str] = []
        else:
            head = [x.strip().strip("`") for x in val.split(",") if x.strip()]
        table = declared_impl[f]
        if head != table:
            fail("R4", f"헤더 선언과 매핑 표가 다르다: {f} / 헤더={head or '없음'} 표={table or '없음'}")

    # ---- R5 -------------------------------------------------------------
    mapped_mockups = {mk for v in declared_impl.values() for mk in v}
    for m in mockups:
        rel = f"docs/mockups/{m}"
        if rel not in mapped_mockups and rel not in unimplemented:
            fail("R5", f"mockup 이 매핑 표 어디에도 없다: {rel}")
    for rel, signal in sorted(unimplemented.items()):
        if rel in mapped_mockups:
            fail("R5", f"'구현 없음'으로 등재된 mockup 이 동시에 화면에 매핑돼 있다: {rel}")
        if not signal:
            fail("R5", f"'구현 없음' 행에 구현 신호가 비어 있다: {rel}")
            continue
        hit = subprocess.run(
            ["git", "-C", str(ROOT), "grep", "-l", "-F", "--", signal, "--", *SCAN_DIRS],
            capture_output=True, text=True,
        )
        if hit.stdout.strip():
            files = ", ".join(sorted(hit.stdout.split()))
            fail("R5", f"'구현 없음'으로 등재된 {rel} 의 구현 신호 '{signal}' 가 이제 코드에 있다 "
                       f"({files}) — 매핑 표를 갱신하라")

    # ---- R6/R7 ----------------------------------------------------------
    dev_sec = section(ledger_doc, "정본 이탈 원장")
    if not dev_sec:
        fail("R6", "web/src/design/README.md 에 '## 정본 이탈 원장' 섹션이 없다")
        return report()

    ledger_rows = rows(dev_sec, 5)
    ledger_hex: dict[tuple[str, str], int] = {}
    ledger_inline: dict[tuple[str, str], int] = {}
    exc_files: set[str] = set()
    open_count = 0
    for cells in ledger_rows:
        f_cell, kind, val, cnt_cell, disp = cells
        f = code_cell(f_cell)
        v = code_cell(val)
        if kind not in ("HEX", "INLINE"):
            continue
        try:
            n = int(cnt_cell)
        except ValueError:
            fail("R6", f"원장 행의 건수가 정수가 아니다: {f} {kind} {v} -> {cnt_cell!r}")
            continue
        if disp not in ("DYN", "EXC", "OPEN"):
            fail("R6", f"원장 행의 처분이 DYN/EXC/OPEN 중 하나가 아니다: {f} {kind} {v} -> {disp!r}")
            continue
        (ledger_hex if kind == "HEX" else ledger_inline)[(f, v)] = n
        if disp == "EXC":
            exc_files.add(f)
        if disp == "OPEN":
            open_count += n

    for label, scanned_map, ledger_map in (
        ("HEX", hexes, ledger_hex),
        ("INLINE", inlines, ledger_inline),
    ):
        for key in sorted(set(scanned_map) - set(ledger_map)):
            fail("R6", f"{label} 이탈이 원장에 없다: {key[0]} `{key[1]}` ({scanned_map[key]}건)")
        for key in sorted(set(ledger_map) - set(scanned_map)):
            fail("R6", f"{label} 원장 행이 코드에 없다(고아): {key[0]} `{key[1]}`")
        for key in sorted(set(scanned_map) & set(ledger_map)):
            if scanned_map[key] != ledger_map[key]:
                fail("R6", f"{label} 건수 불일치: {key[0]} `{key[1]}` 원장={ledger_map[key]} 실제={scanned_map[key]}")

    for f in sorted(markers - exc_files):
        fail("R7", f"`design-system-exception:` 마커가 있는데 원장에 EXC 행이 없다: {f}")
    for f in sorted(exc_files - markers):
        fail("R7", f"원장에 EXC 행이 있는데 코드에 `design-system-exception:` 마커가 없다: {f}")

    # rgba 원장 (파일 단위 총량)
    rgba_rows = rows(section(ledger_doc, "정본 이탈 원장"), 2)
    ledger_rgba = {}
    for cells in rgba_rows:
        f = code_cell(cells[0])
        if not f.startswith("web/"):
            continue
        try:
            ledger_rgba[f] = int(cells[1])
        except ValueError:
            fail("R6", f"rgba 원장 행의 건수가 정수가 아니다: {f} -> {cells[1]!r}")
    for f in sorted(set(rgbas) - set(ledger_rgba)):
        fail("R6", f"rgba 리터럴이 원장에 없다: {f} ({rgbas[f]}건)")
    for f in sorted(set(ledger_rgba) - set(rgbas)):
        fail("R6", f"rgba 원장 행이 코드에 없다(고아): {f}")
    for f in sorted(set(rgbas) & set(ledger_rgba)):
        if rgbas[f] > ledger_rgba[f]:
            fail("R9", f"rgba 상한 초과: {f} 상한={ledger_rgba[f]} 실제={rgbas[f]}")
        elif rgbas[f] < ledger_rgba[f]:
            fail("R9", f"rgba 가 줄었으면 상한도 내린다: {f} 상한={ledger_rgba[f]} 실제={rgbas[f]}")

    # ---- R8 -------------------------------------------------------------
    panel_rows = rows(panel_sec, 5)
    per_file_declared: dict[str, set[str]] = {}
    gap_total = 0
    seen_pairs: set[tuple[str, str]] = set()
    for cells in panel_rows:
        f_cell, mk_cell, impl_cell, mo_cell, io_cell = cells
        f, mk = code_cell(f_cell), code_cell(mk_cell)
        seen_pairs.add((f, mk))
        impl_set = set(x.strip() for x in code_cell(impl_cell).split("·") if x.strip() and x.strip() != "—")
        mo_declared = set(x.strip() for x in code_cell(mo_cell).split("·") if x.strip() and x.strip() != "—")
        io_declared = set(x.strip() for x in code_cell(io_cell).split("·") if x.strip() and x.strip() != "—")
        per_file_declared.setdefault(f, set()).update(impl_set)
        if not (ROOT / mk).exists():
            fail("R8", f"패널 원장이 가리키는 mockup 이 없다: {mk}")
            continue
        actual_mock = set(mockup_panels(ROOT / mk))
        actual_impl_all = set(impl_panels(ROOT / f)) if (ROOT / f).exists() else set()
        if not impl_set <= actual_impl_all:
            fail("R8", f"선언한 구현 패널이 파일에 없다: {f} ↔ {mk} / 선언={sorted(impl_set)} 실제={sorted(actual_impl_all)}")
        mo_actual = actual_mock - impl_set
        io_actual = impl_set - actual_mock
        if mo_actual != mo_declared:
            fail("R8", f"mockup-only 차집합 불일치: {f} ↔ {mk} 원장={sorted(mo_declared)} 실제={sorted(mo_actual)}")
        if io_actual != io_declared:
            fail("R8", f"impl-only 차집합 불일치: {f} ↔ {mk} 원장={sorted(io_declared)} 실제={sorted(io_actual)}")
        gap_total += len(mo_actual) + len(io_actual)

    # 완성도: 어느 한쪽이라도 패널을 가진 (구현, mockup) 쌍은 반드시 원장에 행이 있어야 한다.
    # (구현 쪽 패널이 0개여도 mockup 쪽에 패널이 있으면 그 전량이 mockup-only 차집합이다 —
    #  이 조건을 빼면 Restore.tsx ↔ restore.html 같은 쌍이 조용히 세어지지 않는다.)
    required_pairs: set[tuple[str, str]] = set()
    for f, mocks in sorted(declared_impl.items()):
        has_impl = bool(impl_panels(ROOT / f))
        for mk in mocks:
            if has_impl or mockup_panels(ROOT / mk):
                required_pairs.add((f, mk))
    for pair in sorted(required_pairs - seen_pairs):
        fail("R8", f"패널을 가진 쌍이 원장에 없다: {pair[0]} ↔ {pair[1]}")
    for pair in sorted(seen_pairs - required_pairs):
        if pair[1] not in declared_impl.get(pair[0], []):
            fail("R8", f"패널 원장의 쌍이 매핑 표에 없다: {pair[0]} ↔ {pair[1]}")

    for f in sorted({p[0] for p in seen_pairs}):
        actual_impl_all = set(impl_panels(ROOT / f)) if (ROOT / f).exists() else set()
        union = per_file_declared.get(f, set())
        if union != actual_impl_all:
            fail("R8", f"쌍별 선언의 합집합이 파일의 패널 전체와 다르다: {f} "
                       f"합집합={sorted(union)} 실제={sorted(actual_impl_all)}")

    # ---- R9 -------------------------------------------------------------
    declared_open = declared_int(ledger_doc, "미해소(OPEN) 상한")
    if declared_open is None:
        fail("R9", "web/src/design/README.md 에 '미해소(OPEN) 상한 = **N**' 줄이 없다")
    elif open_count > declared_open:
        fail("R9", f"미해소 이탈이 상한을 넘었다: 상한={declared_open} 실제={open_count}")
    elif open_count < declared_open:
        fail("R9", f"미해소 이탈이 줄었으면 상한도 내린다: 상한={declared_open} 실제={open_count}")

    declared_gap = declared_int(map_doc, "패널 차집합 상한")
    if declared_gap is None:
        fail("R9", "docs/mockups/README.md 에 '패널 차집합 상한 = **N**' 줄이 없다")
    elif gap_total > declared_gap:
        fail("R9", f"패널 차집합이 상한을 넘었다: 상한={declared_gap} 실제={gap_total}")
    elif gap_total < declared_gap:
        fail("R9", f"패널 차집합이 줄었으면 상한도 내린다: 상한={declared_gap} 실제={gap_total}")

    declared_screens = declared_int(map_doc, "매핑된 구현 파일")
    if declared_screens is None:
        fail("R9", "docs/mockups/README.md 에 '매핑된 구현 파일 = **N**' 줄이 없다")
    elif declared_screens != len(declared_impl):
        fail("R9", f"매핑 집계 불일치: 선언={declared_screens} 실제={len(declared_impl)}")

    # ---- R10 ------------------------------------------------------------
    dupe_sec = section(ledger_doc, "토큰 복제 원장")
    if not dupe_sec:
        fail("R10", "web/src/design/README.md 에 '## 토큰 복제 원장' 섹션이 없다")
    else:
        declared_dupes: dict[str, tuple[int, int, str, str]] = {}
        for cells in rows(dupe_sec, 5):
            f = code_cell(cells[0])
            try:
                shared, mism = int(code_cell(cells[1])), int(code_cell(cells[2]))
            except ValueError:
                fail("R10", f"원장 행의 건수가 정수가 아니다: {f} -> {cells[1]!r}/{cells[2]!r}")
                continue
            disp = code_cell(cells[3])
            if disp not in ("DUP", "IND"):
                fail("R10", f"원장 행의 처분이 DUP/IND 중 하나가 아니다: {f} -> {disp!r}")
                continue
            declared_dupes[f] = (shared, mism, disp, cells[4].strip())

        # (a) 양방향 집합 동등성 — 새 복제 파일은 원장 행 없이 통과할 수 없고(침묵 불가),
        #     사라진 파일의 행도 고아로 잡힌다.
        for f in sorted(set(token_dupes) - set(declared_dupes)):
            s, m_ = dupe_row(f, token_dupes[f], canon)
            fail("R10", f"정본 토큰을 재선언하는데 원장에 없다: {f} (공유 {s}개, 값 불일치 {m_}건)")
        for f in sorted(set(declared_dupes) - set(token_dupes)):
            fail("R10", f"원장 행이 실제 복제와 맞지 않는다(고아): {f}")

        for f in sorted(set(token_dupes) & set(declared_dupes)):
            shared, mism, disp, why = declared_dupes[f]
            a_shared, a_mism = dupe_row(f, token_dupes[f], canon)
            # (b) 건수 일치
            if (shared, mism) != (a_shared, a_mism):
                fail("R10", f"원장 건수 불일치: {f} 원장=공유 {shared}/불일치 {mism} "
                            f"실제=공유 {a_shared}/불일치 {a_mism}")
            # (c) DUP 은 정본 팔레트의 복제다 — 값이 어긋나면 "조용히 어긋난" 것이므로 즉시 실패.
            if disp == "DUP" and a_mism:
                bad = sorted(n for n, v in token_dupes[f].items() if canon[n] != v)
                fail("R10", f"DUP 파일의 값이 정본과 다르다: {f} -> {bad}")
            # (d) IND 는 이름만 겹치는 독립 팔레트다 — 사유 없이 예외가 되는 것을 막는다.
            if disp == "IND" and len(why.replace("—", "").strip()) < 10:
                fail("R10", f"IND 행에 사유가 없다: {f}")

        # (e) 유지비 문구 — 게이트가 계산한 값과만 일치할 수 있다. 즉 **참값이 거부될 수 없다.**
        dup_only = {f: d for f, d in token_dupes.items()
                    if declared_dupes.get(f, (0, 0, "DUP", ""))[2] == "DUP"}
        lo, hi = dupe_cost(dup_only, canon)
        for doc_name, doc in (("web/src/design/README.md", ledger_doc),
                              ("docs/mockups/README.md", map_doc)):
            m = re.search(r"최소\s*\*\*(\d+)\*\*곳\s*·\s*최대\s*\*\*(\d+)\*\*곳", doc)
            if not m:
                fail("R10", f"{doc_name} 에 '최소 **N**곳 · 최대 **M**곳' 문구가 없다")
            elif (int(m.group(1)), int(m.group(2))) != (lo, hi):
                fail("R10", f"{doc_name} 의 유지비 문구가 실제와 다르다: "
                            f"문구={m.group(1)}~{m.group(2)} 실제={lo}~{hi}")

    if not errors:
        print(f"R1-R5  화면 매핑 OK — 구현 파일 {len(declared_impl)}개 · mockup {len(mockups)}종 "
              f"(구현 없음 {len(unimplemented)}종, 구현 신호 0 hit 확인)")
        print(f"R6-R7  정본 이탈 원장 OK — HEX {sum(hexes.values())}건 / INLINE {sum(inlines.values())}건 "
              f"/ rgba {sum(rgbas.values())}건, EXC 마커 {len(markers)}건")
        print(f"R8     패널 구조 OK — 쌍 {len(seen_pairs)}개, 차집합 {gap_total}건")
        print(f"R9     상한 OK — 미해소 {open_count}/{declared_open} · 패널 차집합 {gap_total}/{declared_gap}")
        n_dup = sum(1 for f in token_dupes if declared_dupes[f][2] == "DUP")
        n_ind = len(token_dupes) - n_dup
        print(f"R10    토큰 복제 원장 OK — 복제 파일 {len(token_dupes)}개(DUP {n_dup} · IND {n_ind}), "
              f"정본 토큰 {len(canon)}개, 유지비 {lo}~{hi}곳")
    return report()


def report() -> int:
    if errors:
        print("렌더링 정합성 게이트 실패:", file=sys.stderr)
        for e in errors:
            print("  " + e, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
