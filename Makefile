BINARY      := go-ledger
CMD         := ./cmd/server
BUILD_DIR   := bin

MIGRATIONS  := internal/postgres/migrations

.PHONY: run build test cover load lint tidy clean dev docker-build openapi sqlc proto migrate-up migrate-down jaeger help

run: ## Run the server
	go run $(CMD)

build: ## Build the server binary
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/$(BINARY) $(CMD)

test: ## Run all tests
	go test -race -cover ./...

cover: ## Run coverage and enforce floors (needs Docker for full numbers)
	@bash scripts/coverage.sh

load: ## Run the k6 load test against the local load-test Compose stack
	@docker compose --profile load-test up -d --build
	@echo "waiting for app health..."
	@until curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; do sleep 1; done
	@k6 run test/load/post_transactions.js || true
	@docker compose --profile load-test down

lint: ## Run golangci-lint
	golangci-lint run ./...

tidy: ## Tidy go.mod
	go mod tidy

openapi: ## Regenerate the committed OpenAPI spec (api/openapi.yaml)
	go run ./cmd/genopenapi

sqlc: ## Regenerate sqlc query code from internal/postgres/queries
	sqlc generate

proto: ## Lint and regenerate protobuf/gRPC code from proto/
	buf lint
	buf generate

migrate-up: ## Apply all migrations (needs DATABASE_URL)
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" up

migrate-down: ## Roll back the last migration (needs DATABASE_URL)
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" down

dev: ## Run with hot reload (requires air)
	air

jaeger: ## Start Jaeger all-in-one for local tracing (UI on :16686)
	docker compose up -d jaeger

docker-build: ## Build the Docker image
	docker build -t $(BINARY):latest .

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'
