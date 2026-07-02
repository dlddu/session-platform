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
# debian-slim rather than distroless/alpine: the session workload IS an
# interactive /bin/bash with the standard userland, and glibc keeps the pod
# CRIU-friendlier for the later checkpoint/restore work (AC-B*/AC-D4, see
# ../docs/criu-verification.md). Only the static agent binary is added on top.
FROM debian:bookworm-slim
COPY --from=build /out/agent /agent
# Agent port; keep in sync with cmd/agent (defaultAddr) and the control plane
# orchestrator's agentPort.
EXPOSE 8090
ENTRYPOINT ["/agent"]
