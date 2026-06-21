# session-platform

Per-session pod platform: each session runs in its own dedicated data plane
pod, idle sessions freeze to a CRIU checkpoint to hand back compute, and a
control plane is the single entry point for creating, listing, and switching
between sessions.

> **This repository is a bootstrap scaffolding.** Structure, dependencies,
> boundaries, and the dev loop are in place; domain logic (real pod
> orchestration, Redis atomics, CRIU) is stubbed behind interfaces. See the
> design docs under [`docs/`](docs/) for the value/PRD/AC and mockups this is
> built from.

## Layout

```
control-plane/        Go: REST API + orchestration adapters (stub) + SPA serving
  api/openapi.yaml      OpenAPI spec for the /api/v1 surface
  cmd/control-plane/    main: wires adapters, serves API + embedded SPA on one port
  internal/
    session/            domain: Session entity, State enum, Manager port
    service/            concrete Manager wiring the adapters (happy path)
    adapter/k8s/        PodOrchestrator port + in-memory stub  (client-go later)
    adapter/redis/      StateStore port + in-memory stub       (go-redis later)
    adapter/criu/       Checkpointer port + gated stub         (CRIU later)
    api/                REST handlers (thin) + tests
    static/             embeds web/dist and serves the SPA
  Dockerfile            multi-stage: build SPA -> embed in Go -> minimal image
web/                  React + Vite + TS SPA
  src/design/tokens.css design tokens ported 1:1 from the mockups
  src/app/              AppShell (rail + viewport), StateBadge
  src/screens/          Sessions, NewSession, Workspace, Restore
  src/api/              typed client over /api/v1
data-plane/           placeholder for the session workload runtime
deploy/               kind config + Redis & control-plane manifests
docs/                 value / PRD·AC / journeys / mockups / CRIU verification note
```

## Architecture

- **Control plane / data plane split** (AC-A1): the control plane orchestrates;
  workloads run only in data plane pods. One dedicated pod per session (AC-A2).
- **State model** `active | idle | snapshot` held in Redis with atomic
  transitions (AC-C1). Read/Write/Switch dispatch on state (AC-C2/C3/C4).
- **Lifecycle**: 60-min max idle → CRIU snapshot + pod reclaim (AC-B1);
  access → restore into a new pod (AC-B2). CRIU is **gated** (`CRIU_ENABLED`)
  and stubbed; see [`docs/criu-verification.md`](docs/criu-verification.md).
- **Single entry point**: the control plane container serves both the REST API
  (`/api/v1`) and the statically built SPA on one port.

Unresolved product policy is marked in code with `TODO(policy: ...)` (idle/
snapshot read & write behaviour) and `TODO(client-go|go-redis|criu|rbac)` for
the deferred real implementations.

## Prerequisites

Go 1.24+, Node 22+, (optional) Docker, kind.

## Build & run

```bash
make build      # web build -> embed -> control-plane binary
make run        # build then serve API + SPA on http://localhost:8080
make test       # Go unit tests + web typecheck
make dev        # control plane (:8080) + Vite dev server (:5173, proxies /api)
make docker     # single combined API + SPA image
```

`make build` regenerates `control-plane/internal/static/dist/` from the web
build; only the placeholder `index.html` is tracked, the built assets are
gitignored.

## API

`/api/v1`, spec in [`control-plane/api/openapi.yaml`](control-plane/api/openapi.yaml):

| Method + path                | Purpose                        | AC     |
| ---------------------------- | ------------------------------ | ------ |
| `POST /sessions`             | create (provision pod, active) | A1, A2 |
| `GET  /sessions`             | list                           | V5     |
| `GET  /sessions/{id}`        | get one                        | V5     |
| `POST /sessions/{id}/read`   | read (state-branched)          | C2     |
| `POST /sessions/{id}/write`  | write (state-branched)         | C3     |
| `POST /sessions/{id}/switch` | switch (restore if snapshot)   | C4     |

## Testing

- **Unit** (`make test-unit`): API handlers + service manager with stub adapters.
- **Integration** (`make test-integration`, build tag `integration`): happy-path
  scenarios from `docs/test/architecture.md`. The real harness will bring up
  kind + Redis from `deploy/`; CRIU scenarios stay skipped until a verified
  runtime exists.

## CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs lint + unit (Go),
typecheck + build (web), and the combined image build on every PR. The
integration job is opt-in (`workflow_dispatch`).
