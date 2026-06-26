# data-plane

The data plane is where actual session workloads run — one dedicated pod per
session (AC-A2). The control plane provisions and reclaims these pods via the
`PodOrchestrator` port.

## Status: placeholder

The concrete "session workload" runtime is **not yet defined** (out of scope for
the bootstrap scaffolding). This directory is a placeholder for:

- the data plane container image (base image + session agent),
- the pod template / manifest the orchestrator instantiates,
- the CRIU-capable runtime configuration (see `../docs/criu-verification.md`).

Until the real workload image exists, the control plane's client-go orchestrator
(`control-plane/internal/adapter/k8s`) provisions a **placeholder pod** from a
generic image (`alpine:3.20`, overridable via `DATA_PLANE_IMAGE`) that just stays
running so the pod reports Ready. This proves the 1:1 session↔pod lifecycle
(create/Ready/reclaim, AC-A1/A2/A3) end-to-end without the real session agent.
When run outside a cluster (local `make run`), it falls back to an in-memory stub
so the happy path runs without a cluster.
