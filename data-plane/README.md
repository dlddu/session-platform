# data-plane

The data plane is where actual session workloads run — one dedicated pod per
session (AC-A2). The control plane provisions and reclaims these pods via the
`PodOrchestrator` port.

## Status: shell agent built (J5-S1) · payload semantics pending (J5-S2/S3)

The concrete "session workload" runtime is **defined and running**: a session is
an **interactive shell** (default `/bin/bash`) attached to a PTY, running inside
the session's dedicated pod. See `../docs/prd/shell-workload.md` (AC-D1~D5) and
value V6 in `../docs/values.md`. Read/write map onto the shell: write = stdin
input, read = accumulated stdout/stderr output.

This directory holds the **session agent** (`cmd/agent`) and its image
(`Dockerfile`, debian-slim + static Go binary). The image's ENTRYPOINT owns the
workload: on start the agent launches exactly one PTY-attached interactive shell
(`DATA_PLANE_SHELL`, default `/bin/bash` — AC-D1) and serves two endpoints on
:8090:

- `GET /healthz` — 200 while the shell process is alive; the pod's readiness
  probe targets it, so pod Ready implies a live shell.
- `GET /attach` — the WebSocket attach stream. In J5-S1 it is payload-agnostic
  (open/close only): the control plane opens and closes it at the `active`
  transition to prove the shell is reachable. The stdin/stdout semantics
  (AC-D2/D3) land on top of this endpoint in J5-S2/S3.

The control plane's client-go orchestrator
(`control-plane/internal/adapter/k8s`) provisions each session pod from this
image (`DATA_PLANE_IMAGE`; published by `.github/workflows/docker-build-push.yaml`
as `…-data-plane`) with no command override — and refuses to mark a session
`active` until the attach stream opens. The in-code fallback image remains a
generic `alpine:3.20`, which cannot pass the shell readiness probe: real
deployments must inject `DATA_PLANE_IMAGE` (k8s/deployment.yaml and the e2e
overlay both do). The control plane talks to a cluster either via its in-cluster
config (as a pod) or the ambient kubeconfig (local development against a kind
cluster).

Still future here: the CRIU-capable runtime configuration for checkpoint/restore
(AC-B*/AC-D4, see `../docs/criu-verification.md`).
