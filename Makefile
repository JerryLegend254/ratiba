# Ratiba task runner.

.DEFAULT_GOAL := help
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

POSTGRES_USER     ?= ratiba
POSTGRES_PASSWORD ?= ratiba_local_dev
POSTGRES_DB       ?= ratiba
POSTGRES_PORT     ?= 5432
DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable

# Exported so the migrate binary and, later, the API can read them.
TEST_DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)_test?sslmode=disable

# Exported so the migrate binary and, later, the API can read them.
export DATABASE_URL
export TEST_DATABASE_URL
export PGPASSWORD = $(POSTGRES_PASSWORD)

.PHONY: help
help: ## Show this help
	@echo "Ratiba — clinic appointment API"
	@echo ""
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Build and start postgres, run migrations and seed, start the API
	docker compose up --build -d

.PHONY: down
down: ## Stop the stack, keeping the database volume
	docker compose down

.PHONY: clean
clean: ## Stop the stack, delete the database volume and build output
	docker compose down --volumes --remove-orphans
	rm -rf bin coverage.out coverage.html

.PHONY: logs
logs: ## Follow the API logs
	docker compose logs -f api

.PHONY: db-start
db-start: ## Start only PostgreSQL, for running the API natively
	docker compose up -d postgres
	@for i in $$(seq 1 40); do \
		docker compose exec -T postgres pg_isready -U $(POSTGRES_USER) -d $(POSTGRES_DB) >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	@echo "==> PostgreSQL ready on port $(POSTGRES_PORT)"

.PHONY: db-stop
db-stop: ## Stop PostgreSQL
	docker compose stop postgres

.PHONY: observability
observability: ## Start the optional OTel + Prometheus + Grafana + Tempo stack
	OTEL_TRACES_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
		docker compose --profile observability up --build -d
	@echo "  Grafana     http://localhost:3000  (anonymous admin)"
	@echo "  Prometheus  http://localhost:9090"

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show which migrations have been applied
	go run ./cmd/migrate status

.PHONY: seed
seed: ## Insert or refresh the deterministic demo dataset (safe to re-run)
	go run ./cmd/migrate seed

.PHONY: psql
psql: ## Open a psql shell against the local database
	docker compose exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

.PHONY: generate
generate: ## Regenerate sqlc query code from db/queries and db/migrations
	sqlc generate

.PHONY: verify-generate
verify-generate: ## Fail if committed generated code differs from a fresh run
	sqlc generate
	@if ! git diff --quiet -- internal/postgres/sqlcgen; then \
		echo "ERROR: generated code is stale. Run 'make generate' and commit."; \
		git --no-pager diff --stat -- internal/postgres/sqlcgen; \
		exit 1; \
	fi
	@echo "==> Generated code is up to date"

.PHONY: format
format: ## Format all Go code
	gofmt -w -s .
	go mod tidy

.PHONY: format-check
format-check: ## Fail if any Go file is not gofmt-clean
	@unformatted=$$(gofmt -l -s .); \
	if [ -n "$$unformatted" ]; then \
		echo "ERROR: these files are not formatted. Run 'make format':"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "==> All files are formatted"

.PHONY: vet
vet: ## Run go vet, including build-tagged files
	go vet ./...
	@# Integration files are behind a build tag, so the default vet run does not
	@# see them. An unused import there only surfaces when the suite is run.
	go vet -tags=integration ./...

.PHONY: test-db
test-db: ## Create the integration test database if it does not exist
	@bash scripts/create-test-db.sh

.PHONY: run
run: ## Run the API against the local database
	go run ./cmd/api

.PHONY: docker-build
docker-build: ## Build the production image
	docker build -t ratiba:latest .

.PHONY: verify-compose
verify-compose: ## Validate the compose file and the observability profile
	docker compose config --quiet
	docker compose --profile observability config --quiet
	@echo "==> compose.yaml is valid"

.PHONY: build
build: ## Compile both binaries into ./bin
	@mkdir -p bin
	go build -trimpath -o bin/ratiba-api ./cmd/api
	go build -trimpath -o bin/ratiba-migrate ./cmd/migrate
	@echo "==> Built bin/ratiba-api and bin/ratiba-migrate"

.PHONY: unit-test
unit-test: ## Run unit tests with the race detector (no database needed)
	go test -race -count=1 ./...

.PHONY: integration-test
integration-test: test-db ## Run integration tests against real PostgreSQL
	go test -tags=integration -race -count=1 ./internal/postgres/...

.PHONY: verify-openapi
verify-openapi: ## Validate the OpenAPI contract and check it matches the routes
	go test -run 'TestOpenAPI' -count=1 ./internal/transport/http/...

.PHONY: test
test: unit-test integration-test ## Run every test suite
