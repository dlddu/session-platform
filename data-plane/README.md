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

Until then, `control-plane/internal/adapter/k8s` uses an in-memory stub
orchestrator so the create/list/switch happy path runs without real pods.
