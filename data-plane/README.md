# data-plane

The data plane is where actual session workloads run — one dedicated pod per
session (AC-A2). The control plane provisions and reclaims these pods via the
`PodOrchestrator` port.

## Status: workload defined (spec) · image placeholder

The concrete "session workload" runtime is now **defined**: a session is an
**interactive shell** (default `/bin/bash`) attached to a PTY, running inside the
session's dedicated pod. See `../docs/prd/shell-workload.md` (AC-D1~D5) and value
V6 in `../docs/values.md`. Read/write map onto the shell: write = stdin input,
read = accumulated stdout/stderr output.

This directory is the home for:

- the data plane container image (base image + PTY-attached shell agent),
- the pod template / manifest the orchestrator instantiates,
- the CRIU-capable runtime configuration (see `../docs/criu-verification.md`).

The concrete agent image that launches the PTY-attached shell is **not yet
built** — until it exists, the control plane's client-go orchestrator
(`control-plane/internal/adapter/k8s`) provisions a **placeholder pod** from a
generic image (`alpine:3.20`, overridable via `DATA_PLANE_IMAGE`) that just stays
running so the pod reports Ready. This proves the 1:1 session↔pod lifecycle
(create/Ready/reclaim, AC-A1/A2/A3) end-to-end without the real session agent.
The control plane talks to a cluster either via its in-cluster config (as a pod)
or the ambient kubeconfig (local development against a kind cluster).
