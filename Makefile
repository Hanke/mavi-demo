.DEFAULT_GOAL := help
SHELL := /bin/bash

COMPOSE := docker compose

# Python for the AI service: the checkout's venv when present, else whatever is on PATH.
PYTHON ?= $(shell test -x $(CURDIR)/ai/.venv/bin/python && echo $(CURDIR)/ai/.venv/bin/python || echo python)

# Every file a generator writes. `make check-contracts` fails when one is stale.
GENERATED := ai/openapi.json api/internal/aiclient/types.gen.go api/internal/contract/types.gen.go web/src/api/schema.d.ts

.PHONY: help up down logs ps migrate migrate-down migrate-status seed worker test test-api test-ai test-db test-web health \
        lint lint-api lint-ai fmt-ai generate generate-ai-spec generate-api generate-web check-contracts

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

up: ## Build and start all services
	@[ -f .env ] || cp .env.example .env
	$(COMPOSE) up --build -d
	@$(MAKE) --no-print-directory health

down: ## Stop all services (keeps volumes)
	$(COMPOSE) down

logs: ## Tail logs for all services
	$(COMPOSE) logs -f

ps: ## Show service status
	$(COMPOSE) ps

migrate: ## Apply pending migrations in infra/db/migrations
	$(COMPOSE) run --build --rm --no-deps api migrate up

migrate-down: ## Roll back the last migration (STEPS=n or STEPS=all for more)
	$(COMPOSE) run --build --rm --no-deps api migrate down $(or $(STEPS),1)

migrate-status: ## Show which migrations are applied
	$(COMPOSE) run --build --rm --no-deps api migrate status

seed: ## Load seed data from infra/db/seed
	$(COMPOSE) run --build --rm --no-deps api seed

worker: ## Run an extra background job worker next to the one inside the API (Ctrl-C to stop)
	$(COMPOSE) run --rm --no-deps -e WORKER_ID=worker-$$$$ api worker

test: check-contracts lint test-api test-ai test-web ## Contract freshness, lint, then every test suite

lint: lint-api lint-ai ## Lint every service

lint-api: ## gofmt and go vet
	@cd api && unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt: run gofmt -w on:"; echo "$$unformatted"; exit 1; fi
	cd api && go vet ./...

lint-ai: ## ruff check, ruff format --check and pyright (strict) on the AI service
	cd ai && $(PYTHON) -m ruff check . && $(PYTHON) -m ruff format --check . && $(PYTHON) -m pyright

fmt-ai: ## Fix what ruff can and reformat the AI service
	cd ai && $(PYTHON) -m ruff check --fix . && $(PYTHON) -m ruff format .

test-api: ## Go API tests
	cd api && go test ./...

test-ai: ## Python AI service tests
	cd ai && $(PYTHON) -m pytest -q

test-db: ## Migration round-trip, API CRUD and job queue tests against the compose DB (each test gets a throwaway database)
	@url="$$(grep '^DATABASE_URL=' .env 2>/dev/null | cut -d= -f2-)"; \
	if [ -z "$$url" ]; then echo "test-db: DATABASE_URL not set in .env (run make up first)"; exit 1; fi; \
	cd api && TEST_DATABASE_URL="$$url" go test ./internal/db/ ./internal/server/ ./internal/jobs/ ./internal/tasks/ -v -count=1

test-web: ## Web typecheck + tests
	cd web && npm run typecheck && npm test

generate: generate-ai-spec generate-api generate-web ## Regenerate every contract artifact from the specs

generate-ai-spec: ## Export the FastAPI app's OpenAPI document to ai/openapi.json
	cd ai && $(PYTHON) -m app.openapi

generate-api: generate-ai-spec ## Go types from api/openapi.yaml and ai/openapi.json (oapi-codegen, pinned in api/go.mod)
	cd api && go generate ./...

generate-web: ## TypeScript types from api/openapi.yaml (openapi-typescript)
	cd web && npm run generate

check-contracts: ## Fail if regenerating changes any generated contract file (i.e. a spec changed without `make generate`)
	@before="$$(git hash-object $(GENERATED) 2>/dev/null)"; \
	$(MAKE) --no-print-directory generate >/dev/null; \
	after="$$(git hash-object $(GENERATED))"; \
	if [ "$$before" != "$$after" ]; then \
	  echo "check-contracts: these generated files were out of date and have been regenerated; review and commit them:"; \
	  paste <(echo "$$before") <(echo "$$after") <(printf '%s\n' $(GENERATED)) | awk '$$1 != $$2 {print "  " $$3}'; \
	  exit 1; fi
	@echo "check-contracts: ok"

health: ## Hit every health endpoint
	@for s in "api http://localhost:$${API_PORT:-8080}/health" \
	          "ai  http://localhost:$${AI_PORT:-8000}/health" \
	          "web http://localhost:$${WEB_PORT:-5173}/"; do \
	  set -- $$s; printf "  %-4s %s -> " $$1 $$2; \
	  curl -sf -o /dev/null -w "%{http_code}\n" $$2 || echo "unreachable"; \
	done
