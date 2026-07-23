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
# ubuntu rather than distroless/alpine: the session workload IS an
# interactive /bin/bash with the standard userland, and glibc keeps the pod
# CRIU-friendlier for the later checkpoint/restore work (AC-B*/AC-D4, see
# ../docs/criu-verification.md). Only the static agent binary is added on top.
# 22.04 (jammy) rather than 24.04: the agent CRIU-dumps/restores its shell tree
# in-pod (agent-driven checkpoint, docs/criu-verification.md) and needs the criu
# binary, which noble dropped from the archive ("no installation candidate")
# while jammy still packages it. criu is only invoked when CRIU_ENABLED is on
# and the pod carries the criu capabilities; CRIU_BIN overrides the path.
FROM ubuntu:22.04
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
