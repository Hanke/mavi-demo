.DEFAULT_GOAL := help
SHELL := /bin/bash

COMPOSE := docker compose

# Python for the AI service: the checkout's venv when present, else whatever is on PATH.
PYTHON ?= $(shell test -x $(CURDIR)/ai/.venv/bin/python && echo $(CURDIR)/ai/.venv/bin/python || echo python)

# Every file a generator writes. `make check-contracts` fails when one is stale.
GENERATED := ai/openapi.json api/internal/aiclient/types.gen.go api/internal/contract/types.gen.go web/src/api/schema.d.ts

# `make smoke` runs a copy of the stack of its own: a separate compose project (its own
# containers and volumes, so an empty database) on its own host ports, next to whatever
# `make up` is running. The developer's .env is not read: the providers are the key-free
# ones and everything else is the compose file's default.
SMOKE_API_PORT ?= 18080
SMOKE_AI_PORT ?= 18000
SMOKE_POSTGRES_PORT ?= 15433
SMOKE_COMPOSE := LLM_PROVIDER=fake EMBEDDING_PROVIDER=local ANTHROPIC_API_KEY= OPENAI_API_KEY= \
	API_PORT=$(SMOKE_API_PORT) AI_PORT=$(SMOKE_AI_PORT) POSTGRES_PORT=$(SMOKE_POSTGRES_PORT) \
	$(COMPOSE) --env-file /dev/null -p mavi-smoke

.PHONY: help up down logs ps migrate migrate-down migrate-status seed seed-render seed-generate fixtures-render rubric-render eval cache-clear worker run-metrics test test-api test-ai test-db test-web smoke health \
        lint lint-api lint-ai lint-web fmt-ai generate generate-ai-spec generate-api generate-web check-contracts

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

seed: ## Load seed data from infra/db/seed (~200 candidates, sample roles; queues their embeddings)
	$(COMPOSE) run --build --rm --no-deps api seed

seed-render: ## Rewrite infra/db/seed/*.sql from infra/db/seed/data/*.json (no API key)
	cd ai && $(PYTHON) -m app.seedgen render

seed-generate: ## Rewrite the synthetic candidates with the model (needs ANTHROPIC_API_KEY in .env), then render; SLOTS=3,17 or 1-20 for a subset, NO_CACHE=1 to bypass the response cache
	@set -a; [ -f .env ] && . ./.env; set +a; \
	cd ai && $(PYTHON) -m app.seedgen generate $(if $(SLOTS),--only $(SLOTS)) $(if $(NO_CACHE),--no-cache) && $(PYTHON) -m app.seedgen render

eval: ## Score the parsers and the reranker on infra/fixtures with the configured provider; PROVIDER=fake for no key, NO_CACHE=1 to bypass the response cache
	@set -a; [ -f .env ] && . ./.env; set +a; \
	cd ai && $(PYTHON) -m app.eval $(if $(PROVIDER),--provider $(PROVIDER)) $(if $(NO_CACHE),--no-cache)

cache-clear: ## Delete the cached LLM and embedding responses in ai/.cache
	rm -rf ai/.cache

fixtures-render: ## Rewrite infra/fixtures/resumes/*.pdf from the .txt files (no API key)
	cd ai && $(PYTHON) -m app.fixtures render

rubric-render: ## Rewrite the tables of docs/rerank-rubric.md from ai/app/rubric.py (no API key)
	cd ai && $(PYTHON) -m app.rubric

worker: ## Run an extra background job worker next to the one inside the API (Ctrl-C to stop)
	$(COMPOSE) run --rm --no-deps -e WORKER_ID=worker-$$$$ api worker

run-metrics: ## What the matching runs took and cost: time per stage, LLM calls, tokens and estimated cost per run, then the averages
	@$(COMPOSE) exec -T db sh -c 'psql -X -q -P pager=off -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" -f -' < infra/db/reports/run_metrics.sql

test: check-contracts lint test-api test-ai test-web ## Contract freshness, lint, then every test suite

lint: lint-api lint-ai lint-web ## Lint every service

lint-api: ## gofmt and go vet
	@cd api && unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt: run gofmt -w on:"; echo "$$unformatted"; exit 1; fi
	cd api && go vet -tags smoke ./...

lint-ai: ## ruff check, ruff format --check and pyright (strict) on the AI service
	cd ai && $(PYTHON) -m ruff check . && $(PYTHON) -m ruff format --check . && $(PYTHON) -m pyright

lint-web: ## eslint on the web app
	cd web && npm run lint

fmt-ai: ## Fix what ruff can and reformat the AI service
	cd ai && $(PYTHON) -m ruff check --fix . && $(PYTHON) -m ruff format .

test-api: ## Go API tests
	cd api && go test ./...

test-ai: ## Python AI service tests
	cd ai && $(PYTHON) -m pytest -q

test-db: ## Migration round-trip, API CRUD, job queue and end-to-end pipeline tests against the compose DB (each test gets a throwaway database)
	@url="$$(grep '^DATABASE_URL=' .env 2>/dev/null | cut -d= -f2-)"; \
	if [ -z "$$url" ]; then echo "test-db: DATABASE_URL not set in .env (run make up first)"; exit 1; fi; \
	cd api && TEST_DATABASE_URL="$$url" go test ./internal/db/ ./internal/server/ ./internal/jobs/ ./internal/tasks/ ./internal/e2e/ -v -count=1

smoke: ## Smoke test: start a throwaway copy of the stack (fake LLM, no key), drive one role from resume upload to release over HTTP, remove it
	@$(SMOKE_COMPOSE) down -v --remove-orphans >/dev/null 2>&1 || true
	@trap 'status=$$?; [ $$status -eq 0 ] || $(SMOKE_COMPOSE) logs --no-color --tail 100 api ai; $(SMOKE_COMPOSE) down -v --remove-orphans; exit $$status' EXIT; \
	set -e; \
	$(SMOKE_COMPOSE) up --build -d --wait db ai api; \
	$(SMOKE_COMPOSE) run --rm --no-deps api migrate up; \
	cd api && SMOKE_API_URL=http://localhost:$(SMOKE_API_PORT) go test -tags smoke -run '^TestSmoke$$' -count=1 -v ./internal/e2e/

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
