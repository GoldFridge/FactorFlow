.PHONY: help build run test test-all test-race lint fmt tidy up down migrate-up migrate-down

GO      ?= go
BIN     ?= bin/factorflow
PKG     ?= ./...

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

build: ## Build the server binary
	$(GO) build -o $(BIN) ./cmd/factorflow

run: ## Run the server locally
	$(GO) run ./cmd/factorflow

test: ## Unit and property tests (no Docker required)
	$(GO) test -short $(PKG)

test-all: ## All tests including integration (requires Docker)
	$(GO) test $(PKG)

test-race: ## All tests with the race detector
	$(GO) test -race $(PKG)

cover: ## Test coverage summary
	$(GO) test -short -coverprofile=coverage.txt $(PKG)
	$(GO) tool cover -func=coverage.txt | tail -1

lint: ## Static analysis
	$(GO) vet $(PKG)
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, ran go vet only"

fmt: ## Format sources
	$(GO) fmt $(PKG)

tidy: ## Tidy module dependencies
	$(GO) mod tidy

up: ## Start local dependencies
	docker compose -f deploy/docker-compose.yml up -d

down: ## Stop local dependencies
	docker compose -f deploy/docker-compose.yml down
