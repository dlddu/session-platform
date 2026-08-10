# Multi-workload data plane image. DATA_PLANE_WORKLOAD selects a PTY-attached
# shell, the serial one-shot Claude Code runner, or its localhost credential
# proxy sidecar. Shell and Claude expose lifecycle/I/O endpoints; shell adds
# CRIU snapshots, Claude adds filesystem archives, and the proxy isolates auth.
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

# ---- stage 2: build criu from source ----
# trixie packages criu 4.1.1, but the in-pod checkpoint/restore path needs 4.2,
# so build the pinned release. The alternative — pinning criu from sid — would
# drag sid's glibc and friends into the runtime (the session workload IS that
# userland) and make the image a moving target that can break CI at any time;
# a pinned tarball keeps the build reproducible. Same base as the runtime stage
# so the binary links against the exact libs it will find there.
FROM debian:trixie AS criu-build
ARG CRIU_VERSION=4.2
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential pkg-config curl ca-certificates \
      libbsd-dev libcap-dev libnet1-dev libnl-3-dev libnl-route-3-dev \
      libprotobuf-c-dev libprotobuf-dev protobuf-c-compiler protobuf-compiler \
      python3 python3-protobuf uuid-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Only the criu binary is needed at runtime (the parasite blobs are linked into
# it); man pages and the python crit tool are skipped.
RUN curl -fsSL "https://github.com/checkpoint-restore/criu/archive/refs/tags/v${CRIU_VERSION}.tar.gz" \
      | tar -xz --strip-components=1 \
 && make -j"$(nproc)" criu \
 && ./criu/criu --version

# ---- stage 3: install the official Claude Code package ----
# Install from the official package in a Node 22, trixie-based build stage, then
# copy that immutable installation into the runtime. The ARG remains
# overridable, while the default is a published exact version rather than the
# moving npm "latest" tag.
FROM node:22-trixie-slim AS claude-build
ARG CLAUDE_CODE_VERSION=2.1.220
ENV DISABLE_AUTOUPDATER=1
RUN npm install --global "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && claude --version \
 && npm cache clean --force

# ---- stage 4: install the official GitHub CLI release ----
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

# ---- stage 5: runtime ----
# A full glibc distro rather than distroless/alpine: the session workload IS an
# interactive /bin/bash or Node-based Claude CLI, and glibc keeps shell pods
# CRIU-friendly (AC-B*/AC-D4, see ../docs/criu-verification.md). The pinned
# Node/Claude runtime, criu, and static agent are added below.
# debian:trixie for the CRIU story: the agent CRIU-dumps/restores its shell tree
# in-pod (agent-driven checkpoint, docs/criu-verification.md). The 2026-07-23
# on-cluster verification showed jammy's CRIU 3.16.1 cannot parse the vdso on
# kernel 6.12/arm64 (init fails), and noble packages no criu at all. criu itself
# comes from the source-build stage above (4.2, not trixie's 4.1.1); only its
# runtime libraries are installed here. criu is invoked only when CRIU_ENABLED
# is on.
FROM debian:trixie
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl git jq kubectl \
      libbsd0 libcap2 libnet1 libnl-3-200 libnl-route-3-200 libprotobuf-c1 libuuid1 \
 && git --version \
 && curl --version \
 && jq --version \
 && kubectl version --client \
 && rm -rf /var/lib/apt/lists/*
COPY --from=gh-build /out/gh /usr/local/bin/gh
RUN gh --version
# The same image serves both workload modes. Copy the official Node 22 runtime,
# npm installation metadata, and the pinned Claude Code package together.
COPY --from=claude-build /usr/local/ /usr/local/
ENV DISABLE_AUTOUPDATER=1
RUN node --version && npm --version && claude --version
COPY --from=criu-build /src/criu/criu /usr/local/sbin/criu
# Fail the image build (i.e. in CI) if the copied binary is missing a shared
# library here, instead of discovering it when a session pod tries to checkpoint.
# `--version` needs no privileges; the privileged `criu check` runs on-cluster.
RUN /usr/local/sbin/criu --version
# Pin the agent to the source-built binary rather than a PATH lookup, so a stray
# distro criu could never be picked up instead.
ENV CRIU_BIN=/usr/local/sbin/criu
COPY --from=build /out/agent /agent
COPY --chmod=0755 data-plane/entrypoint.sh /entrypoint.sh
COPY --chmod=0755 data-plane/git-askpass.sh /usr/local/bin/session-platform-git-askpass
# Go 1.24 uses Multipath TCP for listeners by default on Linux, but CRIU cannot
# checkpoint MPTCP sockets — the agent's :8090 listener must stay plain TCP or
# the pod checkpoint fails (AC-B*/AC-D4). Container env is inherited by the
# session shell too, so Go programs run inside the session stay checkpointable
# as well.
ENV GODEBUG=multipathtcp=0
# Agent port; keep in sync with cmd/agent (defaultAddr) and the control plane
# orchestrator's agentPort.
EXPOSE 8090
ENTRYPOINT ["/entrypoint.sh"]
