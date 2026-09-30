# Mavi

Monorepo for the Mavi demo stack.

| Path | What | Port |
| --- | --- | --- |
| [`web/`](web/) | React + TypeScript (Vite dev server) | 5173 |
| [`api/`](api/) | Go HTTP API | 8080 |
| [`ai/`](ai/) | Python FastAPI service (embeddings / LLM) | 8000 |
| [`infra/`](infra/) | Postgres init scripts, SQL migrations, seed data | 5433 (host) → 5432 |

## Quick start

Requires Docker Desktop (Compose v2).

```sh
cp .env.example .env     # add provider keys if you want real LLM/embedding calls
make up                  # docker compose up --build -d, then prints health
make migrate             # apply infra/db/migrations
make seed                # load infra/db/seed
```

Then open <http://localhost:5173>. The page calls the API's `/health`, which in
turn checks Postgres and the AI service, so a green status means all four
services are wired together.

## Health endpoints

- `GET http://localhost:8080/health` — API; `200 {"status":"ok","checks":{"postgres":"ok","ai":"ok"}}`, or `503` with the failing check named.
- `GET http://localhost:8000/health` — AI service.
- `GET http://localhost:5173/` — web dev server.

## Make targets

Run `make` to list them. The main ones:

- `make up` / `make down` / `make logs` / `make ps`
- `make migrate` / `make seed` — run inside the `api` image against the compose DB
- `make test` — Go, Python and web test suites (host toolchains: Go 1.24, Python 3.12+, Node 22)
- `make health` — curl every health endpoint

## Local dev without Docker

Each service runs standalone against `DATABASE_URL` / `AI_SERVICE_URL` from `.env`:

```sh
(cd api && go run ./cmd/api)
(cd ai && python -m venv .venv && .venv/bin/pip install -r requirements.txt && .venv/bin/uvicorn app.main:app --reload)
(cd web && npm install && npm run dev)
```

## Provider configuration

`EMBEDDING_PROVIDER=local` (the default) uses a deterministic hash-based stub so
the stack runs with no API keys. Set it to `openai` with `OPENAI_API_KEY` for real
embeddings. `LLM_PROVIDER` selects `anthropic` or `openai` for chat calls; the
matching key must be set.
