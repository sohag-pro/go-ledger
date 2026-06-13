BINARY      := go-ledger
CMD         := ./cmd/server
BUILD_DIR   := bin

.PHONY: run build test lint tidy clean dev docker-build openapi help

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

dev: ## Run with hot reload (requires air)
	air

docker-build: ## Build the Docker image
	docker build -t $(BINARY):latest .

clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR)

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'
