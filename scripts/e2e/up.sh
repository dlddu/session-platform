#!/usr/bin/env bash
# Bring up the kind-based e2e SUT: a *deployed* control-plane, reachable at
# http://localhost:8080 through the NodePort extraPortMapping (host :8080 ->
# node :30080, see deploy/kind-config.yaml). Session state lives in ConfigMaps +
# Leases in the cluster, so there is no separate state-store deployment to wait
# on.
#
#   scripts/e2e/up.sh        # create cluster (if needed) + build + load + deploy
#
# Idempotent: if the cluster already exists (e.g. created by helm/kind-action in
# CI) the create step is skipped and only build/load/deploy/wait run. Pair with
# scripts/e2e/down.sh to reclaim the cluster.
set -euo pipefail

CLUSTER="${KIND_CLUSTER:-session-platform}"
IMAGE="${CP_IMAGE:-session-platform/control-plane:dev}"
# Data plane session agent image (PTY shell + attach/healthz, AC-D1) the
# control plane provisions per session. Built locally and loaded into kind so
# pods come up without a per-pod registry pull (pullPolicy IfNotPresent). Keep
# in sync with the deploy/ overlay's DATA_PLANE_IMAGE patch.
DP_IMAGE="${DATA_PLANE_IMAGE:-session-platform/data-plane:dev}"
BASE_URL="${E2E_BASE_URL:-http://localhost:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "e2e: missing required tool: $1" >&2
    exit 1
  }
}
require kind
require kubectl
require docker

cd "$ROOT"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "e2e: kind cluster '$CLUSTER' already exists — skipping create"
else
  echo "e2e: creating kind cluster '$CLUSTER'"
  kind create cluster --config deploy/kind-config.yaml
fi

echo "e2e: building image $IMAGE"
make docker

echo "e2e: loading image into kind cluster '$CLUSTER'"
kind load docker-image "$IMAGE" --name "$CLUSTER"

if [ "$DP_IMAGE" = "session-platform/data-plane:dev" ]; then
  echo "e2e: building data plane agent image $DP_IMAGE"
  make docker-data-plane
else
  echo "e2e: ensuring data plane image '$DP_IMAGE' is present locally"
  docker image inspect "$DP_IMAGE" >/dev/null 2>&1 || docker pull "$DP_IMAGE"
fi

echo "e2e: loading data plane image into kind cluster '$CLUSTER'"
kind load docker-image "$DP_IMAGE" --name "$CLUSTER"

echo "e2e: applying deploy/ overlay (kustomize: base k8s/ + kind patches)"
kubectl apply -k deploy/

echo "e2e: verifying data-plane read-only RBAC excludes Secrets"
DATA_PLANE_USER="system:serviceaccount:default:data-plane"
kubectl auth can-i get pods --all-namespaces --as="$DATA_PLANE_USER" >/dev/null
if kubectl auth can-i get secrets --all-namespaces --as="$DATA_PLANE_USER" >/dev/null; then
  echo "e2e: data-plane ServiceAccount unexpectedly has Secret read access" >&2
  exit 1
fi

echo "e2e: waiting for rollouts"
# MinIO backs the CRIU checkpoint archives (deploy/minio.yaml). Wait for it too,
# so the first snapshot cannot race its startup or bucket creation.
kubectl rollout status deploy/minio --timeout=180s
kubectl rollout status deploy/control-plane --timeout=120s

echo "e2e: polling $BASE_URL/api/v1/healthz"
for _ in $(seq 1 60); do
  if curl -fsS "$BASE_URL/api/v1/healthz" >/dev/null 2>&1; then
    echo "e2e: SUT healthy at $BASE_URL"
    exit 0
  fi
  sleep 2
done

echo "e2e: SUT did not become healthy at $BASE_URL within timeout" >&2
kubectl get pods -o wide || true
exit 1
