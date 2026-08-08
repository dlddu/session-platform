#!/usr/bin/env python3
"""e2e 충실도 허용목록 ↔ 코드 seam 양방향 1:1 검사.

이 레포의 e2e는 kind 실클러스터에 배포된 control-plane(SUT)을 대상으로 진짜 Pod ·
실 ConfigMap/Lease · 실 data-plane 에이전트 위에서 도는 것이 기본이고, 충실도를 낮추는
치환(프로덕션 게이트의 no-op 분기 · test-only 트리거 · web 네트워크 인터셉트)은
`docs/test/e2e.md`의 「e2e 충실도 허용목록」에 등재된 것만 허용한다.

이 스크립트는 그 정책을 문서 낭독이 아니라 **집합 동등성**으로 강제한다:

  R1  허용목록 섹션이 존재한다(제목에 충실도/모킹/허용목록).
  R2  등재 표의 카테고리는 GATE/TRIG/EXT/NET 중 하나이고, '구동 구간'·'잔여' 칸이 비어 있지 않다.
  R3  회계 표가 참조하는 CODE는 등재 표에 존재한다(비-seam은 '-').
  R4  등재된 CODE는 회계 표에 최소 1행을 가진다(고아 등재 없음).
  R5  회계 표의 (파일, 토큰) 집합 == 코드 스캔 결과(마커 토큰 제외). 양방향 완전 동등.
  R6  코드의 `mock-exception: <CODE>` 마커는 등재된 CODE만 쓰고, CODE별 마커 파일 집합이
      그 CODE의 회계 행 파일 집합과 정확히 같다.
  R7  web 네트워크 인터셉트 히트는 NET 카테고리 CODE에 귀속돼야 한다(현재 0건 = 회귀 방지선).
  R8  집계 줄의 숫자가 실제와 일치한다.

스캔 스코프와 토큰 정규식은 정합성 모델 `tbm_session-platform-e2e-mock-policy`의 as-is
버전 스크립트와 **동일하게** 유지한다(둘 중 하나만 바뀌면 감지 루프와 CI 게이트가 서로
다른 것을 세게 된다). 표준 라이브러리만 쓰고, 파일 목록은 `git ls-files`로 얻는다.
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DOC = ROOT / "docs" / "test" / "e2e.md"

# --- 모델 as-is 버전 스크립트와 동기화되는 스캔 정의 -------------------------------

SCOPE_PATHSPECS = [
    "control-plane/cmd",
    "control-plane/internal/api",
    "control-plane/test",
    "data-plane/cmd",
    "deploy",
    "k8s",
    "scripts/e2e",
    "web",
    "Makefile",
    ".github/workflows/e2e.yml",
]

# 대안은 긴 것부터 — POSIX grep -E(leftmost-longest)와 같은 결과를 내게 한다.
TOKEN_RE = re.compile(
    r"CRIU_ENABLED"
    r"|E2E_[A-Z0-9_]+"
    r"|NewStub[A-Za-z0-9]*"
    r"|WithTestEndpoints"
    r"|routeFromHAR"
    r"|routeWebSocket"
    r"|route\.continue\("
    r"|route\.fulfill\("
    r"|route\.abort\("
    r"|mock-exception:"
    r"|test-only"
    r"|\.unroute\("
    r"|\.route\("
)

# SUT 주소 지정일 뿐 충실도 seam이 아니다(모델 정의의 명시적 제외).
TOKEN_EXCLUDE = {"E2E_BASE_URL"}

MARKER_TOKEN = "mock-exception:"
INTERCEPT_TOKENS = {
    ".route(",
    ".unroute(",
    "route.fulfill(",
    "route.abort(",
    "route.continue(",
    "routeFromHAR",
    "routeWebSocket",
}

CATEGORIES = {"GATE", "TRIG", "EXT", "NET"}
NON_SEAM = {"-", "—", "–"}  # 회계 표에서 '등재 대상 아님'을 뜻하는 CODE 자리표

MARKER_RE = re.compile(r"mock-exception:\s*([A-Z][A-Z0-9-]*)")
SECTION_RE = re.compile(r"^##+\s+.*(?:충실도|모킹|허용목록)")

failures: list[str] = []


def fail(rule: str, msg: str) -> None:
    failures.append(f"[{rule}] {msg}")


# --- 코드 스캔 ------------------------------------------------------------------


def scoped_files() -> list[str]:
    out = subprocess.run(
        ["git", "ls-files", "--", *SCOPE_PATHSPECS],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    ).stdout.splitlines()
    files = []
    for f in out:
        if not f or "node_modules/" in f:
            continue
        if f.endswith(".test.ts") or f.endswith(".test.tsx"):
            continue
        if f.endswith("_test.go"):
            # e2e 스위트만 SUT 경로다. 단위/통합(integration) 하네스는 범위 밖.
            with open(ROOT / f, encoding="utf-8", errors="replace") as fh:
                if fh.readline().rstrip("\n") != "//go:build e2e":
                    continue
        files.append(f)
    return sorted(files)


def scan() -> tuple[set[tuple[str, str]], dict[str, set[str]]]:
    """(파일, 토큰) 쌍 집합과 CODE -> 마커가 있는 파일 집합을 돌려준다."""
    pairs: set[tuple[str, str]] = set()
    markers: dict[str, set[str]] = {}
    for f in scoped_files():
        text = (ROOT / f).read_text(encoding="utf-8", errors="replace")
        for tok in TOKEN_RE.findall(text):
            if tok not in TOKEN_EXCLUDE:
                pairs.add((f, tok))
        for code in MARKER_RE.findall(text):
            markers.setdefault(code, set()).add(f)
    return pairs, markers


# --- 문서 파싱 ------------------------------------------------------------------


def fidelity_section(lines: list[str]) -> list[str] | None:
    start = None
    for i, line in enumerate(lines):
        if SECTION_RE.match(line):
            start = i
            break
    if start is None:
        return None
    for j in range(start + 1, len(lines)):
        if lines[j].startswith("## "):
            return lines[start:j]
    return lines[start:]


def block(lines: list[str], name: str) -> list[str] | None:
    open_tag, close_tag = f"<!-- fidelity:{name} -->", f"<!-- /fidelity:{name} -->"
    try:
        a = next(i for i, l in enumerate(lines) if l.strip() == open_tag)
        b = next(i for i, l in enumerate(lines) if l.strip() == close_tag)
    except StopIteration:
        return None
    return lines[a + 1 : b]


def table_rows(lines: list[str]) -> list[list[str]]:
    """마크다운 표에서 헤더·구분선을 뺀 데이터 행의 셀 목록."""
    rows = []
    for line in lines:
        s = line.strip()
        if not s.startswith("|"):
            continue
        cells = [c.strip() for c in s.strip("|").split("|")]
        if all(set(c) <= {"-", ":", " "} and c for c in cells):
            continue  # 구분선
        rows.append(cells)
    return rows[1:] if rows else []  # 첫 행은 헤더


def unquote(cell: str) -> str:
    return cell.strip().strip("`").strip()


def main() -> int:
    if not DOC.exists():
        fail("R1", f"{DOC.relative_to(ROOT)} 가 없다")
        return report()

    lines = DOC.read_text(encoding="utf-8").splitlines()
    section = fidelity_section(lines)
    if section is None:
        fail(
            "R1",
            "docs/test/e2e.md 에 충실도 허용목록 섹션이 없다 "
            "(제목에 '충실도'/'모킹'/'허용목록' 중 하나를 포함한 ## 섹션이어야 한다)",
        )
        return report()
    if not section[0].startswith("## "):
        fail("R1", f"허용목록 섹션은 ## 수준이어야 한다: {section[0]!r}")

    reg_block = block(section, "registry")
    led_block = block(section, "ledger")
    sum_block = block(section, "summary")
    for name, blk in (("registry", reg_block), ("ledger", led_block), ("summary", sum_block)):
        if blk is None:
            fail("R1", f"허용목록 섹션에 <!-- fidelity:{name} --> 블록이 없다")
    if reg_block is None or led_block is None or sum_block is None:
        return report()

    # --- 등재 표 (registry): CODE | 카테고리 | 구동 구간 | 잔여 -------------------
    registry: dict[str, str] = {}
    for cells in table_rows(reg_block):
        if len(cells) < 4:
            fail("R2", f"등재 표 행의 칸이 모자란다(4칸 필요): {cells}")
            continue
        code, cat, runs, residual = unquote(cells[0]), unquote(cells[1]), cells[2], cells[3]
        if code in registry:
            fail("R2", f"등재 표에 CODE 중복: {code}")
        if cat not in CATEGORIES:
            fail("R2", f"{code}: 카테고리 {cat!r} 는 허용되지 않는다 (허용: {sorted(CATEGORIES)})")
        if not runs.strip() or runs.strip() == "-":
            fail("R2", f"{code}: 'e2e에서 실제로 구동되는 구간' 칸이 비어 있다")
        if not residual.strip() or residual.strip() == "-":
            fail("R2", f"{code}: '검증되지 않는 잔여' 칸이 비어 있다 — 무엇이 검증되지 않는지 반드시 적는다")
        registry[code] = cat

    # --- 회계 표 (ledger): 파일 | 토큰 | CODE | 사유 -----------------------------
    ledger: set[tuple[str, str]] = set()
    ledger_files: dict[str, set[str]] = {}
    non_seam_rows = 0
    for cells in table_rows(led_block):
        if len(cells) < 4:
            fail("R3", f"회계 표 행의 칸이 모자란다(4칸 필요): {cells}")
            continue
        f, tok, code, why = unquote(cells[0]), unquote(cells[1]), unquote(cells[2]), cells[3]
        if (f, tok) in ledger:
            fail("R3", f"회계 표에 중복 행: {f} · {tok}")
        ledger.add((f, tok))
        if not why.strip():
            fail("R3", f"{f} · {tok}: 사유 칸이 비어 있다")
        if code in NON_SEAM:
            non_seam_rows += 1
            continue
        if code not in registry:
            fail("R3", f"{f} · {tok}: 등재 표에 없는 CODE {code!r} 를 참조한다 (비-seam이면 '—')")
            continue
        ledger_files.setdefault(code, set()).add(f)

    for code in registry:
        if code not in ledger_files:
            fail("R4", f"고아 등재: {code} 는 회계 표에 행이 하나도 없다 — 코드에서 사라졌으면 등재도 지운다")

    # --- 코드 스캔과 대조 --------------------------------------------------------
    pairs, markers = scan()
    scanned = {(f, t) for (f, t) in pairs if t != MARKER_TOKEN}
    marker_pairs = {(f, t) for (f, t) in pairs if t == MARKER_TOKEN}

    for f, tok in sorted(scanned - ledger):
        fail("R5", f"미등재 seam: {f} · {tok} — 허용목록 회계 표에 없다 (등재하거나 치환을 제거할 것)")
    for f, tok in sorted(ledger - scanned):
        fail("R5", f"고아 회계 행: {f} · {tok} — 코드에 없다 (허용목록에서 지울 것)")

    marker_files = {f for fs in markers.values() for f in fs}
    for f, _ in sorted(marker_pairs):
        if f not in marker_files:
            fail(
                "R6",
                f"{f}: `mock-exception:` 토큰은 있으나 뒤에 CODE가 없다 — "
                "표기 규약은 `mock-exception: <CODE> — <사유>` 다",
            )
    for code, files in sorted(markers.items()):
        if code not in registry:
            fail("R6", f"미등록 CODE 마커: `mock-exception: {code}` ({', '.join(sorted(files))}) 가 등재 표에 없다")
    for code, files in sorted(ledger_files.items()):
        got = markers.get(code, set())
        for f in sorted(files - got):
            fail("R6", f"{code}: {f} 에 회계 행은 있으나 `mock-exception: {code}` 마커가 없다")
        for f in sorted(got - files):
            fail("R6", f"{code}: {f} 에 마커는 있으나 그 파일의 회계 행이 없다")

    intercepts = sorted((f, t) for (f, t) in scanned if t in INTERCEPT_TOKENS)
    net_codes = {c for c, cat in registry.items() if cat == "NET"}
    for f, tok in intercepts:
        owner = next((c for c, fs in ledger_files.items() if f in fs and c in net_codes), None)
        if owner is None:
            fail(
                "R7",
                f"web 네트워크 인터셉트 {f} · {tok} 가 NET 카테고리 등재에 귀속되지 않았다 — "
                "실 SUT가 낼 수 없는 상태(서버 실패·지연 주입)만 NET으로 등재할 수 있다",
            )
    if not intercepts and net_codes:
        fail("R7", f"NET 등재 {sorted(net_codes)} 가 있으나 코드에 인터셉트가 0건이다")

    # --- 집계 --------------------------------------------------------------------
    marker_sites = sum(
        len(MARKER_RE.findall((ROOT / f).read_text(encoding="utf-8", errors="replace")))
        for f in sorted({f for fs in markers.values() for f in fs})
    )
    expect = {
        "seam": len(registry),
        "gate": sum(1 for c in registry.values() if c == "GATE"),
        "trig": sum(1 for c in registry.values() if c == "TRIG"),
        "ext": sum(1 for c in registry.values() if c == "EXT"),
        "net": sum(1 for c in registry.values() if c == "NET"),
        "marker_sites": marker_sites,
        "marker_files": len(marker_files),
        "ledger": len(ledger),
        "non_seam": non_seam_rows,
        "intercept": len(intercepts),
    }
    declared = parse_summary(sum_block)
    for key, want in expect.items():
        if key not in declared:
            fail("R8", f"집계 블록에 '{SUMMARY_KEYS[key]}' 숫자가 없다")
        elif declared[key] != want:
            fail("R8", f"집계 불일치: {SUMMARY_KEYS[key]} 문서={declared[key]} 실제={want}")

    if failures:
        return report()

    print("OK: e2e 충실도 허용목록 ↔ 코드 seam 1:1")
    print(
        f"  등재 seam {expect['seam']} (GATE {expect['gate']} · TRIG {expect['trig']} · "
        f"EXT {expect['ext']} · NET {expect['net']})"
    )
    print(
        f"  회계 행 {expect['ledger']} == 스캔된 (파일,토큰) 쌍 {len(scanned)} "
        f"(등재 귀속 {len(ledger) - non_seam_rows} · 비-seam {non_seam_rows}) "
        f"+ mock-exception: 마커 {len(marker_pairs)}파일"
    )
    print(
        f"  마커 {marker_sites}지점 / {expect['marker_files']}파일 · "
        f"web 네트워크 인터셉트 {expect['intercept']}건"
    )
    return 0


SUMMARY_KEYS = {
    "seam": "등재 seam",
    "gate": "GATE",
    "trig": "TRIG",
    "ext": "EXT",
    "net": "NET",
    "marker_sites": "마커 지점",
    "marker_files": "마커 파일",
    "ledger": "회계 행",
    "non_seam": "비-seam",
    "intercept": "인터셉트",
}


def parse_summary(sum_block: list[str]) -> dict[str, int]:
    """`- <key>: **<n>**` 형태의 집계 줄을 읽는다. key 는 SUMMARY_KEYS 의 값."""
    got: dict[str, int] = {}
    text = "\n".join(sum_block)
    for key, label in SUMMARY_KEYS.items():
        m = re.search(re.escape(label) + r"[^*\n]*\*\*(\d+)\*\*", text)
        if m:
            got[key] = int(m.group(1))
    return got


def report() -> int:
    print("FAIL: e2e 충실도 허용목록이 코드와 어긋난다 (docs/test/e2e.md)\n", file=sys.stderr)
    for f in failures:
        print("  " + f, file=sys.stderr)
    print(
        f"\n{len(failures)}건. 정책: 미등재 치환도, 코드에 없는 고아 등재도 모두 drift다.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
