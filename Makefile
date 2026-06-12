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

vet: ## go vet everything
	go vet ./...

build: ## Build all Go packages
	go build ./...

lint: ## Run golangci-lint (run `make tools` first)
	$(GOBIN)/golangci-lint run ./...

test: ## Run all tests with race detector and coverage
	go test -race -covermode=atomic -coverprofile=coverage.out ./...

coverage-gate: test ## Enforce coverage floors (platform/tenancy 100%, platform/* >= 75%)
	bash tools/ci/coverage_gate.sh coverage.out

dev-up: ## Start the full dev stack and deploy the Vespa app
	$(COMPOSE) up -d --build --wait --wait-timeout 1800
	bash vespa/deploy.sh

dev-down: ## Stop the dev stack (volumes survive, incl. the TEI model cache)
	$(COMPOSE) down

dev-clean: ## Stop the dev stack AND wipe all volumes (full reset)
	$(COMPOSE) down -v

dev-logs: ## Tail dev stack logs
	$(COMPOSE) logs -f --tail=100

vespa-deploy: ## (Re)deploy the Vespa application package
	bash vespa/deploy.sh

e2e-smoke: ## End-to-end smoke test against the running dev stack
	bash tools/e2e/smoke.sh
