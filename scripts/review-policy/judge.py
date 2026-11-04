import argparse
import fnmatch
import re
import sys

POLICY = "docs/review-policy.md"

CASES = (
    ("MA1", ('docs/values.md', 'docs/prd/', 'docs/test/', 'docs/user-journeys/'), ()),
    ("MA2", ('scripts/review-policy/', '.github/workflows/review-policy.yml', 'docs/review-policy.md'), ()),
    ("MA3", ('k8s/rbac.yaml', 'k8s/data-plane-rbac.yaml', 'k8s/session-mcp-serviceaccount.yaml', 'k8s/network-policy.yaml'), ()),
    ("MA4", ('control-plane/internal/repoaccess/', 'control-plane/internal/adapter/configmap/', 'control-plane/internal/adapter/checkpointstore/', 'control-plane/internal/adapter/archive/', 'control-plane/internal/session/session.go', 'data-plane/cmd/agent/state_archive.go', 'data-plane/cmd/agent/checkpoint.go'), ('*_test.go', 'control-plane/internal/adapter/configmap/envtest/')),
    ("MA5", ('control-plane/internal/helperauth/', 'control-plane/cmd/control-plane/repo_access.go', 'control-plane/internal/api/repo_access.go', 'data-plane/cmd/agent/credential_proxy*.go', 'data-plane/cmd/agent/k3s_mcp_proxy.go', 'data-plane/cmd/agent/k3s_mcp_tool_deny.go', 'data-plane/cmd/agent/session_mcp_git_push.go', 'data-plane/cmd/agent/session_mcp_repo_access.go', 'data-plane/cmd/agent/minimax_marketplace.go', 'data-plane/entrypoint.sh', 'control-plane/internal/adapter/k8s/', 'data-plane/cmd/agent/approval_gateway.go', 'data-plane/cmd/agent/approval_wait.go', 'data-plane/cmd/agent/session_mcp_tools.go', 'data-plane/Dockerfile', 'control-plane/Dockerfile'), ('*_test.go',)),
)


def policy_ids(path):
    with open(path, encoding="utf-8") as f:
        return {m.group(1) for m in re.finditer(r"^\| (MA[0-9]+) \|", f.read(), re.M)}


def matches(path, pattern):
    if "*" in pattern:
        return fnmatch.fnmatchcase(path, pattern)
    return path == pattern or (pattern.endswith("/") and path.startswith(pattern))


def touched(paths):
    hits = {}
    for p in paths:
        for case_id, targets, excludes in CASES:
            if any(matches(p, t) for t in targets) and not any(matches(p, x) for x in excludes):
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
    own = {case_id for case_id, _, _ in CASES}
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
