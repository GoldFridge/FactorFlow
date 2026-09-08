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

# -p 1 is not a preference. Each integration package starts its own PostgreSQL container,
# and when several start at once the slower ones time out; pgtest then SKIPS rather than
# fails, so a contended parallel run reports success with the database tests never run.
test-all: ## All tests including integration (requires Docker)
	$(GO) test -p 1 -count=1 $(PKG)

test-race: ## All tests with the race detector
	$(GO) test -race -p 1 -count=1 $(PKG)

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

web: ## Run the web app against a local server
	cd web && npm run dev

web-install: ## Install the web app's dependencies
	cd web && npm install

web-test: ## Web app tests and type check
	cd web && npm run typecheck && npm test

seed: ## Fill the database with the demo dataset
	go run ./cmd/factorflow seed

up: ## Start local dependencies
	docker compose -f deploy/docker-compose.yml up -d

down: ## Stop local dependencies
	docker compose -f deploy/docker-compose.yml down
