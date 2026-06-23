# session-platform — root build orchestration.
# `make build` produces the control-plane binary with the React SPA embedded.

CP_DIR      := control-plane
WEB_DIR     := web
EMBED_DIR   := $(CP_DIR)/internal/static/dist
BIN         := $(CP_DIR)/bin/control-plane

.DEFAULT_GOAL := build

.PHONY: build web embed control-plane run dev test test-unit test-integration lint fmt docker clean tidy

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

## run: build then run the server (serves API + SPA on :8080)
run: build
	./$(BIN)

## dev: run control plane and Vite dev server together (Vite proxies /api).
## Control plane on :8080, SPA with HMR on :5173.
dev:
	cd $(CP_DIR) && go run ./cmd/control-plane & \
	cd $(WEB_DIR) && (test -d node_modules || npm install) && npm run dev; \
	kill %1 2>/dev/null || true

## test: unit tests (Go) + web typecheck
test: test-unit lint

## test-unit: Go unit tests
test-unit:
	cd $(CP_DIR) && go test ./...

## test-integration: opt-in happy-path integration harness (kind + Redis).
## Skips CRIU scenarios unless CRIU_ENABLED=1 and a verified runtime exist.
test-integration:
	cd $(CP_DIR) && go test -tags=integration ./...

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
