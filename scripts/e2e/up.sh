#!/usr/bin/env bash
# Bring up the kind-based e2e SUT: a *deployed* control-plane, reachable at
# http://localhost:8080 through the NodePort extraPortMapping (host :8080 ->
# node :30080, see deploy/kind-config.yaml). Session state lives in ConfigMaps +
# Leases in the cluster, so there is no separate state-store deployment to wait
# on.
#
#   scripts/e2e/up.sh                     # create cluster (if needed) + build + load + deploy
#   SKIP_IMAGE_BUILD=1 scripts/e2e/up.sh  # same, but images are already built locally
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

# SKIP_IMAGE_BUILD=1: both images are already in the local docker image store
# (CI builds them with buildx and its layer cache, .github/workflows/e2e.yml),
# so only check they exist and load them. Unset, this script builds them itself.
SKIP_BUILD="${SKIP_IMAGE_BUILD:-0}"

if [ "$SKIP_BUILD" = 1 ]; then
  echo "e2e: SKIP_IMAGE_BUILD=1 — using prebuilt $IMAGE"
  docker image inspect "$IMAGE" >/dev/null
else
  echo "e2e: building image $IMAGE"
  make docker
fi

echo "e2e: loading image into kind cluster '$CLUSTER'"
kind load docker-image "$IMAGE" --name "$CLUSTER"

if [ "$SKIP_BUILD" = 1 ]; then
  echo "e2e: SKIP_IMAGE_BUILD=1 — using prebuilt $DP_IMAGE"
  docker image inspect "$DP_IMAGE" >/dev/null
elif [ "$DP_IMAGE" = "session-platform/data-plane:dev" ]; then
  echo "e2e: building data plane agent image $DP_IMAGE"
  make docker-data-plane
else
  echo "e2e: ensuring data plane image '$DP_IMAGE' is present locally"
  docker image inspect "$DP_IMAGE" >/dev/null 2>&1 || docker pull "$DP_IMAGE"
fi

echo "e2e: loading data plane image into kind cluster '$CLUSTER'"
kind load docker-image "$DP_IMAGE" --name "$CLUSTER"

# The approval gateway is the real PocketAide (deploy/e2e-pocket-aide.yaml).
# Its published image carries the server only, not the IdP the harness logs in
# through, so both are built here from one commit. POCKET_AIDE_SHA is the only
# pin of that dependency: move it when PocketAide's external approval API or
# its user API changes.
POCKET_AIDE_SHA="${POCKET_AIDE_SHA:-f50b2463aca6904ec861bd91adf3be304824e832}"
require go
require git
PA_SRC="$(mktemp -d)"
PA_CTX="$(mktemp -d)"
trap 'rm -rf "$PA_SRC" "$PA_CTX"' EXIT
echo "e2e: building PocketAide server + oidcmock at $POCKET_AIDE_SHA"
git init -q "$PA_SRC"
git -C "$PA_SRC" fetch -q --depth 1 https://github.com/dlddu/pocket-aide "$POCKET_AIDE_SHA"
git -C "$PA_SRC" checkout -q FETCH_HEAD
test "$(git -C "$PA_SRC" rev-parse HEAD)" = "$POCKET_AIDE_SHA"
(
  cd "$PA_SRC/backend"
  CGO_ENABLED=0 go build -ldflags="-s -w" -o "$PA_CTX/pocket-aide" ./cmd/server
  CGO_ENABLED=0 go build -ldflags="-s -w" -o "$PA_CTX/oidcmock" ./cmd/oidcmock
)
docker build -q -f scripts/e2e/pocket-aide.Dockerfile --build-arg BINARY=pocket-aide \
  -t pocket-aide:e2e "$PA_CTX" >/dev/null
docker build -q -f scripts/e2e/pocket-aide.Dockerfile --build-arg BINARY=oidcmock \
  -t oidc-mock:e2e "$PA_CTX" >/dev/null
kind load docker-image pocket-aide:e2e oidc-mock:e2e --name "$CLUSTER"

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
# The two endpoints a claude-code session pod contacts during its plugin
# bootstrap, before its agent starts. Waiting here for the same reason as MinIO:
# the first claude-code session must not race their startup (the marketplace
# remote also has to finish seeding its bare repo in an init container).
kubectl rollout status deploy/k3s-mcp-fake --timeout=120s
kubectl rollout status deploy/plugin-marketplace-git --timeout=180s
# The provider stand-in a claude-code session talks to once it is running. Same
# reason again: the first prompt must not race its TLS listener coming up.
kubectl rollout status deploy/anthropic-fake --timeout=120s
# The approval gateway an approval-gated session's helper pod calls before every
# external tool call. Same reason once more: the first gated call must not race
# its listener. The IdP first — PocketAide fetches its discovery document at
# start and exits if it cannot, so it may restart once before going Ready.
kubectl -n oidc-mock rollout status deploy/oidc-mock --timeout=120s
kubectl -n pocket-aide rollout status deploy/pocket-aide --timeout=180s
# The origin an approved external tool call fetches (deploy/fetch-origin.yaml).
# Same reason once more: the approval round trip ends in a real GET, and the
# first one must not race this listener — a refused connection there would look
# like the tool failing rather than the origin still starting.
kubectl rollout status deploy/fetch-origin --timeout=120s
# The ReadWriteMany provisioner behind AC-F5's session-shared volume
# (deploy/shared-volume-provisioner.yaml). Same reason as the fixtures above:
# the first approval-gated session must not race it. Without the class its
# claim never binds and the session hangs instead of failing, so the existence
# check below turns a 90-second mystery into one line.
kubectl -n session-shared-storage rollout status deploy/local-path-provisioner --timeout=120s
if ! kubectl get storageclass session-shared >/dev/null 2>&1; then
  echo "e2e: StorageClass 'session-shared' is missing — AC-F5's shared volume cannot be provisioned" >&2
  exit 1
fi
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
