# session-platform

Per-session pod platform: each session runs in its own dedicated data plane
pod as either an interactive shell or a one-shot Claude Code agent. Idle
sessions can return compute through a workload-specific snapshot, and a control
plane is the single entry point for creating, listing, operating, and switching
between sessions.

> Pod orchestration (client-go), shared session state (Kubernetes ConfigMaps +
> Leases), both data plane modes, and the SPA are implemented. Shell sessions
> map read/write to PTY stdout/stdin (J5, AC-D1~D3). Claude Code sessions queue
> prompts through one serial worker, retain their CLI home/workspace, and project
> Claude Code `stream-json` text deltas plus diagnostic stderr into an append-only
> output buffer. The SPA receives that buffer live over an offset-cursored SSE
> stream; the existing JSON read remains available for full replay and catch-up.
> Every workload type freezes to a **filesystem archive** of its state directory
> plus its output scrollback (J6, AC-E1~E6). Each invocation is bounded to
> 16 MiB and cumulative Claude output to 256 MiB with explicit terminal markers;
> the full state and cursors survive archive restore. What the archive holds
> differs per type: `shell` keeps only the files in the shell's home directory
> (`/session/state/home`) plus its scrollback — a restored shell is a fresh shell
> started in that home, so running processes, environment and shell variables,
> cwd and open descriptors are not kept (AC-D4); `claude-code` and
> `minimax-code` keep workspace, CLI home, conversation and output, and a type
> behind the approval gate (`minimax-code`) also keeps the contents of the
> session's shared RWX volume (`/shared`, AC-F5 in `docs/prd/approval-gate.md`).
> Each type sits behind its own data-transfer gate
> because the archive leaves the cluster: `SHELL_ARCHIVE_ENABLED`,
> `CLAUDE_CODE_ARCHIVE_ENABLED` and `MINIMAX_CODE_ARCHIVE_ENABLED`. All three default off in code and are turned
> on in the production manifest and the kind e2e overlay. A reaper candidate whose workload gate is off
> stays live and an explicit snapshot returns service unavailable: the platform
> never deletes a pod behind synthetic checkpoint metadata. The plain 60-minute reaper exists;
> finer policy such as grace periods and busy-shell handling remains open. See
> the design docs under [`docs/`](docs/) for the
> value/PRD/AC and mockups this is built from. Those docs are published as a
> portal at
> [`dlddu.github.io/session-platform`](https://dlddu.github.io/session-platform/),
> with the mockups under
> [`/mockups/`](https://dlddu.github.io/session-platform/mockups/).
> The **design system** — tokens,
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
    adapter/archive/    Checkpointer port + agent-driven archive adapter + gate-off stub
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
  cmd/agent/            PTY shell, agent CLI runners, state archive, and credential proxy
  Dockerfile            agent + Claude Code / MiniMax Code CLI runtime (no CRIU)
deploy/
  base/                 production manifests (Deployment, Service, RBAC, NetworkPolicy)
  overlays/prod/        what Flux applies from the `deploy` branch (base + namespace)
  overlays/preview/     what PR preview environments apply (base as is)
  overlays/e2e/         kind config + the kind e2e SUT (2 control-plane replicas, fixtures)
docs/                 value / PRD·AC / journeys / mockups / retired CRIU verification record
```

## Architecture

- **Control plane / data plane split** (AC-A1): the control plane orchestrates;
  workloads run only in data plane pods. One dedicated workload pod per session,
  plus any session-scoped auxiliary pods the workload type needs (AC-A2).
- **State model** `active | idle | snapshot` stored in per-session ConfigMaps,
  with resourceVersion compare-and-swap for whole-aggregate transitions and
  renewable `coordination.k8s.io` Leases for occupancy locks (AC-C1) — shared
  across control-plane replicas. Long Snapshot/Restore/recovery operations renew
  the default 15-second Lease every 5 seconds; private snapshot transactions are
  owner-fenced. Read/Write/Switch dispatch on state (AC-C2/C3/C4).
- **Workloads**: `shell` (default) runs one PTY shell; `claude-code` runs one
  Claude Agent SDK turn per prompt (`claude-runner`, which drives the pinned
  Claude Code CLI) through a bounded serial queue. Each turn resumes the SDK
  session id it recorded and runs in the `auto` permission mode (the CLI's
  model classifier decides what the settings rules leave open) unless its write
  chose `acceptEdits` through the optional `permissionMode` field, for that turn
  only. Tool requests that would ask a person go to the SDK's `canUseTool`
  callback, where the platform allows its built-in tools and the session's own
  plugin tools and denies the rest; partial messages are
  enabled and the data plane incrementally redacts and UTF-8-normalizes assistant
  text deltas and diagnostic stderr before appending them to the session
  scrollback. Type and Claude model are immutable session
  metadata. For the `platform-default` alias, the pod projects the optional
  `model` key from the platform `claude-code-config` ConfigMap into the
  tool-running container as `CLAUDE_CODE_MODEL`; a missing or empty key leaves
  `--model` unset so the installed Claude CLI chooses its default. A concrete
  session model is injected literally and takes precedence over that platform
  default. The required `base-url` (ConfigMap) and `auth-token` (credentials
  Secret) keys remain isolated in a
  hardened localhost proxy sidecar that accepts one configured HTTPS upstream.
  As for codex, the optional K3s MCP token lives only in a loopback
  `k3s-mcp-proxy` sidecar: it mints a repository-scoped, read-only GitHub App
  token from K3s MCP, clones the private `dlddu/plugin-marketplace` and serves
  the session's selected plugins as a seed; the agent installs that seed as a
  local `dlddu-plugins` marketplace and registers the K3s MCP itself (the
  runner's `k3s` server on the sidecar's loopback address). The tool-running
  container holds neither token. Without the key, the session runs with no K3s
  MCP tools and no plugins.
  A separate optional `models` ConfigMap key is an ordered UI soft catalog, not
  an API allowlist. The Deployment projects the public `model` and `models`
  keys into the control plane as `CLAUDE_CODE_DEFAULT_MODEL` and
  `CLAUDE_CODE_MODELS`; `GET /api/v1/config` exposes their validated startup
  snapshot without exposing credentials. The proxy forwards safe SSE response
  chunks before upstream completion, holds possible credential
  suffixes across network reads for tail-safe redaction, and enforces a 64 MiB
  raw-upstream SSE cap without whole-body buffering. The tool-running container
  receives a non-secret placeholder. Every data plane pod uses the dedicated
  `data-plane` ServiceAccount, whose cluster-wide built-in `view` binding gives
  read-only Kubernetes access while excluding Secrets.
  `minimax-code` runs the MiniMax Code CLI (`mcode exec`) on that same one-shot
  runner, queue, cursor and archive (AC-G1~G4). It gets the same loopback proxy
  sidecar reading its own `minimax-code-credentials` Secret and
  `minimax-code-config` ConfigMap — whose `auth-token` may be a MiniMax Token
  Plan subscription key — and no Claude
  plugin bootstrap; its archive sits behind its own
  `MINIMAX_CODE_ARCHIVE_ENABLED` gate. See
  [`docs/prd/minimax-code-workload.md`](docs/prd/minimax-code-workload.md).
  `codex` runs the Codex SDK (`@openai/codex-sdk`, pinned) on that same
  one-shot runner, queue, cursor and archive (AC-J1~J7). There is no provider
  Secret: each session signs in to ChatGPT itself — its first prompt prints a
  device-code URL and one-time code into the transcript, and the resulting
  `~/.codex/auth.json` is part of the session state, so it survives
  snapshot/restore behind its own `CODEX_ARCHIVE_ENABLED` gate. Its tool
  surface is conversation, the platform plugin's skills (served to the model
  by a stdio `skills` MCP, since it has no shell) and the K3s MCP through a
  loopback `k3s-mcp-proxy` sidecar that alone holds the optional token; shell,
  file-edit, web-search and image tools are off, writes are blocked by a
  read-only sandbox, and the runner aborts a turn on any shell/file/web item.
  A long-lived session takes a plugin update without losing its conversation
  through the workspace's `Refresh plugin skills` button
  (`POST /api/v1/sessions/{id}/plugins/refresh`, AC-J7); nothing refreshes on
  its own. See [`docs/prd/codex-workload.md`](docs/prd/codex-workload.md).
- **Per-session plugins** (AC-L1, [`docs/prd/session-plugins.md`](docs/prd/session-plugins.md)):
  `POST /api/v1/sessions` takes an optional `plugins` list of plugin names from
  the platform marketplace for `claude-code`, `codex` and `minimax-code`.
  Omitted, a codex-chat session gets the deployment's `CODEX_PLUGINS` and
  every other type runs without plugins; given, the list **replaces** that
  default and is fixed for the session's lifetime, snapshot/restore
  included. It reaches the `k3s-mcp-proxy` seeder of every type as
  `PLUGIN_SEED_PLUGINS`, and claude-code's workload container as
  `CLAUDE_CODE_PLUGINS` too (agent install + managed settings). `GET /api/v1/config` reports
  `takesPlugins` per type.
- **Tenancy** (AC-K1~K5, [`docs/prd/multitenancy.md`](docs/prd/multitenancy.md)):
  a request carrying `Authorization: Bearer <ServiceAccount token>` (audience
  `session-platform`, proven by TokenReview) speaks for the tenant that lists
  that ServiceAccount; it sees and operates only its own sessions (others are
  404) and creates only the workload types its tenant is granted (403). A
  request with no bearer token is the operator — the Authelia-fronted SPA — and
  sees every session, can filter by `?tenant=` and create on a tenant's behalf.
  Tenants come from the optional `session-platform-tenants` ConfigMap
  (`SESSION_PLATFORM_TENANTS`); without it nothing changes for the operator.
- **Lifecycle**: 60-min max idle → workload snapshot + pod reclaim (AC-B1);
  access → restore into a new pod (AC-B2). Every type uses the agent's
  filesystem archive behind its own explicit data-transfer gate
  (`SHELL_ARCHIVE_ENABLED`, `CLAUDE_CODE_ARCHIVE_ENABLED`,
  `MINIMAX_CODE_ARCHIVE_ENABLED`); the restore
  target pod carries the `session-platform.dev/restore-archive` annotation. A
  disabled gate returns a service-unavailable snapshot error and preserves the
  live pod. Archives use a CP-owned generation and durable owner-fenced
  `preparing→committing` transaction: prepare failures abort only that generation,
  while committing recovery keeps admission closed and retries Stop/finalization.
  While a shell checkpoint is in flight its process tree is paused (SIGSTOP) and
  writes are rejected; an aborted snapshot resumes it (SIGCONT) and reopens writes.
  Explicit deletion uses the same lifecycle Lease, reclaims any live pod, and
  removes the session record.
  Already-uploaded checkpoint/archive objects remain governed by the checkpoint
  store's retention policy; DELETE does not currently erase them physically.
- **Single entry point**: the control plane container serves both the REST API
  (`/api/v1`) and the statically built SPA on one port. JSON POST bodies have an
  8 MiB wire limit and a 30-second read timeout; Claude prompts also have the
  workload-specific 1 MiB decoded-payload limit. The long-lived output SSE is
  request-context bounded rather than subject to that short JSON I/O timeout.

Claude workspaces keep one passive session output stream open. Each `output`
event uses the server-issued byte cursor as its SSE id and carries
`{offset,payloadBase64,nextOffset}`; decoding the payload yields exactly
`nextOffset-offset` bytes. Base64 preserves the exact byte range and overlap
math on the wire; it does not make JavaScript string length a cursor. For
Claude output, every server-issued cursor and event boundary is also a UTF-8
code-point boundary, so the same cursor is safe for the JSON read fallback.
Reconnection resumes from `Last-Event-ID` (preferred over the initial `offset`
query).

If a requested cursor is ahead of the retained output, the stream emits
`event: reset` with `id=<currentLength>` and `{"nextOffset":currentLength}`.
The client discards partial decoder state, replaces its rendered history from
`POST /read` at offset 0, and reconnects from that read's `nextOffset`. Opening
the SSE connection and receiving output/reset/keepalive signals remain passive:
they do not promote state, restore a snapshot, or touch `lastAccess`. The
prescribed reset reconciliation is a normal Read API call, so it can promote an
idle session and touch `lastAccess`. On transport failure, the SPA checks
session state and reconnects only active/idle sessions, routing a snapshot to
explicit restore instead of restoring it through an automatic read retry.

The reaper scans the PRD's plain “last read/write was at least 60 minutes ago”
boundary and rechecks authoritative `lastAccess` under the lifecycle Lease.
Read/write do not hold that Lease and their `Touch` is best-effort, so an access
finishing between the recheck and checkpoint is a tracked freshness race.
A crash window is also tracked: a hard crash after `RestoreInto` succeeds but
before final CAS can orphan the restore-target pod. The archive prepare/commit
protocol, now shared by every type, does not close that workload-independent gap.
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
| `GET  /config`               | non-sensitive runtime UI config | E6     |
| `POST /sessions`             | create (provision pod, active) | A1, A2 |
| `GET  /sessions`             | list (`?tenant=` operator filter) | V5, K2 |
| `GET  /sessions/{id}`        | get one                        | V5     |
| `DELETE /sessions/{id}`       | delete and reclaim live pod    | A3     |
| `POST /sessions/{id}/read`   | read (state-branched)          | C2     |
| `POST /sessions/{id}/write`  | write (state-branched)         | C3     |
| `GET  /sessions/{id}/stream` | passive live output SSE        | E3     |
| `POST /sessions/{id}/switch` | switch (restore if snapshot)   | C4     |
| `POST /sessions/{id}/snapshot` | archive now and reclaim pod       | B1, A3 |
| `POST /sessions/{id}/recover` | bring a deleted session back from its last archive | B4, K2 |

## Testing

- **Unit** (`make test-unit`): both Go modules — API/service/store/orchestrator
  adapters plus the shell/Claude/archive/credential-proxy agents. Claude tests
  use reduced deterministic limits to exercise the production output-boundary
  logic without allocating 256 MiB.
- **Integration** (`make test-integration`, build tag `integration`): the
  happy-path scenarios from `docs/test/architecture.md` driven **in-process**
  (handlers mounted in a test server) with the stub adapters.
- **E2E** (kind-deployed SUT): builds the combined image, loads it into a kind
  cluster (`deploy/overlays/e2e/`, 2 control-plane replicas), and runs a Go API suite + a
  Playwright browser suite against the deployed control-plane (reachable at
  `http://localhost:8080` via a NodePort). Covers create/list/get/switch·read·write,
  real-pod provisioning (AC-A1/A2), and cross-replica state consistency over the
  shared ConfigMap store (AC-C1). With the archive gates turned on in the
  overlay, the snapshot → reclaim → restore round trip is a verified assertion
  too (AC-B2/B3/D4). J6 browser contract coverage
  uses deterministic Playwright route fixtures, while the Claude worker,
  cursor, live pre-exit output, reconnect, runner/proxy incremental redaction,
  proxy pre-EOF chunk forwarding, byte boundaries, output limits, resume,
  archive round trip, and lifecycle crash boundaries use fake-runner/adapter Go tests.
  A deployed test against the external Claude API is intentionally not claimed.
  Each e2e file declares the test scenario it verifies in its header
  (`// 검증 시나리오: lifecycle.md#시나리오 2`) and is registered in
  [`docs/test/e2e.md`](docs/test/e2e.md) — `make check-scenario-mapping` enforces
  the 1:1. The axis moved from acceptance criteria to test scenarios on
  2026-09-08, so the registry lists the scenarios that do not have a dedicated
  file yet; the reaper-driven idle→snapshot trigger
  (`lifecycle.md#시나리오 1`) stays a registered exception, waiting on the same
  `TODO(policy)` as the idle-state read/write branches.
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

PRs run [`.github/workflows/pr.yml`](.github/workflows/pr.yml); `main` branch
protection needs only its `pr-gate` check (plus `review/manual-approval`,
[docs/review-policy.md](docs/review-policy.md)). A `changes` job decides which
path-scoped jobs apply:

- **image** — [`image.yml`](.github/workflows/image.yml) publishes both images
  for the PR head SHA (see Deployment).
- **test** — [`test.yml`](.github/workflows/test.yml), every PR, no image
  needed: the static docs ↔ code gates (e2e fidelity allowlist, render
  fidelity, comment-policy ledger, scenario ↔ e2e mapping), lint + unit (Go),
  typecheck + build (web), the in-process integration harness, the
  real-apiserver envtest conflict suite, and — on changes to
  `docs/journeys/`, `docs/user-journeys/`, `docs/mockups/README.md`,
  `docs/index.html`, `docs/doc-structure-state.md` or `tools/` — the
  user-journey ↔ mockup gate.
- **e2e** — [`e2e.yml`](.github/workflows/e2e.yml) (kind SUT, Go API +
  Playwright) after `image`, running the images it just published, on PRs
  touching `control-plane/`, `data-plane/`, `web/`, `deploy/`, `scripts/e2e/`,
  `Makefile`, or the image/e2e/PR workflows; Playwright reports/traces upload
  as artifacts.
- **pr-gate** — fails unless every job above succeeded (a path-filtered e2e
  counts as passed).

Pushes to `main` run only `image.yml` (publish + pin, when the merge touches
the images or the production manifests) — the commit already passed `pr-gate`.

## Deployment

The cluster runs this via GitOps (Flux) from the `flux-cd-apps` repo.

- [`.github/workflows/image.yml`](.github/workflows/image.yml)
  publishes the combined control-plane image as
  `ghcr.io/dlddu/session-platform:<sha>` and the multi-workload agent as
  `ghcr.io/dlddu/session-platform-data-plane:<sha>` for every PR (head SHA, used
  by preview environments) and every push to `main` that touches the images or production manifests; on
  `main` the `pin` job then pins that SHA in `deploy/base/deployment.yaml` and
  force-pushes the result to the `deploy` branch, which Flux tracks (`main` is
  never rewritten). There is no `latest` tag.
- [`deploy/base/`](deploy/base/) holds the cluster manifests Flux applies (through
  [`deploy/overlays/prod/`](deploy/overlays/prod/), from the `deploy` branch): the `control-plane`
  Deployment + Service (port 80 → 8080), control-plane RBAC (pods + configmaps
  + leases), and the data-plane ServiceAccount with a cluster-wide built-in
  `view` binding. The latter is read-only and deliberately cannot read Secrets.
  Session state lives in in-cluster ConfigMaps/Leases, so there is no separate
  backing-store deployment. The namespace, ingress, and VPA live on the cluster
  side in `flux-cd-apps`.

Before creating Claude Code sessions, provision the Secret named by
`CLAUDE_CODE_CREDENTIALS_SECRET` (default `claude-code-credentials`) and the
ConfigMap named by `CLAUDE_CODE_CONFIG_CONFIGMAP` (default
`claude-code-config`). The Secret holds credentials only: its required
`auth-token` key is exposed only to the credential-proxy sidecar, and its
optional `k3s-mcp-token` key is exposed only to the loopback `k3s-mcp-proxy`
sidecar as `K3S_MCP_TOKEN`. Every other key below lives in the ConfigMap; its
required `base-url` key, like `auth-token`, is exposed only to the
credential-proxy sidecar. The K3s MCP sidecar uses its token to obtain
short-lived read access to the private plugin marketplace and seed the
session's selected plugins, which the agent installs; the session reaches the
K3s MCP through the same sidecar, which injects the token. The ConfigMap's `model` key is optional: a non-empty value selects
the model for `platform-default` sessions, while a missing or empty value falls
back to the installed Claude CLI default. A concrete model supplied when the
session is created always wins. Non-empty `model` values are validated against
`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`
when the data-plane container starts, so an invalid platform default fails fast
instead of being forwarded as a CLI option. The optional leading `~` supports
OpenRouter moving aliases such as `~anthropic/claude-opus-latest`.

The separate optional `models` key is a JSON string array used only as an
ordered UI picker catalog. The Deployment projects the public `model` and
`models` keys into the control plane as `CLAUDE_CODE_DEFAULT_MODEL` and
`CLAUDE_CODE_MODELS`; it does not grant the process access to `base-url` or
`auth-token`. A non-empty catalog must be a JSON array of unique model strings
that satisfy
`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`; empty entries, surrounding whitespace,
duplicates, and the reserved `platform-default` alias make the control plane
fail startup. Missing, empty, or `[]` keeps the existing free-text model UI.
`GET /api/v1/config` returns
`{"claudeCode":{"defaultModel":"~deepseek/deepseek-v4-flash-latest","models":[...]}}`
with `Cache-Control: no-store`; missing/empty `model` returns
`platform-default`. The UI renders the concrete default once as
`<model> (platform default)` and omits `model` from that create request, while
other choices are pinned literally. The catalog remains advisory: the
create-session API accepts valid model identifiers outside it.

If `CLAUDE_CODE_CONFIG_CONFIGMAP` is changed from its default name, also
patch the `valueFrom.configMapKeyRef.name` field for both
`CLAUDE_CODE_DEFAULT_MODEL` and `CLAUDE_CODE_MODELS` to the same ConfigMap.
Kubernetes does not
interpolate one environment variable into a
`configMapKeyRef.name`.

Changing the singular `model` key is observed the next time a
`platform-default` primary container starts: in a new pod, a pod recreated
during restore, or a container restart. It does not immediately mutate an
already-running container, and a concrete-model session keeps its literal
setting. The config API and UI read both `model` and `models` from the
control-plane process's startup environment, so either display change requires
a Deployment rollout. During the interval before that rollout, a newly started
`platform-default` session may already use the new ConfigMap model while the UI
still shows the prior snapshot. See
[`deploy/base/claude-code-credentials-secret.example.yaml`](deploy/base/claude-code-credentials-secret.example.yaml)
and
[`deploy/base/claude-code-config-configmap.example.yaml`](deploy/base/claude-code-config-configmap.example.yaml).

Codex sessions need `DATA_PLANE_CODEX_IMAGE` (the same data-plane image) and,
optionally, the `codex-credentials` Secret (`k3s-mcp-token`) and `codex-config`
ConfigMap (`model`, `models`, `k3s-mcp-url`, `plugin-marketplace-url`); see the
`deploy/base/codex-*.example.yaml` files. `codex-chat` also needs at least one slot in
its pool (`CODEX_CHAT_AUTH_SLOTS`, with `CODEX_CHAT_AUTH_STORAGE_CLASS` /
`_STORAGE_SIZE`, default `1Gi`): each slot PVC holds the ChatGPT login and the
session's SQLite database, which the sidecar serves as the `db` MCP server
(`raw_query`, `raw_exec`, `ledger_get`, `ledger_list`, `ledger_put`, `ledger_delete`).
See [session ledger contracts and storage migration](docs/session-ledger.md). Tenancy needs the
`session-platform-tenants` ConfigMap
([example](deploy/base/session-platform-tenants-configmap.example.yaml)) and a
`system:auth-delegator` ClusterRoleBinding for the control-plane
ServiceAccount, kept outside this base for the reason `deploy/base/rbac.yaml` gives.

The [`deploy/overlays/e2e/`](deploy/overlays/e2e/) directory remains the local `kind` setup for the
integration harness; `deploy/base/` is the deployed-cluster source of truth.
