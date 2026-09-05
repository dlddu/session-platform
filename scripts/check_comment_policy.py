#!/usr/bin/env python3
"""주석 비중복성 판정 원장 ↔ 실제 주석 상태 대조 체커.

`docs/comment-policy.md`의 판정 이력은 「이 범위는 판정이 끝났다」는 주장이다. 그 주장은
사람이 손으로 적고 아무것도 검사하지 않으므로 **조용히 낡는다** — 판정을 잰 커밋과 머지
커밋 사이에 다른 PR이 그 경로에 주석을 더하면, 그 증분은 어느 행에도 속하지 않은 채 영원히
미판정으로 남는다. 자매 레포 `dlddu/homelab-k3s-mcp`에서 실제로 그렇게 됐고, 이 스크립트는
그 레포가 세운 게이트를 이 레포의 스캔 범위로 옮긴 것이다.

이 체커가 판정하는 것은 **원장의 무결성**이지 중복 자체가 아니다. 「이 주석이 복원 가능한가」는
정책의 판정 절차대로 사람이 답하고, 기계는 **판정이 끝난 범위가 그 뒤로 변하지 않았는지**만
본다. 변했다면 그 범위는 다시 판정받아야 하고, 그때 원장을 갱신하는 것이 곧 재판정의 기록이다.

클러스터도 서드파티 의존성도 필요 없다(표준 라이브러리 전용) — CI에서 초 단위로 돈다.

판정하는 것:

* **R1** 원장 행의 범위 파일이 실재하고 정책의 스캔 범위 안이다. 빈 범위는 등재가 아니다.
* **R2** 각 행의 범위를 **모델 as-is 지문과 동일한 추출·정규화·정렬**로 재측정한 주석 줄 수와
  지문이 등재값과 같다. 다르면 그 범위는 판정 이후 주석이 바뀐 것이므로 재판정 대상이다.
* **R3** 행 사이에 같은 파일이 두 번 등재되지 않는다(판정 완료량의 이중 계상 방지).
* **R4** 합계 마커 == 원장 행들의 주석 줄 수 합(양방향 미러). 범위를 더하거나 빼면 같은 PR에서
  합계도 움직여야 한다.
* **R5** 원장 문서가 「이미 말하는 곳」을 가리킬 때 **줄 번호 좌표를 쓰지 않는다.** 줄 번호는 다음
  판정이 그 파일에서 주석을 지우는 순간 밀려나 조용히 썩는데, R1~R4 중 어느 것도 그것을 재측정하지
  않아 **영원히 초록**이다(실제로 한 슬라이스가 자기 원장의 좌표 5건을 한 번에 낡게 만들었다).
  좌표는 심볼 이름이나 AC 번호로 적는다 — 그쪽은 grep 으로 되짚을 수 있다.

통과하면 **범위 전체의 현재 인구조사를 출력한다.** 그 수치는 문서 프로즈에 적지 않는다 —
낡는 형태를 없애는 것이 이 게이트의 목적이고, 최신값이 필요하면 여기서 읽는다.
"""

from __future__ import annotations

import hashlib
import pathlib
import re
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
REPO_ROOT = HERE.parent
POLICY = REPO_ROOT / "docs" / "comment-policy.md"

# 아래 넷은 모델 tbm_session-platform-comment-redundancy 의 as-is 버전 스크립트와 글자 그대로
# 같은 정의다. 하나라도 갈리면 이 게이트가 강제하는 지문이 모델이 관측하는 지문과 다른 것을
# 재므로, 고칠 때는 모델 정의와 함께 고칠 것. 공백 문자 클래스는 `\s` 가 아니라 POSIX
# `[[:space:]]`(LC_ALL=C)와 같은 바이트 집합을 그대로 적는다 — `\s` 는 유니코드 공백까지
# 집어서 원본 파이프라인과 갈릴 수 있다.
SCAN_PATHSPECS = ("control-plane", "data-plane", "web/src", "web/e2e")
SCAN_EXCLUDE_RE = re.compile(r"(^|/)(vendor|node_modules|dist)/")
COMMENT_RE = re.compile(r"^[ \t\v\f\r]*(//|/\*|\*[^/])")
DIRECTIVE_RE = re.compile(r"//go:|nolint|eslint-|@ts-|검증 AC:|mock-exception:|mockup:")
WHITESPACE_RUN_RE = re.compile(r"[ \t\v\f\r]+")

# R5 — 원장이 가리키는 좌표에서 금지되는 형태. 확장자를 요구해 산문의 우연한 `낱말:1` 을 피하고,
# 뒤에 숫자가 붙는 경우만 본다(`docs/test/e2e.md` 같은 순수 경로 포인터는 권장 형태라 걸리지 않는다).
LINE_COORDINATE_RE = re.compile(r"[A-Za-z0-9_./-]+\.(?:go|ts|tsx|md|yaml|yml|sh|py):\d+(?:-\d+)?")

LEDGER_OPEN = "<!-- 판정-원장 -->"
LEDGER_CLOSE = "<!-- /판정-원장 -->"
TOTAL_OPEN = "<!-- 판정-합계 -->"
TOTAL_CLOSE = "<!-- /판정-합계 -->"

BACKTICKED_RE = re.compile(r"`([^`]+)`")
FINGERPRINT_LEN = 12

failures: list[str] = []


def fail(rule: str, message: str) -> None:
    failures.append(f"[{rule}] {message}")


def scan_files() -> list[str]:
    """모델 as-is 지문과 동일한 스캔 범위(레포 상대 경로, 정렬)."""
    out = subprocess.run(
        ["git", "ls-files", "--", *SCAN_PATHSPECS],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    paths = [ln for ln in out.splitlines() if ln and not SCAN_EXCLUDE_RE.search(ln)]
    if not paths:
        raise SystemExit(
            "스캔 범위가 비어 있다 — git ls-files 가 아무 파일도 돌려주지 않았다. "
            "레포 루트에서 실행 중인지 확인할 것."
        )
    return sorted(paths)


def comment_lines(paths: list[str]) -> list[str]:
    """`경로:주석` 줄의 정규화·정렬 목록. as-is 지문과 같은 구성이다.

    지시어 주석(기계가 읽는 것)은 제외한다. 판정 대상이 아니기 때문이다.
    """
    hits: list[str] = []
    for rel in sorted(paths):
        try:
            text = (REPO_ROOT / rel).read_text(encoding="utf-8")
        except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError):
            continue
        for line in text.splitlines():
            if not COMMENT_RE.match(line):
                continue
            entry = f"{rel}:{line}"
            if DIRECTIVE_RE.search(entry):
                continue
            hits.append(WHITESPACE_RUN_RE.sub(" ", entry).strip(" "))
    return sorted(hits)


def fingerprint(hits: list[str]) -> str:
    return hashlib.sha256("\n".join(hits).encode("utf-8")).hexdigest()


def marked_block(text: str, open_marker: str, close_marker: str) -> str:
    """마커 사이 본문. 정규식 끝 앵커(`$`)로 뜯지 않는다 — 멀티라인에서 줄 끝에도 붙어
    표가 조용히 0행으로 파싱되고, 0행은 '위반 0' 으로 보여 초록으로 새어 나간다."""
    if open_marker not in text or close_marker not in text:
        raise SystemExit(f"{POLICY.name}: 마커({open_marker} … )를 찾지 못했다.")
    return text.split(open_marker, 1)[1].split(close_marker, 1)[0]


def parse_ledger(text: str) -> list[dict]:
    """판정 이력 표를 행 목록으로. 헤더 행과 구분선은 버린다."""
    rows: list[dict] = []
    lines = [ln.strip() for ln in marked_block(text, LEDGER_OPEN, LEDGER_CLOSE).splitlines()]
    table = [ln for ln in lines if ln.startswith("|") and ln.endswith("|")]
    for index, line in enumerate(table):
        cells = [c.strip() for c in line.strip("|").split("|")]
        if all(set(c) <= set("-: ") and c for c in cells):
            continue  # 구분선
        if index + 1 < len(table):
            nxt = [c.strip() for c in table[index + 1].strip("|").split("|")]
            if nxt and all(set(c) <= set("-: ") and c for c in nxt):
                continue  # 구분선 바로 앞 = 헤더
        if len(cells) != 5:
            raise SystemExit(f"{POLICY.name}: 판정 이력 행의 열 수가 5가 아니다 -> {line}")
        count = cells[2].strip("`")
        if not count.isdigit():
            raise SystemExit(f"{POLICY.name}: 주석 줄 수가 정수가 아니다 ({count!r}).")
        rows.append(
            {
                # 같은 날 두 범위를 판정하는 일이 흔하므로 날짜만으로는 행을 못 가리킨다.
                "date": f"{cells[0]}(#{len(rows) + 1})",
                "paths": [m.group(1) for m in BACKTICKED_RE.finditer(cells[1])],
                "lines": int(count),
                "fingerprint": cells[3].strip("`"),
                "row": line,
            }
        )
    if not rows:
        raise SystemExit(f"{POLICY.name}: 판정 이력에서 행을 하나도 읽지 못했다(파싱 실패).")
    return rows


def parse_total(text: str) -> int:
    raw = marked_block(text, TOTAL_OPEN, TOTAL_CLOSE).strip()
    if not raw.isdigit():
        raise SystemExit(f"{POLICY.name}: 판정 합계가 정수가 아니다 ({raw!r}).")
    return int(raw)


def census(hits: list[str]) -> str:
    files = sorted({h.split(":", 1)[0] for h in hits})
    buckets = {"Go 소스": 0, "Go 테스트": 0, "TypeScript": 0, "기타": 0}
    for hit in hits:
        path = hit.split(":", 1)[0]
        if path.endswith("_test.go"):
            buckets["Go 테스트"] += 1
        elif path.endswith(".go"):
            buckets["Go 소스"] += 1
        elif path.endswith((".ts", ".tsx")):
            buckets["TypeScript"] += 1
        else:
            buckets["기타"] += 1
    breakdown = " · ".join(f"{k} {v}" for k, v in buckets.items() if v)
    return f"판정 대상 주석 {len(hits)}줄 / {len(files)}파일 ({breakdown})"


def main() -> int:
    if not POLICY.exists():
        print(f"[R1] 정책 SSOT 가 없다: {POLICY.relative_to(REPO_ROOT)}", file=sys.stderr)
        return 1

    text = POLICY.read_text(encoding="utf-8")
    rows = parse_ledger(text)
    total = parse_total(text)

    in_scope = set(scan_files())

    # R1 — 범위의 실재와 유효성
    for row in rows:
        if not row["paths"]:
            fail("R1", f"{row['date']} 행에 범위 파일이 선언돼 있지 않다(빈 범위는 등재가 아니다).")
        for rel in row["paths"]:
            if rel not in in_scope:
                fail(
                    "R1",
                    f"{row['date']} 행의 `{rel}` 이 정책 스캔 범위에 없다"
                    f" (범위: {' · '.join(SCAN_PATHSPECS)}). 파일이 사라졌거나 이름이"
                    " 바뀌었으면 그 범위는 다시 판정받아야 한다.",
                )

    # R3 — 범위 간 이중 계상
    seen: dict[str, str] = {}
    for row in rows:
        for rel in row["paths"]:
            if rel in seen:
                fail("R3", f"`{rel}` 이 {seen[rel]} 행과 {row['date']} 행에 모두 등재돼 있다.")
            else:
                seen[rel] = row["date"]

    # R2 — 등재값 ↔ 재측정
    for row in rows:
        present = [rel for rel in row["paths"] if rel in in_scope]
        hits = comment_lines(present)
        actual_fingerprint = fingerprint(hits)[:FINGERPRINT_LEN]
        if len(hits) != row["lines"]:
            fail(
                "R2",
                f"{row['date']} 행의 주석 줄 수가 등재 {row['lines']} != 실측 {len(hits)}."
                " 등재 이후 이 범위의 주석이 바뀌었다 — 정책의 판정 절차로 다시 판정하고"
                " 같은 PR 에서 이 행을 갱신할 것.",
            )
        if actual_fingerprint != row["fingerprint"]:
            fail(
                "R2",
                f"{row['date']} 행의 지문이 등재 `{row['fingerprint']}` !="
                f" 실측 `{actual_fingerprint}`. 줄 수가 같아도 내용이 바뀌면 재판정 대상이다.",
            )

    # R5 — 줄 번호 좌표 금지
    for lineno, line in enumerate(text.splitlines(), 1):
        for hit in LINE_COORDINATE_RE.finditer(line):
            fail(
                "R5",
                f"{POLICY.name} {lineno} 번째 줄에 줄 번호 좌표 `{hit.group(0)}` 이 있다."
                " 줄 번호는 다음 판정이 그 파일을 건드리는 순간 밀려나고 어떤 규칙도 그것을"
                " 재측정하지 않는다 — 심볼 이름이나 AC 번호로 적을 것.",
            )

    # R4 — 합계 미러(양방향)
    ledger_sum = sum(row["lines"] for row in rows)
    if total != ledger_sum:
        fail(
            "R4",
            f"판정 합계 {total} != 원장 행 줄 수 합 {ledger_sum}."
            " 범위를 더하거나 빼면 같은 PR 에서 합계도 움직여야 한다.",
        )

    hits = comment_lines(sorted(in_scope))
    if failures:
        for line in failures:
            print(line, file=sys.stderr)
        print(f"\nFAIL: {len(failures)}건 — {census(hits)}", file=sys.stderr)
        return 1

    judged = ledger_sum
    share = (judged * 100.0 / len(hits)) if hits else 0.0
    print(
        f"OK: 규칙 R1~R5 위반 없음 — {census(hits)}"
        f" · 판정 완료 {judged}줄({share:.1f}%) / 등재 범위 {len(rows)}"
    )
    for row in rows:
        print(
            f"  {row['date']}  {row['lines']:>4}줄  {row['fingerprint']}"
            f"  {' '.join(row['paths'])}"
        )
    print(f"  전체 지문: {fingerprint(hits)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
