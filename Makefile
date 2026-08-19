# Ratiba task runner.
#
# There is no application yet — these targets exist to get a local PostgreSQL
# running and the schema applied, so the constraints can be exercised directly
# with SQL before any Go is written.

.DEFAULT_GOAL := help
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

POSTGRES_USER     ?= ratiba
POSTGRES_PASSWORD ?= ratiba_local_dev
POSTGRES_DB       ?= ratiba
POSTGRES_PORT     ?= 5432
POSTGRES_IMAGE    ?= postgres:17.5-alpine
CONTAINER         ?= ratiba-postgres

DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=disable
export PGPASSWORD = $(POSTGRES_PASSWORD)

.PHONY: help
help: ## Show this help
	@echo "Ratiba — clinic appointment API"
	@echo ""
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: db-start
db-start: ## Start a local PostgreSQL container
	@docker rm -f $(CONTAINER) >/dev/null 2>&1 || true
	docker run -d --name $(CONTAINER) \
		-e POSTGRES_USER=$(POSTGRES_USER) \
		-e POSTGRES_PASSWORD=$(POSTGRES_PASSWORD) \
		-e POSTGRES_DB=$(POSTGRES_DB) \
		-p $(POSTGRES_PORT):5432 $(POSTGRES_IMAGE)
	@echo "==> Waiting for PostgreSQL"
	@for i in $$(seq 1 40); do \
		docker exec $(CONTAINER) pg_isready -U $(POSTGRES_USER) -d $(POSTGRES_DB) >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	@echo "==> Ready on port $(POSTGRES_PORT)"

.PHONY: db-stop
db-stop: ## Stop and remove the local PostgreSQL container
	docker rm -f $(CONTAINER)

.PHONY: migrate-up
migrate-up: ## Apply migrations with psql
	@# The goose annotations are stripped here because there is no migration
	@# binary yet. Applying the same files two ways would be a good source of
	@# drift, so this is temporary: it is replaced by cmd/migrate once the Go
	@# module has something to build.
	@for f in db/migrations/*.sql; do \
		echo "==> $$f"; \
		awk '/^-- \+goose Down/{exit} !/^-- \+goose/{print}' "$$f" \
			| psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -q; \
	done
	@echo "==> Schema applied"

.PHONY: psql
psql: ## Open a psql shell against the local database
	psql "$(DATABASE_URL)"
