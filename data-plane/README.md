# data-plane

The data plane is where actual session workloads run — one dedicated pod per
session (AC-A2). The control plane provisions and reclaims these pods via the
`PodOrchestrator` port.

## Status: shell, claude-code, and credential-proxy agents built

`DATA_PLANE_WORKLOAD` selects one of three modes in the dedicated session pod:

- `shell` (default) launches one PTY-attached interactive shell
  (`DATA_PLANE_SHELL`, default `/bin/bash`). Write is shell stdin, read is
  accumulated PTY output, and checkpoint/restore uses CRIU.
- `claude-code` launches no resident shell. Write asynchronously queues a
  prompt for one serial worker; each job executes
  `claude [--continue] [--model MODEL] -p -- PROMPT`, and read returns the merged
  stdout/stderr through the same offset cursor contract. `--continue` starts
  only after the first successful invocation; a failed or timed-out first run
  still starts a new conversation next time. `CLAUDE_CODE_MODEL=platform-default` omits the
  model flag. Each invocation is limited to 30 minutes by default; set
  `CLAUDE_CODE_RUN_TIMEOUT` to another positive Go duration when needed. One
  prompt is limited to 1 MiB; an oversized write is not queued and returns 413.
  The pending queue is bounded to 64 prompts and 8 MiB; a new write that would
  exceed either bound returns 429 while already accepted prompts keep draining.
- `credential-proxy` is the localhost-only sidecar mode. It reads the real
  `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, pins every request to that
  configured HTTPS upstream (plain HTTP is rejected), forwards only an explicit
  Anthropic/Claude/Stainless header allowlist, rejects tunnels/upgrades, and
  injects the real token as `Authorization: Bearer …`. Set
  `DATA_PLANE_AGENT_ADDR=127.0.0.1:8091`; non-loopback bind addresses are
  rejected. Its `/healthz` endpoint never contacts the upstream.

Claude's cwd (`workspace/`) and HOME (`home/`) live under
`CLAUDE_CODE_STATE_DIR` (default `/session`). Its checkpoint closes prompt
admission, drains accepted work, and archives that state tree plus the redacted
scrollback; restore safely installs the archive before reopening writes. The
image includes the official `@anthropic-ai/claude-code` npm package. Auth
credentials are not present in the Claude container or inherited by its Bash
tools. That container receives only
`ANTHROPIC_BASE_URL=http://127.0.0.1:8091` and the non-secret
`ANTHROPIC_AUTH_TOKEN=session-platform-proxy`; the credential-proxy sidecar
alone receives the Secret. Fresh session HOME also receives a platform-managed
`.claude/settings.json` that allows only the coding tools
`Read`, `Write`, `Edit`, `Glob`, `Grep`, and `Bash`; the agent does not
use `--dangerously-skip-permissions`.

Merged raw stdout/stderr from one Claude invocation is capped at 16 MiB,
including the terminal line
`[session-platform: invocation output truncated at 16 MiB]`. Cumulative Claude
scrollback is capped at 256 MiB, including
`[session-platform: session output limit reached; further prompts are disabled]`.
The cumulative cap never rewrites bytes already exposed through `nextOffset`.
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
`/read`, `/checkpoint`, and `/restore` on port 8090. Restore-target agents
report healthy while awaiting the archive so provisioning cannot deadlock.
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
