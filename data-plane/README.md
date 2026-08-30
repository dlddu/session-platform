# data-plane

The data plane is where actual session workloads run — one dedicated pod per
session (AC-A2). The control plane provisions and reclaims these pods via the
`PodOrchestrator` port.

The runtime image includes `git`, `gh`, `curl`, `jq`, and `kubectl` for use by
interactive shell and Claude Code workloads.

## Status: shell, claude-code, and credential-proxy agents built

`DATA_PLANE_WORKLOAD` selects one of three modes in the dedicated session pod:

- `shell` (default) launches one PTY-attached interactive shell
  (`DATA_PLANE_SHELL`, default `/bin/bash`). Write is shell stdin, read is
  accumulated PTY output, and checkpoint/restore uses CRIU.
- `claude-code` launches no resident shell. Write asynchronously queues a
  prompt for one serial worker; each job executes
  `claude [--continue] [--model MODEL] --permission-mode auto
  -p --output-format stream-json --verbose --include-partial-messages -- PROMPT`.
  The agent parses stdout JSONL and
  projects ordered `text_delta` values, while diagnostic stderr is retained as
  text. Both are incrementally redacted and appended through the same offset
  cursor contract while the invocation is still running. `--continue` starts
  only after the first successful invocation; a failed or timed-out first run
  still starts a new conversation next time. A non-empty concrete
  `CLAUDE_CODE_MODEL` adds `--model` with that value as one argv element;
  an empty value or the `platform-default` sentinel omits the model flag. Each
  invocation is limited to 30 minutes by default; set
  `CLAUDE_CODE_RUN_TIMEOUT` to another positive Go duration when needed. One
  prompt is limited to 1 MiB; an oversized write is not queued and returns 413.
  The pending queue is bounded to 64 prompts and 8 MiB; a new write that would
  exceed either bound returns 429 while already accepted prompts keep draining.
- `credential-proxy` is the localhost-only sidecar mode. It reads the real
  `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, pins every request to that
  configured HTTPS upstream (plain HTTP is rejected), forwards only an explicit
  Anthropic/Claude/Stainless header allowlist, rejects tunnels/upgrades, and
  injects the real token as `Authorization: Bearer …`. Provider `text/event-stream`
  bodies are forwarded incrementally up to a 64 MiB raw-upstream cap: a safe
  flushed chunk reaches Claude before upstream EOF, while a streaming literal
  redactor holds possible token suffixes across response reads so a split
  credential is never exposed transiently. This does not buffer the full
  Anthropic SSE response. Set
  `DATA_PLANE_AGENT_ADDR=127.0.0.1:8091`; non-loopback bind addresses are
  rejected. Its `/healthz` endpoint never contacts the upstream.

Claude's cwd (`workspace/`) and HOME (`home/`) live under
`CLAUDE_CODE_STATE_DIR` (default `/session/state`). Kubernetes mounts the
session volume at `/session`, leaving the replaceable state tree one level
below the mount point so restore can atomically swap it without trying to
rename an active mount. Its checkpoint closes prompt admission, drains accepted
work, and archives that state tree plus the redacted scrollback; restore safely
installs the archive before reopening writes. The image includes the official
`@anthropic-ai/claude-code` npm package. For a `claude-code` container only,
`entrypoint.sh` calls K3s MCP with `K3S_MCP_TOKEN` to mint a short-lived
`contents:read` GitHub App token scoped to `plugin-marketplace`, supplies that
token only through a process-scoped Git HTTPS Authorization header while running
`claude plugin marketplace add https://github.com/dlddu/plugin-marketplace.git`
and `claude plugin install session-platform@dlddu-plugins`, then exports the
runtime plugin seed and execs the agent. The GitHub token is unset before the
agent starts; shell and credential-proxy workloads skip this bootstrap.
Provider authentication credentials are not present in the Claude container or
inherited by its Bash tools. That container receives
`ANTHROPIC_BASE_URL=http://127.0.0.1:8091` and the non-secret
`ANTHROPIC_AUTH_TOKEN=session-platform-proxy`; the credential-proxy sidecar
alone receives the required Secret `base-url` and `auth-token` keys. The primary
container separately receives the required
Secret key `k3s-mcp-token` as `K3S_MCP_TOKEN`; that literal is included in
incremental output redaction and is never projected into the credential proxy.
For a session whose immutable model metadata is `platform-default`, the primary
container also receives `CLAUDE_CODE_MODEL` from that Secret's optional `model`
key. A missing or empty key omits `--model` and therefore delegates to the
installed Claude CLI; a concrete session model is instead injected literally
and takes precedence over the Secret default. The optional model is not exposed
to the credential-proxy sidecar. Any non-empty effective model must match
`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`.
A leading `~` supports OpenRouter moving aliases. Invalid Secret configuration
is rejected when the agent starts instead of being forwarded as CLI options.
Changes to that Secret default are resolved
whenever a `platform-default` primary container starts, including a new pod,
a pod recreated after restore, or a container restart. They do not immediately
mutate a running container environment or a concrete session model.

The plural Secret key `models` is not data-plane configuration and is never
projected into either session container. For UI presentation, the Deployment
projects the public singular `model` and plural `models` keys into the control
plane as `CLAUDE_CODE_DEFAULT_MODEL` and `CLAUDE_CODE_MODELS`. The config API
returns their validated startup snapshot; a concrete default is rendered once
as `<model> (platform default)`, while missing/empty falls back to the generic
alias. Either display change requires a control-plane rollout, but a session
pod still resolves its own `model` SecretKeyRef whenever it starts. Missing,
empty, or `[]` preserves the UI's free-text input, and the catalog does not
restrict models accepted by the session API. If the credentials Secret is
renamed, both control-plane SecretKeyRef names must be patched to the same
literal name because Kubernetes does not interpolate
`CLAUDE_CODE_CREDENTIALS_SECRET` there.

Fresh session HOME also receives a platform-managed `.claude/settings.json`
that enables only `session-platform@dlddu-plugins` and allows only the coding
tools `Read`, `Write`, `Edit`, `Glob`, `Grep`, and `Bash`; the agent does not use
`--dangerously-skip-permissions`. Every invocation starts Claude Code in `auto`
permission mode through an explicit CLI flag.

Projected assistant text plus diagnostic stderr from one Claude invocation is
capped at 16 MiB after incremental redaction, including the terminal line
`[session-platform: invocation output truncated at 16 MiB]`. Cumulative Claude
scrollback is capped at 256 MiB, including
`[session-platform: session output limit reached; further prompts are disabled]`.
The cumulative cap never rewrites bytes already exposed through `nextOffset`.
The streaming writer holds incomplete UTF-8 and possible credential-match
suffixes until they are safe to expose, so every issued cursor lands on a UTF-8
code-point boundary and a credential split across runner writes is never
transiently readable.
Prompts accepted before the cap was observed still drain through the serial
worker (later output is discarded); new writes after the terminal marker return
507. Checkpoint/restore preserves the bounded bytes, terminal state, resume flag,
and every previously issued cursor.

The production archive transaction is control-plane generated and crash
recoverable. Before calling the agent, the control plane durably records a
private `preparing` transaction containing generation, owner, and source pod,
then supplies that generation in `X-Session-Checkpoint-ID`. After the archive
ref is durable it records `committing` before deleting the pod. Recovery claims
a `preparing` owner fence before aborting that exact generation and conditionally
clearing it; a `committing` transaction is never aborted and instead retries pod
reclamation and finalization. Snapshot/restore/recovery renew the default
15-second Lease every 5 seconds with a per-renewal deadline. A delayed abort can
never reopen a different active generation; repeating an already completed
abort is idempotent.

The shell and Claude modes expose `/healthz`, reachability-only `/attach`, `/write`,
`/read`, `/stream`, `/checkpoint`, and `/restore` on port 8090. `/read`
remains the non-consuming JSON replay/catch-up endpoint. `/stream?offset=N` is
a long-lived SSE view of the same append-only bytes. An `output` event uses
`nextOffset` as its id and JSON `{offset,payloadBase64,nextOffset}`; the decoded
byte count equals `nextOffset-offset`. Base64 preserves exact bytes and cursor
length rather than allowing Claude events to split a UTF-8 rune: every
server-issued Claude cursor and event boundary is a code-point boundary.

If the requested cursor is greater than the retained byte length, the agent
emits `event: reset` with that current length as both the id and
`data.nextOffset`. A receiver must discard partial decoder state, reconcile the
full retained history through `/read?offset=0`, and resume from that read's
cursor. `Last-Event-ID` takes precedence over the query cursor on a reconnect,
whether the agent endpoint is called directly or through the public proxy.
The SSE reset itself is passive; at the public API, the prescribed `POST /read`
reconciliation retains normal state-branched access semantics and can promote
an idle session and refresh `lastAccess`.
Comment keepalives carry no cursor or workload state, and there are no run/queue
lifecycle events. Restore-target agents report healthy while awaiting the
archive so provisioning cannot deadlock.
The credential proxy exposes only `/healthz` plus the fixed-upstream proxy on
its loopback port 8091; it never logs the token and strips/redacts it from
upstream responses.

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

The agent-driven CRIU round trip is exercised by the kind e2e overlay. The
production manifest deliberately keeps `CRIU_ENABLED=false` until its target
nodes and privilege policy are approved (see `../docs/criu-verification.md`).
