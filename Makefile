# session-platform — root build orchestration.
# `make build` produces the control-plane binary with the React SPA embedded.

CP_DIR      := control-plane
WEB_DIR     := web
EMBED_DIR   := $(CP_DIR)/internal/static/dist
BIN         := $(CP_DIR)/bin/control-plane

# Isolated nested module holding the real-apiserver CAS/Lease conflict suite.
ENVTEST_DIR         := $(CP_DIR)/internal/adapter/configmap/envtest
ENVTEST_K8S_VERSION ?= 1.30.0

.DEFAULT_GOAL := build

.PHONY: build web embed control-plane run dev test test-unit test-integration test-envtest lint fmt docker clean tidy e2e-up e2e-down e2e-api e2e-web e2e

## build: web -> embed -> control-plane binary
build: control-plane

## web: install deps (if needed) and produce web/dist
web:
	cd $(WEB_DIR) && (test -d node_modules || npm install) && npm run build

## embed: copy the built SPA into the Go embed directory
embed: web
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	cp -r $(WEB_DIR)/dist/. $(EMBED_DIR)/

## control-plane: build the Go binary (with embedded SPA)
control-plane: embed
	cd $(CP_DIR) && go build -o bin/control-plane ./cmd/control-plane

## run: build then run the server (serves API + SPA on :8080).
## Needs a reachable cluster: the in-cluster config, or a kubeconfig pointing at
## e.g. a local kind cluster (`make e2e-up` brings one up). The control plane
## provisions data plane pods there and exits if no cluster is reachable.
run: build
	./$(BIN)

## dev: run control plane and Vite dev server together (Vite proxies /api).
## Control plane on :8080, SPA with HMR on :5173. Like `run`, the control plane
## needs a reachable cluster (kubeconfig / kind) to start.
dev:
	cd $(CP_DIR) && go run ./cmd/control-plane & \
	cd $(WEB_DIR) && (test -d node_modules || npm install) && npm run dev; \
	kill %1 2>/dev/null || true

## test: unit tests (Go) + web typecheck
test: test-unit lint

## test-unit: Go unit tests
test-unit:
	cd $(CP_DIR) && go test ./...

## test-integration: opt-in happy-path integration harness (in-process stubs).
## Skips CRIU scenarios unless CRIU_ENABLED=1 and a verified runtime exist.
test-integration:
	cd $(CP_DIR) && go test -tags=integration ./...

## test-envtest: real-apiserver CompareAndSwap/Lease conflict suite (AC-C1
## single-winner). Runs the isolated envtest module — controller-runtime stays
## out of the main module's deps — provisioning kube-apiserver + etcd via
## setup-envtest. Needs network on first run to fetch the binaries. setup-envtest
## is pinned to the release-0.18 line so it builds with this module's Go 1.24
## (newer setup-envtest releases require a newer toolchain).
test-envtest:
	cd $(ENVTEST_DIR) && \
	  KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.18 use $(ENVTEST_K8S_VERSION) -p path)" \
	  go test ./...

## e2e-up: bring up the kind-based SUT (deployed control-plane, 2 replicas over
## the in-cluster ConfigMap/Lease store) reachable at http://localhost:8080.
## Idempotent (skips create if it exists).
e2e-up:
	./scripts/e2e/up.sh

## e2e-down: delete the kind e2e cluster.
e2e-down:
	./scripts/e2e/down.sh

## e2e-api: run the Go API e2e suite against the running SUT (needs `e2e-up`).
e2e-api:
	cd $(CP_DIR) && go test -tags=e2e ./test/...

## e2e-web: run the Playwright browser e2e suite against the running SUT.
e2e-web:
	cd $(WEB_DIR) && (test -d node_modules || npm install) && npx playwright test

## e2e: run both e2e suites against an already-up SUT (api then web).
e2e: e2e-api e2e-web

## lint: go vet + gofmt check + web typecheck
lint:
	cd $(CP_DIR) && go vet ./... && test -z "$$(gofmt -l . | tee /dev/stderr)"
	cd $(WEB_DIR) && (test -d node_modules || npm install) && npm run lint

## fmt: format Go sources
fmt:
	cd $(CP_DIR) && gofmt -w .

## tidy: tidy the Go module
tidy:
	cd $(CP_DIR) && go mod tidy

## docker: build the single combined API+SPA image
docker:
	docker build -t session-platform/control-plane:dev -f $(CP_DIR)/Dockerfile .

## clean: remove build artifacts (keeps the embed placeholder)
clean:
	rm -rf $(CP_DIR)/bin $(WEB_DIR)/dist $(EMBED_DIR)/assets
	cd $(CP_DIR) && git checkout -- internal/static/dist/index.html 2>/dev/null || true
