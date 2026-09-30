.DEFAULT_GOAL := help
SHELL := /bin/bash

COMPOSE := docker compose

.PHONY: help up down logs ps migrate seed test test-api test-ai test-web health

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

migrate: ## Apply SQL migrations in infra/db/migrations
	$(COMPOSE) run --rm --no-deps api migrate

seed: ## Load seed data from infra/db/seed
	$(COMPOSE) run --rm --no-deps api seed

test: test-api test-ai test-web ## Run all test suites

test-api: ## Go API tests
	cd api && go test ./...

test-ai: ## Python AI service tests
	cd ai && python -m pytest -q

test-web: ## Web typecheck + tests
	cd web && npm run typecheck && npm test

health: ## Hit every health endpoint
	@for s in "api http://localhost:$${API_PORT:-8080}/health" \
	          "ai  http://localhost:$${AI_PORT:-8000}/health" \
	          "web http://localhost:$${WEB_PORT:-5173}/"; do \
	  set -- $$s; printf "  %-4s %s -> " $$1 $$2; \
	  curl -sf -o /dev/null -w "%{http_code}\n" $$2 || echo "unreachable"; \
	done
