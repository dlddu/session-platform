# data-plane

The data plane is where actual session workloads run — one dedicated workload
pod per session, plus any session-scoped auxiliary pods the workload type
needs (AC-A2). The control plane provisions and reclaims these pods via the
`PodOrchestrator` port.

The runtime image includes `git`, `gh`, `curl`, `jq`, and `kubectl` for use by
interactive shell and Claude Code workloads.

## Status: shell, claude-code, minimax-code, codex-chat, codex-task, mcp, credential-proxy, and k3s-mcp-proxy agents built


`DATA_PLANE_WORKLOAD` selects one of these modes across a session's pods:

- `shell` (default) launches one PTY-attached interactive shell
  (`DATA_PLANE_SHELL`, default `/bin/bash`) in its home directory
  `$SHELL_STATE_DIR/home` (default `/session/state/home`), with `HOME` pointing
  there. Write is shell stdin and read is accumulated PTY output. A snapshot
  archives the home directory and the scrollback; a restore starts a fresh
  shell in the restored home with the scrollback preloaded. Running processes,
  environment, cwd and shell variables/functions/aliases are not carried over.
- `claude-code` launches no resident shell. Write asynchronously queues a
  prompt for one serial worker; each job executes
  `claude [--continue] [--model MODEL] [--effort EFFORT] --permission-mode auto
  -p --output-format stream-json --verbose --include-partial-messages -- PROMPT`.
  `CLAUDE_CODE_EFFORT` (`low`, `medium`, `high`, `xhigh` or `max`, AC-E8) adds
  `--effort`; empty leaves the CLI default and any other value refuses start.
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
  `CLAUDE_CODE_AUTH_MODE` picks the provider auth (AC-E7). Unset or `gateway`
  is the loopback credential proxy above. `subscription` signs the CLI in to a
  Claude subscription with the platform's long-lived OAuth token (the one
  `claude setup-token` prints), which the control plane projects from the
  `oauth-token` key of `claude-code-credentials` as `CLAUDE_CODE_OAUTH_TOKEN`.
  The agent refuses to start if that variable is empty or holds whitespace, or
  if `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` is
  set. There is no sign-in step and no auth slot; the token is redacted from
  session output like every other credential.

- `minimax-code` runs the MiniMax Code CLI on the same one-shot runner as
  `claude-code` — queue, bounds, cursor, resume flag and archive are shared
  (AC-G2/G4). Each job executes
  `mcode exec --config CONFIG --model custom_provider:session-platform/MODEL
  --permission full --effort xhigh --output-format stream-json [--continue] --
  PROMPT`, and
  the agent projects only the `contentDelta` text of `agent_message` items;
  reasoning, tool-call and lifecycle records are dropped, and a failed turn is
  reported through mcode's own stderr diagnostics. `CONFIG` is a platform-managed
  file written at startup *beside* the state tree (so it is never archived): it
  declares one Anthropic-messages provider at the helper pod's credential proxy
  (`ANTHROPIC_BASE_URL`, `http://<helper-ip>:8091`) with the
  non-secret placeholder key and the session's model (with the effort levels
  mcode needs to accept `--effort`, and the 512k/128k token limits for
  `MiniMax-M3.1-Flash`), and turns data
  contribution and self-update off; the agent also sets
  `MCODE_DISABLE_EVAL_CAPTURE=1` so no run uploads step reports. `MINIMAX_CODE_MODEL` selects the model; `platform-default`
  or empty runs `MiniMax-M3.1-Flash`, because mcode, unlike Claude Code, has no
  installation default to fall back to. The container gets no Claude bootstrap
  and refuses to start if any provider key (`ANTHROPIC_API_KEY`,
  `MINIMAX_API_KEY`, `MCODE_PROVIDER_API_KEY`, …) or `K3S_MCP_TOKEN` is set
  (AC-G3). As a type behind the approval gate (`docs/prd/approval-gate.md`), the pod has no sidecar and its egress reaches
  only its session's helper pod (AC-G6), so every address it gets is a helper
  pod address that must not be loopback: the provider proxy (`:8091`), the
  session MCP (`SESSION_MCP_URL`, `:8092`) and the K3s MCP proxy
  (`K3S_MCP_PROXY_URL`, `:8093`). The helper's provider proxy is the
  gate's helper proxy (AC-F6) pointed at the `minimax-code-credentials` Secret and
  `minimax-code-config` ConfigMap: the ConfigMap's `base-url` is MiniMax's
  Anthropic-compatible endpoint (for example
  `https://api.minimax.io/anthropic`) and the Secret's `auth-token` may be a Token Plan
  subscription key: MiniMax accepts it as `Authorization: Bearer`, which is
  exactly what the proxy injects. On every start and restore the agent stages
  Claude-format plugins beside the managed config and copies them into
  `~/.minimax/plugins/`, removing only plugins it marked as its own: its own
  `session-platform-session-mcp`, registering the session MCP (its external
  tools pass the approval gate and the agent tails the MCP's notices into the
  output, AC-F3), plus every plugin of the K3s MCP proxy's `/plugin-seed` bundle.
  In that bundle only MCP servers change: the one whose URL is `K3S_MCP_URL` is
  re-pointed at the proxy with its OAuth field and `Authorization` header
  removed, and any other server is dropped with a warning, since the pod cannot
  reach it. The K3s MCP tools are not behind the session gate because the K3s
  MCP gates its own writes.
- `codex-chat` (formerly `codex`, still accepted as `DATA_PLANE_WORKLOAD`)
  runs the Codex SDK on the same one-shot runner as `claude-code`
  (AC-J1/J2): queue, bounds, cursor, resume flag and archive are shared. Each
  job executes `codex-runner --model MODEL [--resume] -- PROMPT`
  (`CODEX_RUNNER_BIN`, default `/usr/local/bin/codex-runner`, a shim for
  `node /opt/codex-runner/runner.mjs`). `MODEL` is `CODEX_MODEL` or
  `platform-default`, which lets codex pick its own default. The runner
  (`codex-runner/`, `@openai/codex-sdk` pinned to 0.166.0 by its lockfile)
  first checks `codex login status`. If the session is not signed in, it runs
  `codex login --device-auth` and forwards the ChatGPT verification URL and
  one-time code as notice lines, then runs the turn once sign-in completes. A
  failed sign-in exits 2 (AC-J3). It starts a thread, or resumes the one
  recorded in `$CODEX_HOME/session-platform-thread.json`, with
  `sandboxMode: read-only`, `approvalPolicy: never`, web search and sandbox
  network off and `skipGitRepoCheck`. It writes every SDK event as one JSON
  line and exits 0 on `turn.completed` and 1 on `turn.failed` or `error`. Its
  codex child gets only `HOME`, `CODEX_HOME`, `PATH`, `LANG`, `TZ` and
  `SSL_CERT_FILE`. A `command_execution`, `file_change` or `web_search` item
  trips a kill switch: the runner writes a notice, aborts the turn and exits 3.
  The agent projects notices, finished `agent_message` text, a
  `· server.tool` line per MCP call (plus an `error:` line if it fails) and
  `error:` lines for failed turns, and drops everything else, including
  reasoning and tool output. Records are capped at 8 MiB per line.
  On every start and restore the agent rewrites the platform-owned
  `$HOME/.codex/config.toml`. It pins approval, sandbox, web search, file-based
  ChatGPT login (`cli_auth_credentials_store = "file"`,
  `forced_login_method = "chatgpt"`) and no update check. It turns off the
  shell, `unified_exec`, `view_image`, image generation, apps, plugins, browser
  and computer use, memories, hooks and the other side-channel features, and
  the bundled system skills. It sets `developer_instructions` that tell the
  model to read skills through the skills MCP. It registers two MCP servers:
  `skills` (`/agent skills-mcp`, a stdio MCP serving `list_skills` and
  `read_skill{name, file?}` from `CODEX_SKILLS_DIR`, with regular files only,
  no symlinks, nothing outside the directory and a 256 KiB cap per file, plus
  `refresh_skills` when the sidecar has a marketplace and
  `SESSION_PLATFORM_AGENT_URL` names this agent on loopback) and,
  only when the sidecar's `/healthz` reports `configured: true`, `k3s` at
  `K3S_MCP_PROXY_URL/mcp`. Codex is left with conversation, plugin skills and
  the K3s MCP (AC-J5). `apply_patch` and the collaboration tools stay
  model-visible because the remote model catalog forces code mode on. The
  read-only sandbox and the kill switch are what stop them. Plugin skills come
  from the sidecar's `/plugin-seed` bundle (the `minimax-code` format). Each
  `skills/<name>/` directory a seeded plugin names is installed as
  `$HOME/.agents/skills/<plugin>--<name>/` with the managed marker; stale
  managed skills are removed and skills the user installed are left alone.
  The agent waits up to 90 s for the sidecar and its seed. After that it
  starts without skills, or without the K3s MCP, rather than failing the pod.
  `POST /plugins/refresh` (AC-J7) fetches them again mid-session: only
  between turns (409 while a turn runs or waits, a checkpoint or another
  refresh is under way), it asks the sidecar's `POST /plugin-seed/refresh` to
  clone the marketplace once more, re-syncs the managed skills from the new
  bundle and answers `{state, revision, added, updated, removed}`. A refresh
  that cannot fetch or read the new bundle answers 502 and leaves the skills as
  they were. Every sync that changes skills — a refresh, or a restore into a
  newer marketplace — folds the change into
  `$HOME/.codex/session-platform-skill-changes.json`; the next resumed turn's
  prompt opens with a note naming those skills and asking the model to re-read
  them, and the record is cleared once a turn succeeds. Nothing refreshes on
  its own. The model's `refresh_skills` posts to the loopback-only
  `POST /plugins/refresh/turn`, which does the same refresh from inside the
  running turn: once per turn (429 after), 409 outside a turn or while a
  checkpoint waits, 403 from anything but loopback. The tool result names the
  changes, so a turn that refreshed and succeeded clears the record; the
  transcript line is written after that turn's end marker.
  Both codex types also get `scheduler` at `SESSION_SCHEDULER_MCP_URL`
  (the sidecar's `/scheduler/mcp`, docs/prd/session-scheduler.md AC-P1~P3):
  `schedule_once`, `loop_create` (fixed `interval`, or self-paced with
  `fallback_interval`), `loop_next`, `schedule_list` and `schedule_cancel`.
  Times are ISO 8601 durations or RFC 3339 times only; limits are 20
  schedules, 16 KiB prompts, 1 minute to 24 hours between runs and 7 days of
  life. The sidecar (enabled by `SESSION_SCHEDULER_AGENT_URL`, this agent on
  loopback) keeps the schedules in memory and, when the loopback-only
  `GET /queue` reports the agent idle, puts one run at a time on the queue
  with `POST /write`, its first line `[session-platform scheduler] <id> …`.
  `GET /scheduler` relays the sidecar's schedule count; the control plane's
  idle reaper holds a codex session while it is above zero (AC-P3).
  The pod has no provider credential: `K3S_MCP_PROXY_URL` must be
  `http://<loopback IP>:8093` (the `k3s-mcp-proxy` sidecar, AC-J6), and the
  agent refuses to start if `OPENAI_API_KEY`, `CODEX_API_KEY`,
  `CODEX_ACCESS_TOKEN`, `K3S_MCP_TOKEN`, any `ANTHROPIC_*` key, `GITHUB_TOKEN`
  or another credential a bundled CLI reads is set. The ChatGPT login
  (`$HOME/.codex/auth.json`) and codex's own thread history are part of HOME,
  so the archive carries them and a restored session stays signed in. The
  control plane only offers the archive when `CODEX_ARCHIVE_ENABLED` is set
  (AC-J4). Before archiving, the agent drops the absolute helper links codex
  leaves in `$CODEX_HOME/tmp/arg0-linux` when a run is killed. The image build
  installs the runner with `npm ci`, runs its `node --test` suite, renders the
  managed config with the hidden `/agent codex-render-config <home>` (widest
  surface, K3s MCP included) and runs `codex-runner/surface-check.mjs` against
  it. The check runs `codex debug prompt-input` and fails the build if
  `exec_command`, `write_stdin`, `view_image` or another shell, image or web
  tool is model-visible, if bundled system skills are listed, or if the
  managed instructions are missing. That rendering does not include MCP
  tools, because codex does not wait for MCP servers there, so the check
  covers what the config removes.
- `codex-task` (`docs/prd/codex-task-workload.md`) is `codex-chat`'s pod and
  runner with claude-code's tool level (AC-M1). Each job executes
  `codex-runner --mode task --model MODEL [--effort EFFORT] [--resume] --
  PROMPT`; `CODEX_REASONING_EFFORT` (`minimal`, `low`, `medium`, `high` or
  `xhigh`, AC-M4) sets the SDK's `modelReasoningEffort`, and `codex-chat`
  refuses to start with it set. Task mode runs with `sandboxMode:
  danger-full-access`, `approvalPolicy: never`, sandbox network on and web
  search off, and has no kill switch. Its managed `config.toml` leaves the
  shell and `unified_exec` on, opens the sandbox, keeps every other
  `codex-chat` switch, and carries its own developer instructions (marker
  `session-platform codex task session`). The ChatGPT login is the auth
  slot's `auth.json` (`AGENT_AUTH_SLOT_DIR`), kept in sync with
  `$HOME/.codex/auth.json` — copied into HOME before every turn (HOME's copy
  removed when the slot has none), back every second while the runner works
  and once more after it exits (a file removed in HOME empties the slot),
  always by atomic 0600 replace rather than a symlink — and left out of the
  archive (AC-M2, AC-M3, `docs/prd/agent-auth-pool.md` AC-N4;
  `GET /auth-slot` answers `{"signedIn": bool}` for a workload with a slot and
  404 otherwise); an empty slot gets the same in-turn device-code sign-in as
  `codex-chat`, and the end-of-turn copy keeps it for the next session. The
  image build renders both modes (`/agent codex-render-config <home>
  chat|task`) and runs `surface-check.mjs --mode chat|task`: task mode must
  show a shell tool and its own marker, and no image or web tool or bundled
  skill.
- `k3s-mcp-proxy` is the `minimax-code` helper pod's K3s MCP proxy, and the
  `codex-chat` and `codex-task` pods' loopback sidecar (`DATA_PLANE_PROXY_PLACEMENT=sidecar`,
  `127.0.0.1:8093`): the
  credential-proxy implementation with the MCP Streamable HTTP header allowlist
  (`Accept`, `Content-Type`, `Mcp-Session-Id`, `Mcp-Protocol-Version`,
  `Last-Event-Id`, `User-Agent`), bound per `DATA_PLANE_PROXY_PLACEMENT` like
  the provider proxy (`:8093` in the helper pod), forwarding to the
  origin of `K3S_MCP_URL` (default `https://homelab-k3s-mcp.llkm.nl`) with
  `K3S_MCP_TOKEN` as the bearer. Without a token it still starts, reports
  `configured: false` on `/healthz`, and answers everything else with 503.
  Session traffic passes a deny filter first: a `tools/call` for any tool in
  `k3sMCPDeniedTools` (`k3s_mcp_tool_deny.go` — the writes, `resource_exec`/
  `attach`/`port_forward`/`proxy`, `opensearch_*`, `session_*`,
  `dear_baby_reset_user`, `github_commit_status_create`) is answered with an
  `isError` tool result and never forwarded, because `mcode` runs with
  `--permission full` and has no deny rules of its own.
  With a token it also bootstraps the plugin marketplace for the workload pod,
  which cannot reach GitHub: it asks the K3s MCP for a `plugin-marketplace` read
  token through its own handler in process, clones
  `MINIMAX_CODE_PLUGIN_MARKETPLACE_URL` into `MINIMAX_CODE_MARKETPLACE_DIR` with a
  URL-scoped header, and serves the plugins named in `MINIMAX_CODE_PLUGINS`
  (the session's selection; empty or unset means none, and then nothing is
  cloned and the bundle is `ready` and empty) — manifest plus the files its `skills`, `commands`,
  `agents` and `mcpServers` fields name, never hooks — at `GET /plugin-seed`
  (503 while bootstrapping, then `ready`, `failed` or `unconfigured`).
- `mcp` is the session MCP that runs in the helper pod's other container
  (AC-F4). It serves `/healthz` and a JSON-RPC endpoint at `/mcp` that answers
  `initialize`, `ping`, `tools/list` and `tools/call`. It offers one external
  tool, `web_fetch_get`, and every call to it passes the human approval gate
  (AC-F3): the request is created on PocketAide's external approval API at
  `APPROVAL_GATEWAY_URL` under the external identifier
  `{SESSION_ID}:{requestID}`, authenticated with the helper pod's
  ServiceAccount token (audience `pocket-aide`) read from
  `APPROVAL_GATEWAY_TOKEN_FILE`, and the outbound GET happens only after an
  APPROVED decision. A rejection, expiry, poll timeout or gateway outage comes
  back as an `isError` tool result, never as a transport error, so the agent
  reads the refusal and carries on with the session still `active`. **Without
  the gateway URL or a readable token the tool list is empty** — a container that cannot ask
  a human offers nothing rather than reaching out ungated. It keeps no state —
  the helper pod is discarded on freeze and rebuilt on restore.
- `credential-proxy` holds the provider credentials outside the tool-running
  container. It reads the real
  `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, pins every request to that
  configured HTTPS upstream (plain HTTP is rejected), forwards only an explicit
  Anthropic/Claude/Stainless header allowlist, rejects tunnels/upgrades, and
  injects the real token as `Authorization: Bearer …`. Provider `text/event-stream`
  bodies are forwarded incrementally up to a 64 MiB raw-upstream cap: a safe
  flushed chunk reaches Claude before upstream EOF, while a streaming literal
  redactor holds possible token suffixes across response reads so a split
  credential is never exposed transiently. This does not buffer the full
  Anthropic SSE response. Set
  `DATA_PLANE_AGENT_ADDR` to match its placement, declared by
  `DATA_PLANE_PROXY_PLACEMENT`: `sidecar` (the default when unset) is the
  `claude-code` arrangement and accepts loopback binds only, while `helper` is
  the approval gate's arrangement in the session helper pod and requires a
  pod-network bind because its client is a pod away (AC-F6). Each placement
  rejects the other's address, so opening the wider bind for the helper cannot
  reach the sidecar. What keeps the wider bind safe is AC-F2's ingress policy,
  which the control plane creates with the helper pod and which admits only that
  session's workload pod. Its `/healthz` endpoint never contacts the upstream.

Claude's cwd (`workspace/`) and HOME (`home/`) live under
`CLAUDE_CODE_STATE_DIR` (default `/session/state`). Kubernetes mounts the
session volume at `/session`, leaving the replaceable state tree one level
below the mount point so restore can atomically swap it without trying to
rename an active mount. Its checkpoint closes prompt admission, drains accepted
work, and archives that state tree plus the redacted scrollback; restore safely
installs the archive before reopening writes. When the workload mounts the
session's shared volume (`SESSION_SHARED_DIR`, a type behind the approval gate — minimax-code),
the archive also carries that volume under `shared/` and restore copies it into
the next round's freshly mounted volume; an archive with shared files refuses to
restore into a pod without one rather than dropping them (AC-F5).

The shell's state tree sits at the same place (`SHELL_STATE_DIR`, default
`/session/state`, volume at `/session`). Its checkpoint closes write admission
(409) and SIGSTOPs the shell and its descendants so the home directory holds
still, archives `home/` plus the scrollback, and keeps both paused until the
control plane reclaims the pod or calls `/checkpoint/abort`, which SIGCONTs the
tree and reopens writes. A process that daemonized out of the shell's tree is
not paused. The image includes the official
`@anthropic-ai/claude-code` npm package. For a `claude-code` container whose
session chose plugins (`CLAUDE_CODE_PLUGINS` set; AC-L1) only,
`entrypoint.sh` calls K3s MCP with `K3S_MCP_TOKEN` to mint a short-lived
`contents:read` GitHub App token scoped to `plugin-marketplace`, supplies that
token only through a process-scoped Git HTTPS Authorization header while running
`claude plugin marketplace add https://github.com/dlddu/plugin-marketplace.git`
and `claude plugin install <name>@dlddu-plugins` for each chosen plugin, then exports the
runtime plugin seed and execs the agent. The GitHub token is unset before the
agent starts; a claude-code session that chose none, and every other workload — `shell`, `minimax-code` (its marketplace path is the K3s MCP proxy, above), `mcp` and
`credential-proxy` — skips this bootstrap.
Provider authentication credentials are not present in the Claude container or
inherited by its Bash tools. That container receives
`ANTHROPIC_BASE_URL=http://127.0.0.1:8091` and the non-secret
`ANTHROPIC_AUTH_TOKEN=session-platform-proxy`; the credential-proxy sidecar
alone receives the required ConfigMap `base-url` and Secret `auth-token` keys. The primary
container separately receives the required
Secret key `k3s-mcp-token` as `K3S_MCP_TOKEN`; that literal is included in
incremental output redaction and is never projected into the credential proxy.
For a session whose immutable model metadata is `platform-default`, the primary
container also receives `CLAUDE_CODE_MODEL` from the platform ConfigMap's
optional `model` key. A missing or empty key omits `--model` and therefore delegates to the
installed Claude CLI; a concrete session model is instead injected literally
and takes precedence over the ConfigMap default. The optional model is not exposed
to the credential-proxy sidecar. Any non-empty effective model must match
`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`.
A leading `~` supports OpenRouter moving aliases. Invalid ConfigMap configuration
is rejected when the agent starts instead of being forwarded as CLI options.
Changes to that ConfigMap default are resolved
whenever a `platform-default` primary container starts, including a new pod,
a pod recreated after restore, or a container restart. They do not immediately
mutate a running container environment or a concrete session model.

The plural ConfigMap key `models` is not data-plane configuration and is never
projected into either session container. For UI presentation, the Deployment
projects the public singular `model` and plural `models` keys into the control
plane as `CLAUDE_CODE_DEFAULT_MODEL` and `CLAUDE_CODE_MODELS`. The config API
returns their validated startup snapshot; a concrete default is rendered once
as `<model> (platform default)`, while missing/empty falls back to the generic
alias. Either display change requires a control-plane rollout, but a session
pod still resolves its own `model` ConfigMapKeyRef whenever it starts. Missing,
empty, or `[]` preserves the UI's free-text input, and the catalog does not
restrict models accepted by the session API. If the config ConfigMap is
renamed, both control-plane ConfigMapKeyRef names must be patched to the same
literal name because Kubernetes does not interpolate
`CLAUDE_CODE_CONFIG_CONFIGMAP` there.

Fresh session HOME also receives a platform-managed `.claude/settings.json`
that enables only the session's chosen `<name>@dlddu-plugins` plugins (none
unless it chose some) and allows only the coding
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
`/read`, `/stream`, `/checkpoint`, `/checkpoint/abort`, and `/restore` on port
8090, with the same generation-fenced archive protocol for every type. `/read`
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
image (`DATA_PLANE_IMAGE`; published by `.github/workflows/image.yml`
as `…-data-plane`) with no command override — and refuses to mark a session
`active` until the attach stream opens. The in-code fallback image remains a
generic `alpine:3.20`, which cannot pass the shell readiness probe: real
deployments must inject `DATA_PLANE_IMAGE` (deploy/base/deployment.yaml and the e2e
overlay both do). The control plane talks to a cluster either via its in-cluster
config (as a pod) or the ambient kubeconfig (local development against a kind
cluster).

Every workload type's archive round trip is exercised by the kind e2e overlay.
Each type has its own control-plane data-transfer gate because archives move
session files to the checkpoint store: `SHELL_ARCHIVE_ENABLED`,
`CLAUDE_CODE_ARCHIVE_ENABLED`, `MINIMAX_CODE_ARCHIVE_ENABLED` and
`CODEX_ARCHIVE_ENABLED`. Session pods need no privilege for any of them.

