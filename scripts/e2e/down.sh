#!/usr/bin/env bash
# Tear down the kind-based e2e SUT created by scripts/e2e/up.sh.
set -euo pipefail

CLUSTER="${KIND_CLUSTER:-session-platform}"

if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "e2e: deleting kind cluster '$CLUSTER'"
  kind delete cluster --name "$CLUSTER"
else
  echo "e2e: kind cluster '$CLUSTER' not present — nothing to do"
fi
