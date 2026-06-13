SHELL := /bin/bash
GOBIN := $(CURDIR)/bin
COMPOSE := docker compose -f deploy/compose/docker-compose.yml

# Tool versions (single source of truth; CI pins golangci-lint to the same
# version via its action input — keep them in sync).
BUF_VERSION := v1.70.0
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1
GOLANGCI_LINT_VERSION := v2.12.2

.PHONY: tools proto fmt vet build lint test coverage-gate dev-up dev-down dev-clean dev-logs vespa-deploy e2e-smoke help

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-16s %s\n", $$1, $$2}'

tools: ## Install dev tooling into ./bin
	@mkdir -p $(GOBIN)
	GOBIN=$(GOBIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN=$(GOBIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(GOBIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	GOBIN=$(GOBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

proto: ## Regenerate Go code from protobuf definitions
	cd platform/proto && PATH="$(GOBIN):$$PATH" buf generate

fmt: ## gofmt all non-generated Go files
	@gofmt -w $$(git ls-files '*.go' | grep -v '/gen/' || true)

# Go source roots. NEVER bare ./... — web/node_modules can contain vendored
# Go files from npm packages that must not enter the build/test/lint walk.
GO_PKGS := ./platform/... ./services/... ./connectors/... ./tools/...

vet: ## go vet everything
	go vet $(GO_PKGS)

build: ## Build all Go packages
	go build $(GO_PKGS)

lint: ## Run golangci-lint (run `make tools` first)
	$(GOBIN)/golangci-lint run $(GO_PKGS)

test: ## Run all tests with race detector and coverage
	go test -race -covermode=atomic -coverprofile=coverage.out $(GO_PKGS)

coverage-gate: test ## Enforce coverage floors (platform/tenancy 100%, platform/* >= 75%)
	bash tools/ci/coverage_gate.sh coverage.out

# Built one at a time: parallel BuildKit builds of 9 images spike memory hard
# enough to OOM-kill running containers on small Docker VMs (observed).
BUILT_SERVICES := gateway control-plane connector-hub ingest enrich index-writer query fake-gmail web

dev-build: ## Build all service images serially (low-memory friendly)
	@for s in $(BUILT_SERVICES); do echo "== build $$s"; $(COMPOSE) build $$s || exit 1; done

dev-up: dev-build ## Start the full dev stack and deploy the Vespa app
	$(COMPOSE) up -d --wait --wait-timeout 1800
	set -a; [ -f deploy/compose/.env ] && . deploy/compose/.env; set +a; bash vespa/deploy.sh

dev-down: ## Stop the dev stack (volumes survive, incl. the TEI model cache)
	$(COMPOSE) down

dev-clean: ## Stop the dev stack AND wipe all volumes (full reset)
	$(COMPOSE) down -v

dev-logs: ## Tail dev stack logs
	$(COMPOSE) logs -f --tail=100

vespa-deploy: ## (Re)deploy the Vespa application package
	set -a; [ -f deploy/compose/.env ] && . deploy/compose/.env; set +a; bash vespa/deploy.sh

e2e-smoke: ## End-to-end smoke test against the running dev stack
	bash tools/e2e/smoke.sh

e2e-m1: ## M1 exit test: synthetic email ingest, freshness, hybrid search (E2E_EMAIL_COUNT=10000)
	bash tools/e2e/m1-e2e.sh

e2e-leakage: ## Cross-tenant leakage suite (sacred — must always pass)
	bash tools/e2e/leakage.sh
