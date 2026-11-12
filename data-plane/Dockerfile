# Multi-workload data plane image. DATA_PLANE_WORKLOAD selects a PTY-attached
# shell, the serial one-shot Claude Code runner (claude-runner, on the Claude
# Agent SDK), or its localhost credential proxy sidecar. Shell and Claude
# expose lifecycle/I/O endpoints and snapshot through the same filesystem
# archive, and the proxy isolates auth.
# See cmd/agent and ./README.md.
# Build from the repo root:  docker build -f data-plane/Dockerfile -t session-platform/data-plane .

# ---- stage 1: build the agent ----
FROM golang:1.24-bookworm AS build
WORKDIR /src
# Module manifests first so the dependency layer caches across source changes.
COPY data-plane/go.mod data-plane/go.sum ./
RUN go mod download
COPY data-plane/ ./
RUN CGO_ENABLED=0 go build -o /out/agent ./cmd/agent

# ---- stage 2: install the official Claude Code package ----
# Install from the official package in a Node 22, trixie-based build stage, then
# copy that immutable installation into the runtime. The ARG remains
# overridable, while the default is a published exact version rather than the
# moving npm "latest" tag.
FROM node:22-trixie-slim AS claude-build
# claude-runner's Agent SDK (data-plane/claude-runner/package.json) drives this
# CLI over a private protocol, so the two move together: the SDK release
# 0.3.N is built for Claude Code 2.1.N, and the runtime stage refuses a pair
# that drifted apart (claude-runner --check-cli).
ARG CLAUDE_CODE_VERSION=2.1.220
# The MiniMax Code CLI (`mcode`) for the minimax-code workload, pinned the same
# way. It needs Node 22.19+, which this stage provides, and installs from the
# public npm package rather than the vendor's curl|bash installer so the image
# stays reproducible and ships one Node runtime, not two.
ARG MINIMAX_CODE_VERSION=0.6.12
ENV DISABLE_AUTOUPDATER=1
RUN npm install --global "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && claude --version \
 && npm install --global "@minimax-ai/code@${MINIMAX_CODE_VERSION}" \
 && mcode --version \
 && npm cache clean --force

FROM node:22-trixie-slim AS claude-runner-build
WORKDIR /opt/claude-runner
COPY data-plane/claude-runner/package.json data-plane/claude-runner/package-lock.json ./
# --omit=optional leaves out the SDK's own bundled CLI binary: the runner drives
# the pinned CLI installed above, so one binary serves both the turns and the
# plugin bootstrap.
RUN npm ci --omit=dev --omit=optional --ignore-scripts && npm cache clean --force
COPY data-plane/claude-runner/*.mjs ./
RUN node --test

FROM node:22-trixie-slim AS codex-build
WORKDIR /opt/codex-runner
COPY data-plane/codex-runner/package.json data-plane/codex-runner/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force
COPY data-plane/codex-runner/*.mjs ./
COPY --from=build /out/agent /agent
# Each Codex workload's managed config must give the model exactly its
# surface: codex-chat no shell (AC-J5), codex-task a shell and still no image,
# web or bundled-skill tools (AC-M1). A Codex upgrade that widens either fails
# the build here.
RUN node --test \
 && "$(node runner.mjs --print-codex-bin)" --version \
 && /agent codex-render-config /tmp/codex-surface chat \
 && node surface-check.mjs --home /tmp/codex-surface --mode chat \
 && /agent codex-render-config /tmp/codex-task-surface task \
 && node surface-check.mjs --home /tmp/codex-task-surface --mode task \
 && rm -rf /tmp/codex-surface /tmp/codex-task-surface

# ---- stage 3: install the official GitHub CLI release ----
# Debian trixie's community gh 2.46 package uses retired GitHub APIs. Download
# a current pinned upstream release instead, verifying the release checksum.
FROM debian:trixie AS gh-build
ARG GH_VERSION=2.97.0
ARG GH_AMD64_SHA256=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112
ARG GH_ARM64_SHA256=73ea440ecad9c9e284429997ee6f93577bc6f7bc6fba357ef62c53ad8fb641a5
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /tmp/gh
RUN gh_arch="$(dpkg --print-architecture)" \
 && case "$gh_arch" in \
      amd64) gh_sha256="$GH_AMD64_SHA256" ;; \
      arm64) gh_sha256="$GH_ARM64_SHA256" ;; \
      *) echo "unsupported GitHub CLI architecture: $gh_arch" >&2; exit 1 ;; \
    esac \
 && gh_archive="gh_${GH_VERSION}_linux_${gh_arch}.tar.gz" \
 && curl -fsSLO "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${gh_archive}" \
 && echo "$gh_sha256  $gh_archive" | sha256sum --check --strict - \
 && tar -xzf "$gh_archive" \
 && install -D -m 0755 "${gh_archive%.tar.gz}/bin/gh" /out/gh \
 && /out/gh --version

# ---- stage 4: runtime ----
# A full glibc distro rather than distroless/alpine: the session workload IS an
# interactive /bin/bash or the Node-based Claude runner and CLI. The pinned
# Node/Claude runtime and static agent are added below.
FROM debian:trixie
# safe.directory '/shared/*': the AC-F5 shared volume is EFS in production, and
# its access point forces every file to uid/gid 50000, so a repository the
# session MCP cloned there is owned by someone other than the workload's user
# and git refuses it ("detected dubious ownership"). System scope because git
# reads safe.directory only from system/global/command scope and the workload's
# HOME is its archived state tree. The trailing '/*' prefix form needs git
# 2.46+; the version gate fails the build if the base image ever ships older.
# python3 + python3-jsonschema: agent workloads (claude-code, minimax-code) run
# skills whose bundled helpers are Python scripts that validate records against
# draft 2020-12 JSON schemas (e.g. reconciler_tool.py). Installed from the distro
# rather than pip so the image keeps one package manager and needs no PyPI.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl git jq kubectl \
      python3 python3-jsonschema \
 && git --version \
 && python3 -c 'import sys, importlib.metadata as m; from jsonschema import Draft202012Validator; print(sys.version.split()[0], m.version("jsonschema"))' \
 && git config --system --add safe.directory '/shared/*' \
 && dpkg --compare-versions "$(git --version | cut -d' ' -f3)" ge 2.46 \
 && curl --version \
 && jq --version \
 && kubectl version --client \
 && rm -rf /var/lib/apt/lists/*
COPY --from=gh-build /out/gh /usr/local/bin/gh
RUN gh --version
# The shell workload runs as this account (control plane: ShellUserID, non-root,
# all capabilities dropped). Other workload types still run as root. The home
# field names the archived state tree the agent points HOME at; it is not
# created here because it lives on the pod's state volume.
RUN groupadd --gid 1000 session \
 && useradd --uid 1000 --gid 1000 --no-create-home \
      --home-dir /session/state/home --shell /bin/bash session \
 && test "$(id -u session)" = 1000
# The same image serves both workload modes. Copy the official Node 22 runtime,
# npm installation metadata, and the pinned Claude Code package together.
COPY --from=claude-build /usr/local/ /usr/local/
ENV DISABLE_AUTOUPDATER=1
RUN node --version && npm --version && claude --version && mcode --version
COPY --from=claude-runner-build /opt/claude-runner /opt/claude-runner
RUN printf '#!/bin/sh\nexec node /opt/claude-runner/runner.mjs "$@"\n' > /usr/local/bin/claude-runner \
 && chmod 0755 /usr/local/bin/claude-runner \
 && claude-runner --check-cli
COPY --from=codex-build /opt/codex-runner /opt/codex-runner
RUN printf '#!/bin/sh\nexec node /opt/codex-runner/runner.mjs "$@"\n' > /usr/local/bin/codex-runner \
 && chmod 0755 /usr/local/bin/codex-runner \
 && "$(codex-runner --print-codex-bin)" --version
COPY --from=build /out/agent /agent
COPY --chmod=0755 data-plane/entrypoint.sh /entrypoint.sh
# Agent port; keep in sync with cmd/agent (defaultAddr) and the control plane
# orchestrator's agentPort.
EXPOSE 8090
ENTRYPOINT ["/entrypoint.sh"]
