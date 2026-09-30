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
- `make migrate` / `make migrate-down` / `make migrate-status` / `make seed` — run inside the `api` image against the compose DB. `make migrate-down STEPS=2` or `STEPS=all` rolls back further.
- `make test-db` — migration up/down round-trip against the compose DB (creates and drops a throwaway database)
- `make test` — Go, Python and web test suites (host toolchains: Go 1.24, Python 3.12+, Node 22)
- `make health` — curl every health endpoint

## Database

Migrations live in `infra/db/migrations` as paired `NNNN_name.up.sql` /
`NNNN_name.down.sql` files and are applied by the Go API (`api migrate up|down|status`).
Each migration commits together with its `schema_migrations` row, and the runner
takes an advisory lock so two runs cannot interleave. The Python service never
touches these tables; it only produces embeddings and structured extractions that
the API writes.

| Table | Purpose |
| --- | --- |
| `candidates` | The person as ingested: contact details, raw resume text, status |
| `candidate_profiles` | One structured profile per candidate: `profile` JSONB, `embedding` (pgvector, HNSW index), and the hard-filter columns `certifications[]`, `software[]`, `availability`, `available_from`, `timezone` |
| `roles` | Raw JD plus `must_haves` / `nice_to_haves` JSONB, promoted `required_certifications[]`, `required_software[]`, `timezone`, `starts_on`, and an embedding |
| `matches` | One row per (role, candidate): `score`, `explanation`, `breakdown`, `status` (proposed / approved / rejected / swapped) |
| `review_events` | Append-only audit of ops approve / reject / swap actions on a match |
| `jobs` | Postgres-backed background queue (`FOR UPDATE SKIP LOCKED` dequeue on the partial `jobs_dequeue_idx`) |

The hard-filter fields are real, indexed columns rather than JSON keys, so a
shortlist query can be written directly in SQL:

```sql
SELECT c.full_name, p.certifications, p.software, p.timezone
FROM roles r
JOIN candidate_profiles p
  ON p.certifications @> r.required_certifications
 AND p.software       @> r.required_software
 AND (r.starts_on IS NULL OR p.available_from <= r.starts_on)
JOIN candidates c ON c.id = p.candidate_id
WHERE r.id = $1 AND c.status = 'active'
ORDER BY p.embedding <=> r.embedding
LIMIT 20;
```

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
