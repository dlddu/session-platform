import argparse
import fnmatch
import re
import sys

POLICY = "docs/review-policy.md"

CASES = (
    ("MA1", ("docs/values.md", "docs/prd/", "docs/test/", "docs/user-journeys/")),
    ("MA2", ("docs/review-policy.md", "scripts/review-policy/", ".github/workflows/review-policy.yml")),
)


def policy_ids(path):
    with open(path, encoding="utf-8") as f:
        return {m.group(1) for m in re.finditer(r"^\| (MA[0-9]+) \|", f.read(), re.M)}


def matches(path, target):
    if "*" in target:
        return fnmatch.fnmatchcase(path, target)
    return path == target or (target.endswith("/") and path.startswith(target))


def touched(paths):
    hits = {}
    for p in paths:
        for case_id, targets in CASES:
            if any(matches(p, t) for t in targets):
                hits.setdefault(case_id, []).append(p)
    return hits


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--policy", default=POLICY)
    args = ap.parse_args()
    try:
        declared = policy_ids(args.policy)
    except OSError as e:
        print(f"판정 불가: 정책 문서를 읽지 못했다 — {e}", file=sys.stderr)
        return 2
    own = {case_id for case_id, _ in CASES}
    if declared != own:
        print(f"판정 불가: 정책 케이스 {sorted(declared)} != 판정기 케이스 {sorted(own)}", file=sys.stderr)
        return 2
    paths = sorted({line.strip() for line in sys.stdin if line.strip()})
    if not paths:
        print("판정 불가: 변경 경로가 비어 있다", file=sys.stderr)
        return 2
    hits = touched(paths)
    if not hits:
        print(f"수동 승인 케이스 미접촉 ({len(paths)} 경로)")
        return 0
    for case_id in sorted(hits):
        for p in hits[case_id]:
            print(f"{case_id}\t{p}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
