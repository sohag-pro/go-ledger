BINARY      := go-ledger
CMD         := ./cmd/server
BUILD_DIR   := bin

MIGRATIONS  := internal/postgres/migrations

.PHONY: run build test lint tidy clean dev docker-build openapi sqlc proto migrate-up migrate-down help

run: ## Run the server
	go run $(CMD)

build: ## Build the server binary
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/$(BINARY) $(CMD)

test: ## Run all tests
	go test -race -cover ./...

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

docker-build: ## Build the Docker image
	docker build -t $(BINARY):latest .

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'
