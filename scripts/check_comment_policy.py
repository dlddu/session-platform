#!/usr/bin/env python3
"""주석 필요성 판정 원장 ↔ 실제 주석 상태 대조 체커.

`docs/comment-policy/ledger.md`의 판정 원장은 「이 범위는 판정이 끝났다」는 주장이다. 그 주장은
사람이 손으로 적고 아무것도 검사하지 않으므로 **조용히 낡는다** — 판정을 잰 커밋과 머지 커밋 사이에
다른 PR이 그 경로에 주석을 더하면, 그 증분은 어느 행에도 속하지 않은 채 영원히 미판정으로 남는다.

이 체커가 판정하는 것은 **원장의 무결성**이지 주석의 필요성 자체가 아니다. 「이 주석이 필요한가」는
정책의 판정 절차대로 사람이 답하고, 기계는 **판정이 끝난 범위가 그 뒤로 변하지 않았는지**만 본다.
변했다면 그 범위는 다시 판정받아야 하고, 그때 원장을 갱신하는 것이 곧 재판정의 기록이다.

**스캔 범위와 추출 규칙은 모델 `tbm_session-platform-comment-necessity` 의 as-is 버전 스크립트와
글자 그대로**여야 한다. 하나라도 갈리면 이 게이트가 강제하는 지문이 모델이 관측하는 지문과 다른
것을 재므로, 고칠 때는 모델 정의와 함께 고칠 것.

* **R1** 원장 행의 범위 파일이 실재하고 정책의 스캔 범위 안이다. 빈 범위는 등재가 아니다.
* **R2** 각 행의 범위를 **모델 as-is 지문과 동일한 추출·정규화·정렬**로 재측정한 주석 줄 수와
  지문이 등재값과 같다. 다르면 그 범위는 판정 이후 주석이 바뀐 것이므로 재판정 대상이다.
* **R3** 행 사이에 같은 파일이 두 번 등재되지 않는다(판정 완료량의 이중 계상 방지).
* **R4** 원장이 **판정 대상 파일을 전수** 담는다 — 실측 파일 집합과 행의 파일 집합이 같다.
  등재가 빠지면 그 파일의 주석은 영원히 미판정으로 남는다. 게이트가 원장을 못 채운 채로
  초록으로 새어 나가는 사고를 막는다.
* **R5** **판정 칸 표기**가 유효하다(`완료` 또는 `—`). 옛 기준의 표기(`①②③④` 등)는 유효한 판정이
  아니다 — 필요성 기준이 바뀐 뒤의 판정 완료가 아니므로 `—`로 되돌린다.
* **R6** 정책 디렉터리의 문서가 「이미 말하는 곳」을 가리킬 때 **줄 번호 좌표를 쓰지 않는다.** 줄 번호는
  다음 판정이 그 파일에서 주석을 지우는 순간 밀려나 조용히 썩는데, R1~R5 중 어느 것도 그것을
  재측정하지 않아 **영원히 초록**이다. 좌표는 심볼 이름이나 AC 번호로 적는다.

**합계·잔량은 원장에 두지 않는다.** 판정 완료 줄·행 수와 미등재 여부는 이 게이트가 표와 실측에서
계산해 출력한다 — 손으로 쓴 집계 값은 언제든 낡는다.

클러스터도 서드파티 의존성도 필요 없다(표준 라이브러리 전용) — CI에서 초 단위로 돈다.
"""

from __future__ import annotations

import hashlib
import pathlib
import re
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
REPO_ROOT = HERE.parent
# 정책 SSOT 는 파일 하나가 아니라 디렉터리다 — 본문(README)·원장(ledger)·패스 상세(passes/)가
# 바뀌는 리듬이 달라서다. 게이트가 파싱하는 마커는 `ledger.md` 에만 있고, 문서 본문 전체를 보는
# 규칙(R6)은 디렉터리 안의 모든 마크다운을 스캔한다.
POLICY_DIR = REPO_ROOT / "docs" / "comment-policy"
POLICY_README = POLICY_DIR / "README.md"
LEDGER = POLICY_DIR / "ledger.md"

# ── 아래는 모델 tbm_session-platform-comment-necessity 의 as-is 버전 스크립트와 글자 그대로
# 같은 정의다. 하나라도 갈리면 이 게이트가 강제하는 지문이 모델이 관측하는 지문과 다른 것을
# 재므로, 고칠 때는 모델 정의와 함께 고칠 것. 공백 문자 클래스는 `\s` 가 아니라 POSIX
# `[[:space:]]`(LC_ALL=C)와 같은 바이트 집합을 그대로 적는다 — `\s` 는 유니코드 공백까지
# 집어서 원본 파이프라인과 갈릴 수 있다.
#
# 범위는 **포함 목록이 아니라 「레포 전체 − 제외」**다 — 새 디렉터리는 등록 없이 자동으로
# 스캔된다.
SCAN_EXCLUDE_RE = re.compile(
    r"^docs/|\.md$|(^|/)(vendor|node_modules|dist|build|target|\.venv|venv|__pycache__|\.next|coverage)/"
    r"|(^|/)(package-lock\.json|yarn\.lock|pnpm-lock\.yaml|go\.sum|uv\.lock|poetry\.lock|Cargo\.lock|Pipfile\.lock)$"
)
# ── 제외 (대상 레포 소유) ── docs/comment-policy/README.md 의 ```comment-scope-exclude 블록(한 줄에 ERE 하나).
# 레포 고유의 ①②③(예: codegen 출력 경로, 테스트 픽스처 데이터)은 모델이 아니라 이 블록에 적는다 —
# 정책 문서를 고치는 대상 레포 PR만으로 범위가 수렴하게 하기 위해서다. 블록이 없으면 고정부만 적용한다.
REPO_EXCLUDE_BLOCK_RE = re.compile(
    r"^```comment-scope-exclude\s*$([\s\S]*?)^```\s*$", re.MULTILINE
)
# ── 기계가 읽는 주석 ── 지문·판정 모두에서 제외. 앞부분은 도구 공통(고정부), 뒤는 레포 고유(슬롯).
DIRECTIVE_RE = re.compile(
    r":#!|//go:|nolint|eslint-|@ts-|prettier-ignore|/// <reference|istanbul ignore|c8 ignore"
    r"|# ?noqa|# ?type:|# ?pragma|# ?pylint:|# ?fmt:|shellcheck |# ?syntax=|yaml-language-server:"
    r"|검증 (AC|시나리오):|mock-exception:|mockup:"
)
WHITESPACE_RUN_RE = re.compile(r"[ \t\v\f\r]+")

# ── 언어군 (템플릿 고정부) ── 파일명 ERE → 주석 줄 ERE. 한 파일은 위에서부터 처음 맞는 군 하나에 든다.
# 견본 접미사(.example 등)는 떼고 본 확장자로 간다(secret.yaml.example → # 언어군).
_SUF = r"(\.(example|sample|template|tmpl|tpl|dist|in|j2))?$"
C_FILES = re.compile(
    r"\.(go|rs|java|kt|kts|scala|groovy|gradle|swift|c|h|cc|cpp|hpp|cs|m|js|jsx|mjs|cjs|ts|tsx|mts|cts|css|scss|less|proto|jsonc)"
    + _SUF
    + r"|(^|/)(go\.mod|go\.work|tsconfig[^/]*\.json|jsconfig[^/]*\.json|\.devcontainer/[^/]*\.json)$"
)
C_PATTERN = re.compile(r"^[ \t\v\f\r]*(//|/\*|\*(?:[ \t\v\f\r]|$)|\{/\*)")
HASH_FILES = re.compile(
    r"\.(py|pyi|rb|sh|bash|zsh|fish|pl|r|ya?ml|toml|tf|tfvars|hcl|cfg|conf|ini|properties|pro|mk|dockerfile|nix|awk|sed)"
    + _SUF
    + r"|(^|/)(Makefile|GNUmakefile|Dockerfile[^/]*|Containerfile|Caddyfile|\.gitignore|\.dockerignore|\.gitattributes"
    r"|\.helmignore|\.editorconfig|\.env[^/]*|CODEOWNERS|requirements[^/]*\.txt)$"
)
HASH_PATTERN = re.compile(r"^[ \t\v\f\r]*#")
DASH_FILES = re.compile(r"\.(sql|lua|hs|elm|ada|adb)" + _SUF)
DASH_PATTERN = re.compile(r"^[ \t\v\f\r]*--")
MIXED_FILES = re.compile(r"\.(html?|vue|svelte|astro)" + _SUF)
MIXED_PATTERN = re.compile(r"^[ \t\v\f\r]*(<!--|//|/\*|\*(?:[ \t\v\f\r]|$)|\{/\*)")
MARKUP_FILES = re.compile(r"\.(xml|svg|xhtml|plist|xsd|xsl)" + _SUF)
MARKUP_PATTERN = re.compile(r"^[ \t\v\f\r]*<!--")
NONE_FILES = re.compile(
    r"\.(json|jsonl|ndjson|csv|tsv|txt|avsc|snap|golden|pem|crt|key|pub|patch|diff|log|lock|sum|mod|map|http)"
    + _SUF
    + r"|(^|/)(LICENSE[^/]*|NOTICE|AUTHORS|\.nvmrc|\.node-version|\.python-version|\.tool-versions|\.gitkeep|py\.typed)$"
)
LANGUAGE_GROUPS = (
    ("C", C_FILES, C_PATTERN),
    ("HASH", HASH_FILES, HASH_PATTERN),
    ("DASH", DASH_FILES, DASH_PATTERN),
    ("MIXED", MIXED_FILES, MIXED_PATTERN),
    ("MARKUP", MARKUP_FILES, MARKUP_PATTERN),
    ("NONE", NONE_FILES, None),
)
GENERATED_MARKER_RE = re.compile(r"DO NOT EDIT|@generated")

# ── 원장 형식 ──
# R6 — 원장이 가리키는 좌표에서 금지되는 형태. 확장자를 요구해 산문의 우연한 `낱말:1` 을 피하고,
# 뒤에 숫자가 붙는 경우만 본다(`docs/test/e2e.md` 같은 순수 경로 포인터는 권장 형태라 걸리지 않는다).
LINE_COORDINATE_RE = re.compile(r"[A-Za-z0-9_./-]+\.(?:go|ts|tsx|md|yaml|yml|sh|py):\d+(?:-\d+)?")

LEDGER_OPEN = "<!-- 판정-원장 -->"
LEDGER_CLOSE = "<!-- /판정-원장 -->"
JUDGED_MARKERS = ("완료", "—")

BACKTICKED_RE = re.compile(r"`([^`]+)`")
FINGERPRINT_LEN = 12

failures: list[str] = []


def fail(rule: str, message: str) -> None:
    failures.append(f"[{rule}] {message}")


def policy_docs() -> list[pathlib.Path]:
    """R6 가 보는 범위 — 정책 디렉터리 안의 모든 마크다운(정렬).

    원장 한 파일만 보면 이 규칙은 조용히 헛돈다. 줄 번호 좌표를 가장 많이 적는 곳은 원장이
    아니라 `passes/` 의 판정 상세이고, 디렉터리로 이전하기 전 R6 는 단일 파일 전체(본문 +
    원장 + 상세)를 덮고 있었다 — 여기서 원장으로 좁히면 이전 자체가 강제력을 줄인다.
    그 축소는 **좁아진 범위 안에 위반이 없는 한 rc=0** 이라 이 게이트에도 CI 에도 잡히지
    않으므로, 범위를 넓히는 것과 함께 그 크기를 출력에 싣는다(아래 `main`).
    """
    docs = sorted(POLICY_DIR.rglob("*.md"))
    if not docs:
        raise SystemExit(
            f"정책 디렉터리에 마크다운이 하나도 없다: {POLICY_DIR.relative_to(REPO_ROOT)}"
            " — R6 가 아무것도 스캔하지 못하는 상태는 '위반 0' 으로 보여 초록으로 새어 나간다."
        )
    return docs


def repo_exclude_pattern() -> re.Pattern[str]:
    """대상 레포 고유 제외 — 정책 본문의 ```comment-scope-exclude 블록(한 줄에 ERE 하나)."""
    if not POLICY_README.exists():
        return re.compile(r"(?!)")
    text = POLICY_README.read_text(encoding="utf-8")
    match = REPO_EXCLUDE_BLOCK_RE.search(text)
    if not match:
        return re.compile(r"(?!)")
    lines = [ln.strip() for ln in match.group(1).splitlines() if ln.strip()]
    if not lines:
        return re.compile(r"(?!)")
    try:
        return re.compile("|".join(lines))
    except re.error as exc:
        raise SystemExit(f"{POLICY_README.name}: comment-scope-exclude 블록의 ERE가 깨졌다 — {exc}") from exc


def language_group(rel: str) -> tuple[str, re.Pattern[str] | None] | None:
    """파일명이 속하는 언어군 하나(위에서부터 처음 맞는 군). 어떤 군에도 안 들면 None(미분류)."""
    for name, files_re, pattern in LANGUAGE_GROUPS:
        if files_re.search(rel):
            return name, pattern
    return None


def scan_files() -> tuple[dict[str, re.Pattern[str]], list[str]]:
    """모델 as-is 지문과 동일한 스캔 범위.

    반환: (스캔 대상 파일 → 주석 줄 패턴, 미분류 파일 경로). NONE 언어군(③ 주석 문법 없음)은
    스캔하지 않는다 — 파일 자체는 제외되지만 지문에도 주석 줄이 없다.
    """
    exclude = repo_exclude_pattern()
    out = subprocess.run(
        ["git", "ls-files"], cwd=REPO_ROOT, capture_output=True, text=True, check=True
    ).stdout
    paths = [ln for ln in out.splitlines() if ln and not SCAN_EXCLUDE_RE.search(ln) and not exclude.search(ln)]
    if not paths:
        raise SystemExit(
            "스캔 범위가 비어 있다 — git ls-files 가 아무 파일도 돌려주지 않았다. "
            "레포 루트에서 실행 중인지 확인할 것."
        )
    # ② 편집 불가 — 머리 5줄에 생성 표식이 있는 파일은 지문·판정에서 뺀다.
    kept: list[str] = []
    for rel in sorted(paths):
        try:
            head = "".join((REPO_ROOT / rel).read_text(encoding="utf-8").splitlines(keepends=True)[:5])
        except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError, OSError):
            head = ""
        if GENERATED_MARKER_RE.search(head):
            continue
        kept.append(rel)
    scanned: dict[str, re.Pattern[str]] = {}
    unclassified: list[str] = []
    for rel in kept:
        group = language_group(rel)
        if group is None:
            unclassified.append(rel)  # 미분류 — 경로가 지문에 들어가므로 생기거나 사라지면 감지가 돈다
            continue
        name, pattern = group
        if pattern is None:  # NONE 언어군 — 주석 문법이 없으므로 스캔하지 않는다
            continue
        scanned[rel] = pattern
    return scanned, unclassified


def comment_lines(paths: list[str], scanned: dict[str, re.Pattern[str]]) -> list[str]:
    """`경로:주석` 줄의 정규화·정렬 목록. as-is 지문과 같은 구성이다.

    지시어 주석(기계가 읽는 것)은 제외한다. 판정 대상이 아니기 때문이다.
    """
    hits: list[str] = []
    for rel in sorted(paths):
        pattern = scanned.get(rel)
        if pattern is None:
            continue
        try:
            text = (REPO_ROOT / rel).read_text(encoding="utf-8")
        except (UnicodeDecodeError, FileNotFoundError, IsADirectoryError, OSError):
            continue
        for line in text.splitlines():
            if not pattern.match(line):
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
        raise SystemExit(f"{LEDGER.name}: 마커({open_marker} … )를 찾지 못했다.")
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
        if len(cells) != 6:
            raise SystemExit(f"{LEDGER.name}: 판정 이력 행의 열 수가 6가 아니다 -> {line}")
        count = cells[2].strip("`")
        if not count.isdigit():
            raise SystemExit(f"{LEDGER.name}: 주석 줄 수가 정수가 아니다 ({count!r}).")
        rows.append(
            {
                # 같은 날 두 범위를 판정하는 일이 흔하므로 날짜만으로는 행을 못 가리킨다.
                "date": f"{cells[0]}(#{len(rows) + 1})",
                "paths": [m.group(1) for m in BACKTICKED_RE.finditer(cells[1])],
                "lines": int(count),
                "fingerprint": cells[3].strip("`"),
                "judgment": cells[4].strip("`"),
                "row": line,
            }
        )
    if not rows:
        raise SystemExit(f"{LEDGER.name}: 판정 이력에서 행을 하나도 읽지 못했다(파싱 실패).")
    return rows


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
    for required in (POLICY_DIR, POLICY_README, LEDGER):
        if not required.exists():
            what = "정책 디렉터리" if required is POLICY_DIR else (
                "정책 본문" if required is POLICY_README else "판정 원장")
            print(f"[R1] {what} 가 없다: {required.relative_to(REPO_ROOT)}", file=sys.stderr)
            return 1

    text = LEDGER.read_text(encoding="utf-8")
    rows = parse_ledger(text)

    scanned, unclassified = scan_files()
    in_scope = set(scanned)
    # 모델 as-is 지문의 `files=` 값과 같은 집합 — 주석 줄이 하나라도 있는 파일만 판정 대상이다.
    comment_files = {h.split(":", 1)[0] for h in comment_lines(sorted(scanned), scanned)}

    # R1 — 범위의 실재와 유효성
    for row in rows:
        if not row["paths"]:
            fail("R1", f"{row['date']} 행에 범위 파일이 선언돼 있지 않다(빈 범위는 등재가 아니다).")
        for rel in row["paths"]:
            if rel not in in_scope:
                fail(
                    "R1",
                    f"{row['date']} 행의 `{rel}` 이 정책 스캔 범위에 없다"
                    " (범위: 레포 전체 − 제외). 파일이 사라졌거나 이름이 바뀌었으면"
                    " 그 범위는 다시 판정받아야 한다.",
                )

    # R3 — 범위 간 이중 계상
    seen: dict[str, str] = {}
    for row in rows:
        for rel in row["paths"]:
            if rel in seen:
                fail("R3", f"`{rel}` 이 {seen[rel]} 행과 {row['date']} 행에 모두 등재돼 있다.")
            else:
                seen[rel] = row["date"]

    # R4 — 전수(합집합 == 실측 파일 집합). 빠진 파일의 주석은 영원히 미판정으로 남는다.
    missing = sorted(comment_files - set(seen))
    if missing:
        fail(
            "R4",
            f"원장이 판정 대상 파일 {len(missing)}개를 담지 않았다 (예: {', '.join(missing[:5])}"
            f"{' …' if len(missing) > 5 else ''}). 등재가 빠진 파일의 주석은 판정받지 않고"
            " 미판정으로 남는다 — 파일별로 행을 만들어 등재할 것.",
        )

    # R2 — 등재값 ↔ 재측정
    for row in rows:
        present = [rel for rel in row["paths"] if rel in in_scope]
        hits = comment_lines(present, scanned)
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

    # R5 — 판정 칸 표기 유효성
    for row in rows:
        if row["judgment"] not in JUDGED_MARKERS:
            fail(
                "R5",
                f"{row['date']} 행의 판정 칸이 `{row['judgment']}` 이다."
                " 유효한 표기는 `완료` 또는 `—` 뿐이다 — 옛 기준의 표기(①②③④ 등)는"
                " 필요성 기준이 바뀐 뒤의 판정 완료가 아니다.",
            )

    # R6 — 줄 번호 좌표 금지(정책 디렉터리의 모든 마크다운)
    r6_docs = policy_docs()
    r6_lines = 0
    for doc in r6_docs:
        rel = doc.relative_to(REPO_ROOT)
        lines = doc.read_text(encoding="utf-8").splitlines()
        r6_lines += len(lines)
        for lineno, line in enumerate(lines, 1):
            for hit in LINE_COORDINATE_RE.finditer(line):
                fail(
                    "R6",
                    f"{rel} {lineno} 번째 줄에 줄 번호 좌표 `{hit.group(0)}` 이 있다."
                    " 줄 번호는 다음 판정이 그 파일을 건드리는 순간 밀려나고 어떤 규칙도 그것을"
                    " 재측정하지 않는다 — 심볼 이름이나 AC 번호로 적을 것.",
                )

    hits = comment_lines(sorted(in_scope), scanned)
    if failures:
        for line in failures:
            print(line, file=sys.stderr)
        print(f"\nFAIL: {len(failures)}건 — {census(hits)}", file=sys.stderr)
        return 1

    judged_lines = sum(row["lines"] for row in rows if row["judgment"] == "완료")
    judged_rows = sum(1 for row in rows if row["judgment"] == "완료")
    pending_lines = sum(row["lines"] for row in rows if row["judgment"] != "완료")
    pending_rows = sum(1 for row in rows if row["judgment"] != "완료")
    share = (judged_lines * 100.0 / len(hits)) if hits else 0.0
    print(
        f"OK: 규칙 R1~R6 위반 없음 — {census(hits)}"
        f" · 판정 완료 {judged_lines}줄({share:.1f}%) / {judged_rows}행"
        f" · 미판정 {pending_lines}줄 / {pending_rows}행"
        f" · 등재 파일 {len(seen)} / 판정 대상 파일 {len(comment_files)}"
        # R6 가 무엇을 덮었는지 함께 싣는다. 스캔 범위가 줄어드는 사고는 좁아진 범위 안에
        # 위반이 없는 한 rc=0 이라 초록으로 새어 나가므로, 크기를 눈에 보이게 두는 것이
        # 이 게이트가 자기 강제력의 축소를 알리는 유일한 수단이다.
        f" · R6 스캔 {len(r6_docs)}파일 {r6_lines}줄"
        f" · 미분류 {len(unclassified)}파일"
    )
    for row in rows:
        print(
            f"  {row['date']}  {row['lines']:>4}줄  {row['fingerprint']}  [{row['judgment']}]"
            f"  {' '.join(row['paths'])}"
        )
    print(f"  전체 지문: {fingerprint(hits)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
