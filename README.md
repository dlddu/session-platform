# session-platform

Per-session pod platform: each session runs in its own dedicated data plane
pod as either an interactive shell or a one-shot Claude Code agent. Idle
sessions can return compute through a workload-specific snapshot, and a control
plane is the single entry point for creating, listing, operating, and switching
between sessions.

> Pod orchestration (client-go), shared session state (Kubernetes ConfigMaps +
> Leases), both data plane modes, and the SPA are implemented. Shell sessions
> map read/write to PTY stdout/stdin (J5, AC-D1~D3). Claude Code sessions queue
> prompts through one serial worker, retain their CLI home/workspace, expose the
> same offset-cursor read contract, and can archive filesystem state without
> CRIU (J6, AC-E1~E6). Each invocation is bounded to 16 MiB and cumulative Claude
> output to 256 MiB with explicit terminal markers; the full state and cursors
> survive archive restore. Shell CRIU is agent-driven behind `CRIU_ENABLED`; Claude
> archives are separately gated by `CLAUDE_CODE_ARCHIVE_ENABLED` because they
> transfer workspace, conversation, and output data to external storage. Both
> gates default off in production. A reaper candidate whose workload gate is off
> stays live and an explicit snapshot returns service unavailable: the platform
> never deletes a pod behind synthetic checkpoint metadata. The plain 60-minute reaper exists;
> finer policy such as grace periods and busy-shell handling remains open. See
> the design docs under [`docs/`](docs/) for the
> value/PRD/AC and mockups this is built from. The **design system** — tokens,
> primitives, and components — lives in code at
> [`web/src/design/`](web/src/design/README.md) (+ `web/src/app/shell.css`);
> that directory is the source of truth for anything visual.

## Layout

```
control-plane/        Go: REST API + orchestration/state adapters + SPA serving
  api/openapi.yaml      OpenAPI spec for the /api/v1 surface
  cmd/control-plane/    main: wires adapters, serves API + embedded SPA on one port
  internal/
    session/            domain: Session entity, State enum, Manager port
    store/              StateStore port (backend-neutral)
    service/            concrete Manager wiring the adapters (happy path)
    adapter/k8s/        PodOrchestrator: client-go pod lifecycle (real)
    adapter/configmap/  StateStore: ConfigMap state + Lease locks via client-go (real)
      envtest/          isolated module: real-apiserver CAS/Lease conflict suite
    adapter/criu/       Checkpointer port + agent-driven adapter + gate-off stub
    api/                REST handlers (thin) + tests
    static/             embeds web/dist and serves the SPA
  Dockerfile            multi-stage: build SPA -> embed in Go -> minimal image
web/                  React + Vite + TS SPA
  src/design/          canonical design system (README.md indexes it; code is the source of truth)
    tokens.css          design tokens + base primitives; mockups hold inline copies of these values
  src/app/shell.css     component/pattern layer of the design system
  src/app/              AppShell (rail + viewport), StateBadge
  src/screens/          Sessions, NewSession, Workspace, Restore
  src/api/              typed client over /api/v1
data-plane/           multi-workload agent selected by DATA_PLANE_WORKLOAD
  cmd/agent/            PTY shell, Claude runner/archive, and credential proxy
  Dockerfile            agent + CRIU 4.2 + Claude Code CLI runtime
deploy/               kind config + control-plane manifests (2-replica e2e overlay)
docs/                 value / PRD·AC / journeys / mockups / CRIU verification note
```

## Architecture

- **Control plane / data plane split** (AC-A1): the control plane orchestrates;
  workloads run only in data plane pods. One dedicated pod per session (AC-A2).
- **State model** `active | idle | snapshot` stored in per-session ConfigMaps,
  with resourceVersion compare-and-swap for whole-aggregate transitions and
  renewable `coordination.k8s.io` Leases for occupancy locks (AC-C1) — shared
  across control-plane replicas. Long Snapshot/Restore/recovery operations renew
  the default 15-second Lease every 5 seconds; private snapshot transactions are
  owner-fenced. Read/Write/Switch dispatch on state (AC-C2/C3/C4).
- **Workloads**: `shell` (default) runs one PTY shell; `claude-code` runs one
  CLI process per prompt through a bounded serial queue. Type and Claude model
  are immutable session metadata. Real provider credentials exist only in a
  hardened localhost proxy sidecar that accepts only a configured HTTPS upstream;
  the tool-running container receives a
  non-secret placeholder and no Kubernetes service-account token.
- **Lifecycle**: 60-min max idle → workload snapshot + pod reclaim (AC-B1);
  access → restore into a new pod (AC-B2). Shell uses CRIU behind
  `CRIU_ENABLED`; Claude uses a filesystem archive behind the independent,
  explicit `CLAUDE_CODE_ARCHIVE_ENABLED` data-transfer gate. A disabled gate
  returns a service-unavailable snapshot error and preserves the live pod.
  Claude archives use a CP-owned generation and durable owner-fenced
  `preparing→committing` transaction: prepare failures abort only that generation,
  while committing recovery keeps admission closed and retries Stop/finalization.
- **Single entry point**: the control plane container serves both the REST API
  (`/api/v1`) and the statically built SPA on one port. JSON POST bodies have an
  8 MiB wire limit and a 30-second read timeout; Claude prompts also have the
  workload-specific 1 MiB decoded-payload limit.

The reaper scans the PRD's plain “last read/write was at least 60 minutes ago”
boundary and rechecks authoritative `lastAccess` under the lifecycle Lease.
Read/write do not hold that Lease and their `Touch` is best-effort, so an access
finishing between the recheck and checkpoint is a tracked freshness race.
Two crash windows are also tracked: a hard crash after `RestoreInto` succeeds
but before final CAS can orphan the restore-target pod, and shell CRIU has no
durable rollback/reconcile protocol after a successful dump followed by
upload/Stop/final-metadata failure. The Claude archive prepare/commit protocol
does not close those workload-independent/shell-specific gaps.
`TODO(policy: ...)` also marks finer product choices such as grace periods,
per-session overrides, and whether a client-idle shell with a long-running
foreground job should freeze.

## Prerequisites

Go 1.24+, Node 22+, (optional, for the image build & e2e) Docker, kind, kubectl.

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

- **Unit** (`make test-unit`): both Go modules — API/service/store/orchestrator
  adapters plus the shell/Claude/archive/credential-proxy agents. Claude tests
  use reduced deterministic limits to exercise the production output-boundary
  logic without allocating 256 MiB.
- **Integration** (`make test-integration`, build tag `integration`): the
  happy-path scenarios from `docs/test/architecture.md` driven **in-process**
  (handlers mounted in a test server) with the stub adapters.
- **E2E** (kind-deployed SUT): builds the combined image, loads it into a kind
  cluster (`deploy/`, 2 control-plane replicas), and runs a Go API suite + a
  Playwright browser suite against the deployed control-plane (reachable at
  `http://localhost:8080` via a NodePort). Covers create/list/get/switch·read·write,
  real-pod provisioning (AC-A1/A2), and cross-replica state consistency over the
  shared ConfigMap store (AC-C1). With CRIU turned on in the overlay, the
  snapshot → reclaim → restore round trip is a verified assertion too
  (`TestDeferred_CRIUIntegrity`, AC-B2/B3/D4). J6 browser contract coverage
  uses deterministic Playwright route fixtures, while the Claude worker,
  cursor, resume, output limits, redaction, archive round trip, and lifecycle
  crash boundaries use fake-runner/adapter Go tests.
  A deployed test against the external Claude API is intentionally not claimed.
  The seeded idle-state cases still lack an operational producer for the
  intermediate `idle` state. Details and the deferred-seed ↔ scenario map:
  [`docs/test/e2e.md`](docs/test/e2e.md).
- **Conflict (envtest)** (`make test-envtest`): an isolated nested module runs
  the ConfigMap adapter against a real kube-apiserver + etcd to assert AC-C1's
  single-winner property (exactly one of N concurrent CompareAndSwap / Lease
  acquisitions wins). controller-runtime stays out of the main module's deps.

  ```bash
  make e2e-up                                          # kind + build + deploy, SUT on :8080
  (cd control-plane && go test -tags=e2e ./test/...)   # API e2e
  (cd web && npx playwright test)                      # browser e2e (J1, J3, J5, J6, smoke)
  make e2e-down                                         # tear down
  ```

## CI

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs lint + unit (Go),
typecheck + build (web), the in-process integration harness, the real-apiserver
envtest conflict suite, and both runtime image builds on every PR.

[`.github/workflows/e2e.yml`](.github/workflows/e2e.yml) runs the kind-based
e2e suites (Go API + Playwright) on PRs touching `control-plane/`, `data-plane/`,
`web/`, `deploy/`, `k8s/`, `scripts/e2e/`, `Makefile`, or that workflow itself,
and on demand (`workflow_dispatch`);
Playwright reports/traces upload as artifacts.

## Deployment

The cluster runs this via GitOps (Flux) from the `flux-cd-apps` repo.

- [`.github/workflows/docker-build-push.yaml`](.github/workflows/docker-build-push.yaml)
  publishes the combined control-plane image as
  `ghcr.io/dlddu/session-platform:{latest,sha}` and the multi-workload agent as
  `ghcr.io/dlddu/session-platform-data-plane:{latest,sha}` on pushes to `main`
  (and builds/pushes for matching pull requests) that touch `control-plane/`,
  `data-plane/`, `web/`, or the workflow itself.
- [`k8s/`](k8s/) holds the cluster manifests Flux applies: the `control-plane`
  Deployment + Service (port 80 → 8080) and its RBAC (pods + configmaps +
  leases). Session state lives in in-cluster ConfigMaps/Leases, so there is no
  separate backing-store deployment. The namespace, ingress, and VPA live on the
  cluster side in `flux-cd-apps`.

The [`deploy/`](deploy/) directory remains the local `kind` setup for the
integration harness; `k8s/` is the deployed-cluster source of truth.
