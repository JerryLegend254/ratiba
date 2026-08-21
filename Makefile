# Ratiba task runner.

.DEFAULT_GOAL := help
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

# The Go version pinned in go.mod. CI and the Dockerfile must agree with it;
# `verify-go-version` checks that they do.
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod)

# Pinned tool versions. CI installs exactly these, so "works on my machine"
# cannot be a lint-version difference.
SQLC_VERSION        := v1.31.1
GOLANGCI_VERSION    := v2.12.2
GOVULNCHECK_VERSION := latest

# Business-critical packages. Coverage is enforced on these, not on the whole
# module — a threshold averaged over generated code and wiring measures nothing.
CRITICAL_PACKAGES := \
	github.com/JerryLegend254/ratiba/internal/appointment \
	github.com/JerryLegend254/ratiba/internal/doctor \
	github.com/JerryLegend254/ratiba/internal/patient
COVERAGE_THRESHOLD := 80

POSTGRES_USER     ?= ratiba
POSTGRES_PASSWORD ?= ratiba_local_dev
POSTGRES_DB       ?= ratiba
POSTGRES_PORT     ?= 5432
DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable

# Exported so the migrate binary and, later, the API can read them.
TEST_DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)_test?sslmode=disable

# Exported so the migrate binary and, later, the API can read them.
API_URL ?= http://localhost:8080

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

.PHONY: doctor
doctor: ## Check local prerequisites and report what is missing
	@bash scripts/doctor.sh

.PHONY: bootstrap
bootstrap: ## Install pinned development tools and create .env
	@echo "==> Installing pinned tools"
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@if [ ! -f .env ]; then cp .env.example .env && echo "==> Created .env from .env.example"; \
	 else echo "==> .env already exists, leaving it alone"; fi
	go mod download
	@echo "==> Ready. Next: make up"

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

.PHONY: verify-go-version
verify-go-version: ## Fail if go.mod, the Dockerfile and CI disagree on the Go version
	@bash scripts/verify-go-version.sh

.PHONY: verify-migrations
verify-migrations: ## Check migration filenames are sequential and well-formed
	@bash scripts/verify-migrations.sh

.PHONY: verify-docs
verify-docs: ## Check that every relative link in the documentation resolves
	@bash scripts/verify-docs.sh

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: vulncheck
vulncheck: ## Scan dependencies for known vulnerabilities
	govulncheck ./...

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

.PHONY: smoke
smoke: ## Run a read-only smoke test against a running API
	@bash scripts/smoke.sh $(API_URL)

.PHONY: smoke-write
smoke-write: ## Run the full book/reschedule/cancel lifecycle against a running API
	@bash scripts/smoke.sh $(API_URL) --write

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

.PHONY: coverage
coverage: test-db ## Measure coverage and enforce the threshold on critical packages
	@bash scripts/coverage.sh coverage.out $(COVERAGE_THRESHOLD) $(CRITICAL_PACKAGES)

.PHONY: coverage-html
coverage-html: coverage ## Open the coverage report in a browser
	go tool cover -html=coverage.out -o coverage.html
	@echo "==> Wrote coverage.html"

.PHONY: check
check: verify-go-version format-check vet lint verify-generate verify-migrations verify-openapi verify-docs vulncheck ## Run every static check

.PHONY: ci
ci: check test coverage build docker-build verify-compose ## Everything CI runs
