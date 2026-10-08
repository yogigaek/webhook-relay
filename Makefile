# Shortcuts for the commands in README.md. `make` alone lists them.

TEST_DATABASE_URL ?= postgres://relay:relay@localhost:5434/relay_test
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.1
REDOCLY := @redocly/cli@2.60.0

.DEFAULT_GOAL := help
.PHONY: help test test-unit lint lint-api check db demo demo-down

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-10s %s\n", $$1, $$2}'

test: db ## Run every test, including the PostgreSQL ones, with the race detector
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -race -count=1 ./...

test-unit: ## Run the tests that need no database
	go test -count=1 ./...

lint: ## gofmt, go vet, and staticcheck
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go run $(STATICCHECK) ./...

lint-api: ## Lint the OpenAPI spec
	npx --yes $(REDOCLY) lint api/openapi.yaml

check: lint lint-api test ## Everything CI runs, except the Docker build

db: ## Start PostgreSQL for development and tests
	docker compose up -d --wait postgres

demo: ## Run the full demo: relay, sink, PostgreSQL, Jaeger, Swagger UI
	docker compose --profile demo up --build

demo-down: ## Stop the demo
	docker compose --profile demo down
