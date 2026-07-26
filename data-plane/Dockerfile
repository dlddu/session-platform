# Data plane session agent image: the ENTRYPOINT launches exactly one
# PTY-attached interactive shell (default /bin/bash, DATA_PLANE_SHELL
# overrides) and serves the /attach + /healthz endpoints the control plane
# reaches at the active transition (AC-D1). See cmd/agent and ./README.md.
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
      python3 python3-protobuf \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Only the criu binary is needed at runtime (the parasite blobs are linked into
# it); man pages and the python crit tool are skipped.
RUN curl -fsSL "https://github.com/checkpoint-restore/criu/archive/refs/tags/v${CRIU_VERSION}.tar.gz" \
      | tar -xz --strip-components=1 \
 && make -j"$(nproc)" criu \
 && ./criu/criu --version

# ---- stage 3: runtime ----
# A full glibc distro rather than distroless/alpine: the session workload IS an
# interactive /bin/bash with the standard userland, and glibc keeps the pod
# CRIU-friendly (AC-B*/AC-D4, see ../docs/criu-verification.md). Only criu and
# the static agent binary are added on top.
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
      libbsd0 libcap2 libnet1 libnl-3-200 libnl-route-3-200 libprotobuf-c1 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=criu-build /src/criu/criu /usr/local/sbin/criu
# Fail the image build (i.e. in CI) if the copied binary is missing a shared
# library here, instead of discovering it when a session pod tries to checkpoint.
# `--version` needs no privileges; the privileged `criu check` runs on-cluster.
RUN /usr/local/sbin/criu --version
# Pin the agent to the source-built binary rather than a PATH lookup, so a stray
# distro criu could never be picked up instead.
ENV CRIU_BIN=/usr/local/sbin/criu
COPY --from=build /out/agent /agent
# Go 1.24 uses Multipath TCP for listeners by default on Linux, but CRIU cannot
# checkpoint MPTCP sockets — the agent's :8090 listener must stay plain TCP or
# the pod checkpoint fails (AC-B*/AC-D4). Container env is inherited by the
# session shell too, so Go programs run inside the session stay checkpointable
# as well.
ENV GODEBUG=multipathtcp=0
# Agent port; keep in sync with cmd/agent (defaultAddr) and the control plane
# orchestrator's agentPort.
EXPOSE 8090
ENTRYPOINT ["/agent"]
