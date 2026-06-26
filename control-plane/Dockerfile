# Multi-stage build: node builds the SPA -> go embeds it -> minimal final image.
# Build from the repo root:  docker build -f control-plane/Dockerfile -t session-platform/control-plane .

# ---- stage 1: build the web SPA ----
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
RUN npm run build

# ---- stage 2: build the Go control plane with the SPA embedded ----
FROM golang:1.24-alpine AS build
WORKDIR /src
# Copy the module manifests first so the dependency download layer caches across
# source-only changes. client-go (PodOrchestrator) and its transitive deps are
# resolved here.
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
WORKDIR /src/control-plane
RUN go mod download
COPY control-plane/ ./
# overlay the freshly built SPA into the embed directory
COPY --from=web /web/dist ./internal/static/dist
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

# ---- stage 3: minimal runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /control-plane
EXPOSE 8080
ENTRYPOINT ["/control-plane"]
