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
COPY control-plane/go.mod ./control-plane/go.mod
# (no go.sum yet — stdlib-only module)
WORKDIR /src/control-plane
RUN go mod download || true
COPY control-plane/ ./
# overlay the freshly built SPA into the embed directory
COPY --from=web /web/dist ./internal/static/dist
RUN CGO_ENABLED=0 go build -o /out/control-plane ./cmd/control-plane

# ---- stage 3: minimal runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /control-plane
EXPOSE 8080
ENTRYPOINT ["/control-plane"]
