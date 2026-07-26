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

# ---- stage 2: runtime ----
# A full glibc distro rather than distroless/alpine: the session workload IS an
# interactive /bin/bash with the standard userland, and glibc keeps the pod
# CRIU-friendly (AC-B*/AC-D4, see ../docs/criu-verification.md). Only criu and
# the static agent binary are added on top.
# debian:trixie because of its CRIU version: the agent CRIU-dumps/restores its
# shell tree in-pod (agent-driven checkpoint, docs/criu-verification.md), and the
# 2026-07-23 on-cluster verification showed jammy's CRIU 3.16.1 cannot parse the
# vdso on kernel 6.12/arm64 (init fails) while trixie's CRIU 4.1.1 passes on the
# same node. noble (24.04) packages no criu at all. criu is only invoked when
# CRIU_ENABLED is on; CRIU_BIN overrides the path.
FROM debian:trixie
RUN apt-get update \
 && apt-get install -y --no-install-recommends criu \
 && rm -rf /var/lib/apt/lists/*
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
