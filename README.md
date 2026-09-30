# Mavi

[![CI](https://github.com/Hanke/mavi-demo/actions/workflows/ci.yml/badge.svg)](https://github.com/Hanke/mavi-demo/actions/workflows/ci.yml)

Monorepo for the Mavi demo stack.

| Path | What | Port |
| --- | --- | --- |
| [`web/`](web/) | React + TypeScript (Vite dev server); API types generated from `api/openapi.yaml` | 5173 |
| [`api/`](api/) | Go HTTP API | 8080 |
| [`ai/`](ai/) | Python FastAPI service (embeddings / LLM) | 8000 |
| [`infra/`](infra/) | Postgres init scripts, SQL migrations, seed data, shared taxonomy | 5433 (host) → 5432 |

## Quick start

Requires Docker Desktop (Compose v2).

```sh
cp .env.example .env     # add provider keys if you want real LLM/embedding calls
make up                  # docker compose up --build -d, then prints health
make migrate             # apply infra/db/migrations
make seed                # load infra/db/seed: ~200 synthetic candidates, sample roles, and their embed jobs
```

Then open <http://localhost:5173>. The page calls the API's `/health`, which in
turn checks Postgres and the AI service, so a green status means all four
services are wired together.

## Health endpoints

- `GET http://localhost:8080/health` — API; `200 {"status":"ok","checks":{"postgres":"ok","ai":"ok"}}`, or `503` with the failing check named.
- `GET http://localhost:8000/health` — AI service.
- `GET http://localhost:5173/` — web dev server.

## API

The Go service is the system of record: JSON CRUD for candidates, their
profiles, roles and matches, all against Postgres. There is no real auth. Every
request outside `/health` picks a persona with the `X-Role` header (`talent`,
`employer` or `ops`; the `mavi_role` cookie works as a session fallback) and may
identify itself with `X-Actor`: the candidate id for talent, an email for ops.
A missing or unknown role is a `401`; a known role calling an endpoint it may
not use is a `403`.

| Endpoint | talent | employer | ops |
| --- | --- | --- | --- |
| `POST /candidates` | yes (marked `source: self`) | | yes |
| `GET /candidates` | | | yes |
| `GET` / `PUT /candidates/{id}` | own record only (`X-Actor` = id) | | yes |
| `DELETE /candidates/{id}` | | | yes |
| `GET` / `PUT /candidates/{id}/profile` | own record only | | yes |
| `DELETE /candidates/{id}/profile` | | | yes |
| `POST` / `PUT` / `DELETE /roles…` | | yes | yes |
| `GET /roles`, `GET /roles/{id}` | open roles only | yes | yes |
| `POST` / `PUT` / `DELETE /matches…` | | | yes |
| `POST /matches/{id}/release`, `…/unrelease` | | | yes |
| `GET /matches`, `GET /matches/{id}` | own, **released only** | **released only** | all |
| `POST /jobs`, `GET /jobs`, `GET /jobs/{id}` | | | yes |

Rules worth knowing:

- **Employers only ever see released matches.** `matches.released_at` is set by
  `POST /matches/{id}/release` (ops) and cleared by `…/unrelease`; both are
  recorded in `review_events` with the `X-Actor` value. The employer filter is
  applied inside the SQL, so no query parameter can widen it, and an unreleased
  match is a `404` for an employer rather than a `403`.
- **Talent is scoped by `X-Actor`.** A talent request without it is a `403`;
  another candidate's record is a `404`.
- **Taxonomy values are accepted as free text** (`"QuickBooks Online"`, `"QBO"`)
  and stored as canonical ids (`quickbooks`). Anything the taxonomy does not know
  is a `422` naming the value, so a hard-filter column never holds a value that
  cannot match.
- `PUT` on candidates and roles replaces the fields you send and keeps the rest;
  an explicit `null` clears an optional field and is a `422` on a required one
  (`full_name`, `title`, `status`). `PUT …/profile` replaces the whole profile
  and returns `201` when it created one.
- Validation errors are `422 {"error": "validation failed", "fields": {...}}`;
  malformed or unknown JSON fields are `400`; a duplicate email or (role,
  candidate) pair is `409`; a delete blocked by review history is `409`.
- Lists take `limit` (default 50, max 200) and `offset`, plus `status` and, for
  matches, `role_id` / `candidate_id`.

```sh
# employer creates a role, ops matches a seeded candidate, releases it
curl -s -X POST localhost:8080/roles -H 'X-Role: employer' -H 'Content-Type: application/json' \
  -d '{"title":"Controller","company":"Acme","required_software":["QBO"]}'
curl -s -X POST localhost:8080/matches -H 'X-Role: ops' -H 'Content-Type: application/json' \
  -d '{"role_id":"<role id>","candidate_id":"11111111-0000-0000-0000-000000000001","score":0.9}'
curl -s localhost:8080/matches -H 'X-Role: employer'                       # []
curl -s -X POST localhost:8080/matches/<match id>/release -H 'X-Role: ops' -H 'X-Actor: ops@example.com'
curl -s localhost:8080/matches -H 'X-Role: employer'                       # [ {...released_at...} ]
```

The AI service client (`api/internal/aiclient`) bounds every call with a
per-operation timeout (3s health, 15s embed) and reports failures as an `*Error`
that says whether the service was unreachable, timed out, rejected the request
or returned something unparseable (`errors.Is(err, aiclient.ErrTimeout)` etc.),
with the service's `detail` message attached. Its request and response types
are generated from the AI service's own OpenAPI document (see
[Contracts](#contracts)).

## Background jobs

Slow work is not done inside a request. The API writes a row to the `jobs`
table and a worker picks it up; there is no broker to run. Today the kinds
are `embed_role` and `embed_profile`: creating or editing a role with a
description, or saving a profile, leaves the row's embedding `NULL` and
queues the job that fills it through the AI service's `/embed`. (Profile
extraction and matching runs will be further kinds.)

How it works (`api/internal/jobs`, handlers in `api/internal/tasks`):

- **Claim.** A worker runs one `UPDATE … WHERE id = (SELECT … WHERE status =
  'queued' AND run_at <= now() ORDER BY priority DESC, run_at, id FOR UPDATE
  SKIP LOCKED LIMIT 1)`. The row lock means two workers can never take the
  same job, and `SKIP LOCKED` means they never wait on each other. The claim
  sets `running`, `locked_by` (the worker id) and counts the attempt in one
  statement.
- **Finish.** A handler returning `nil` marks the job `succeeded`. Any other
  error puts it back to `queued` with `run_at` pushed out by a backoff (5s,
  20s, 80s, … capped at 10m) until `attempts` reaches `max_attempts` (default
  3), when it lands in `failed`. Either way `last_error` holds the message. A
  handler can return `jobs.Permanent(err)` to fail at once; a panic counts as
  a failure. Finishing statements match on `locked_by` and the attempt
  number, so a slow attempt whose lock expired cannot overwrite the result of
  the claim that took over, even in the same worker.
- **Dedupe.** One queued job per `(kind, payload)` (a partial unique index).
  Saving a role five times while its embedding is still waiting queues one
  job; `POST /jobs` answers 200 with the waiting job instead of a 201. Once
  the job is running or finished, the same payload can be queued again.
- **Shutdown.** On SIGTERM the worker stops claiming, gives in-flight handlers
  a grace period (5s), then cancels them; a job interrupted this way goes back
  to `queued` without spending an attempt.
- **Crashes.** A job left `running` longer than `WORKER_LOCK_TIMEOUT` (default
  5m) is reclaimed by any worker's sweeper: back to `queued`, or `failed` if
  its attempts are spent. Keep the timeout above the slowest handler, or a
  live job runs twice. Handlers are written to be re-runnable.
- **Where it runs.** The API container runs `WORKER_CONCURRENCY` (default 2)
  workers in-process, so `make up` is enough. `make worker` starts another
  worker container against the same table (or run `api worker` anywhere with
  `DATABASE_URL` and `AI_SERVICE_URL`); set `WORKER_CONCURRENCY=0` to make the
  API process HTTP-only. Each process names itself `<hostname>-<pid>` in
  `locked_by` unless `WORKER_ID` says otherwise, so scaled replicas stay
  distinct.
- **Enqueue from a write.** The role and profile handlers enqueue after the
  row is committed, on a context detached from the request, so a client that
  disconnects at that moment does not lose the job.

The queue is visible to ops over the API for the UI to poll:

```sh
# a role with a description queues its embedding; poll until succeeded / failed
curl -s -X POST localhost:8080/roles -H 'X-Role: employer' -H 'Content-Type: application/json' \
  -d '{"title":"Controller","description":"Owns the monthly close."}'
curl -s 'localhost:8080/jobs?kind=embed_role' -H 'X-Role: ops'        # newest first
curl -s localhost:8080/jobs/1 -H 'X-Role: ops'                       # {"status":"succeeded","attempts":1,...}

# enqueue by hand (kind must be one the worker has a handler for; otherwise 422)
curl -s -X POST localhost:8080/jobs -H 'X-Role: ops' -H 'Content-Type: application/json' \
  -d '{"kind":"embed_role","payload":{"role_id":"<role id>"},"priority":5,"max_attempts":5}'
curl -s 'localhost:8080/jobs?status=failed' -H 'X-Role: ops'          # last_error says why
```

`make test-db` runs the queue's tests against the compose database: a job is
enqueued, claimed and completed; a failing job retries and lands in `failed`
with its error; sixty jobs shared by two concurrent workers each run exactly
once; a worker shut down mid-job hands it back; stale locks are reclaimed.

## Contracts

The three services share their shapes through two OpenAPI documents, each
written in one place and generated into the others. Nothing hand-writes a
duplicate: the Go store's row types, the handlers' request types and the web
app's types all come out of the generators.

| Contract | Source of truth | Generated into |
| --- | --- | --- |
| Web ↔ Go API | [`api/openapi.yaml`](api/openapi.yaml), hand-written | `api/internal/contract/types.gen.go` (Go, [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen), pinned as a `tool` in `api/go.mod`) and `web/src/api/schema.d.ts` (TypeScript, [openapi-typescript](https://openapi-ts.dev)) |
| Go API → AI service | [`ai/openapi.json`](ai/openapi.json), exported from the FastAPI app by `python -m app.openapi` | `api/internal/aiclient/types.gen.go` (Go, oapi-codegen) |

```sh
make generate         # re-export ai/openapi.json, regenerate all Go and TS types
make check-contracts  # regenerate and fail if anything changed, i.e. a spec moved without its outputs (make test runs this first)
```

What a schema change does:

- **Edit `api/openapi.yaml`** (say, rename a `Match` field). `make generate`
  rewrites the Go and TypeScript types; `go build` fails in every handler or
  store scan that still uses the old name, and `npm run typecheck` fails in
  every component that does. The server's route table is checked against the
  spec's paths and `x-roles` by `TestRoutesMatchOpenAPISpec`, so an endpoint or
  a permission that exists in only one place fails `go test`.
- **Edit a FastAPI model** in `ai/app/main.py` or `ai/app/schemas.py`.
  `tests/test_openapi.py` fails until `ai/openapi.json` is re-exported;
  regenerating then changes `aiclient/types.gen.go`, and `go build` fails
  wherever the Go client used the old shape.
- **Forget to run the generator** after editing a spec: `make check-contracts`
  (and so `make test` and CI) regenerates, sees the output change, and fails
  naming the file. It compares against the working tree, not the last commit,
  so a freshly regenerated tree passes before it is committed.

In the web app, `src/api/index.ts` exposes a typed
[openapi-fetch](https://openapi-ts.dev/openapi-fetch/) client, so a call like
`client.GET("/matches", { params: { query: { status: "released" } } })` is a
type error, and `src/api/types.ts` gives the schemas short names (`Candidate`,
`Match`, `Persona`, …). Add an alias there when a schema lands in the YAML;
never declare a shape.

Two things about the exported AI document: it is downgraded from OpenAPI 3.1
to 3.0.3 (`anyOf [X, null]` becomes `nullable`), which is what oapi-codegen
reads, and the parser output models (`CandidateProfile`, `RoleRequirements`)
are included as components even though no endpoint returns them yet, with
the taxonomy id enums stripped so a taxonomy edit is not a schema change
(their never-null list fields are marked so Go gets plain slices).
The Go API stores those as free-form JSON (`profile`, `requirements`); the
match `breakdown` is free-form too, since no service defines a rubric yet.

## Make targets

Run `make` to list them. The main ones:

- `make up` / `make down` / `make logs` / `make ps`
- `make migrate` / `make migrate-down` / `make migrate-status` / `make seed` — run inside the `api` image against the compose DB. `make migrate-down STEPS=2` or `STEPS=all` rolls back further.
- `make seed-render` / `make seed-generate` — rewrite the seed SQL from the committed JSON, or regenerate the synthetic candidates with the model first (see [Seed data](#seed-data))
- `make worker` — an extra background job worker container next to the one inside the API (see [Background jobs](#background-jobs))
- `make test-db` — migration up/down round-trip, the API CRUD / role tests and the job queue tests against the compose DB (each test creates and drops a throwaway database)
- `make generate` / `make check-contracts` — regenerate the shared types from the OpenAPI documents, or fail if regenerating changes anything (see [Contracts](#contracts))
- `make lint` — `gofmt` + `go vet` for the API; `ruff check`, `ruff format --check` and strict `pyright` for the AI service; `eslint` for the web app (`make fmt-ai` fixes what ruff can)
- `make test` — contract check, lint, then the Go, Python and web test suites (host toolchains: Go 1.24, Python 3.12+, Node 22)
- CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs on every push to `main` and every pull request: `go vet` + `go test`, `ruff` + `pytest`, and the web typecheck, lint, tests and build, one job per service. It sets `EMBEDDING_PROVIDER=local` and no provider keys, so nothing in CI calls an LLM.
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
| `matches` | One row per (role, candidate): `score`, `explanation`, `breakdown`, `status` (proposed / approved / rejected / swapped), `released_at` (set when ops releases it to the employer) |
| `review_events` | Append-only audit of ops approve / reject / swap / release / unrelease actions on a match |
| `jobs` | Postgres-backed background queue: `kind`, `payload`, `status` (queued / running / succeeded / failed), `priority`, `run_at`, `attempts` / `max_attempts`, `last_error`, `locked_by` / `locked_at`; claimed with `FOR UPDATE SKIP LOCKED` on the partial `jobs_dequeue_idx` (see [Background jobs](#background-jobs)) |

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

## Seed data

`make seed` loads [`infra/db/seed`](infra/db/seed) in file-name order:

| File | What |
| --- | --- |
| `010_candidates.sql` | ~200 synthetic finance / accounting candidates: raw resume text plus a structured profile with the hard-filter columns filled |
| `020_roles.sql` | A dozen sample job descriptions whose hard filters each carve a different slice of those candidates |
| `030_documents.sql` | Three demo documents |
| `090_embed_jobs.sql` | Queues an `embed_profile` / `embed_role` job for every row still without a vector, so the worker in the API container fills the embeddings (no provider key needed with `EMBEDDING_PROVIDER=local`) |

Every file is idempotent (fixed ids, `ON CONFLICT DO NOTHING`), so `make seed`
is additive: it never updates a row that already exists. For a clean slate,
`make migrate-down STEPS=all && make migrate && make seed`.

The two SQL files are **generated** from
[`infra/db/seed/data/candidates.json`](infra/db/seed/data/candidates.json) and
[`roles.json`](infra/db/seed/data/roles.json) by `make seed-render`; edit the
JSON, not the SQL (`tests/test_seed_data.py` fails when they disagree). The
candidates come from `ai/app/seedgen`, in two halves:

- **The plan** (`plan.py`) is code: a seeded RNG assigns each of the 200 slots
  a role family (bookkeeper, AP/AR, payroll, staff and senior accountant,
  controller, FP&A, tax, audit, revenue, cost, treasury, international,
  fractional CFO, nonprofit), years of experience, certifications (about a
  third hold a CPA; CMA, CIA, EA, CPP, ACCA, CA and others appear in smaller
  numbers; roughly half hold none), software (QuickBooks and NetSuite most
  common, then SAP, Dynamics, Oracle, Sage Intacct, Xero, the FP&A and close
  tools, payroll systems), GAAP exposure, industries, availability
  (`immediate` through `unknown`, with a relative `available_in_days`),
  timezone (mostly US zones, some Toronto, London, Manila, Bengaluru, Sydney
  and others) and languages. `python -m app.seedgen plan` prints the
  distribution. Because the plan decides the facts, the hard filters and the
  ranking visibly change results no matter what the prose says.
- **The prose** is written by the model from each slot's spec: a 260-450 word
  resume with quotable, specific bullets, a headline and a skills list. Every
  certification and software product in the spec must be named in the resume
  (by label or a taxonomy alias), and the structured profile is validated as a
  `CandidateProfile`, so the parser and the evidence quotes have real text to
  work on. `make seed-generate` does this over the API (`ANTHROPIC_API_KEY` in
  `.env`; `SLOTS=3,17` or `SLOTS=1-20` regenerates a subset). Without a key,
  `python -m app.seedgen specs --slots 1-20` prints the same brief and specs
  for a model session to write, and `python -m app.seedgen ingest <file>`
  applies the same checks and merges the result. The committed file records
  which model wrote it.

No real candidate data is involved: names, employers, emails and phone numbers
are invented, and emails use `example.com`.

## Taxonomy

The hard filters compare a role's `required_certifications` / `required_software`
with a profile's `certifications` / `software` using array containment, which only
works if both sides hold the same canonical values. [`infra/taxonomy.json`](infra/taxonomy.json)
is the single source of those values: three lists (`certifications`, `software`,
`industries`), each entry an `id` (snake_case, never renamed), a `label` and
`aliases`. "QuickBooks Online", "QBO" and "Quickbooks" all resolve to `quickbooks`;
"CPA" and "Certified Public Accountant" resolve to `cpa`.

Who reads it:

- **Parsers** (`ai/app/schemas.py`): `CandidateProfile` (resume) and `RoleRequirements`
  (JD) resolve every taxonomy list to ids before validation. Values that do not
  resolve move to the matching `other_*` free-text field rather than being dropped,
  and the JSON schema handed to the model carries the id list as an enum.
- **Seed** (`api seed`): the seed files and a check of the four hard-filter
  columns run in one transaction (the generated seed writes canonical ids
  by construction; the check still runs). A value that is not a canonical id rolls the
  whole seed back, naming the offending value and the id it should have been.
- **Filters / API write path** (`api/internal/taxonomy`): `Resolve` / `ResolveAll`
  give the Go side the same mapping, so anything the API writes to those columns
  goes through the taxonomy first.

Both implementations share the key rules (lower-case, `&` → `and`, drop
free-standing parentheticals, drop credential words like "certified" / "license"
for certifications only, drop every non-alphanumeric character) and both test suites run
[`infra/taxonomy_cases.json`](infra/taxonomy_cases.json), so they cannot drift apart.
To add a term, add an entry with aliases; to accept a new spelling, add an alias.
The file is mounted into the `ai` and `api` containers at `/app/infra` and read at
runtime via `TAXONOMY_PATH`, so edits do not need a rebuild.

## Local dev without Docker

Each service runs standalone against `DATABASE_URL` / `AI_SERVICE_URL` from `.env`.
The AI service's `requirements-dev.txt` adds pytest, ruff and pyright on top of
the runtime `requirements.txt` the Docker image installs; the lint rules live
in `ai/pyproject.toml` and `ai/pyrightconfig.json`.

```sh
(cd api && go run ./cmd/api)
(cd ai && python3.12 -m venv .venv && .venv/bin/pip install -r requirements-dev.txt && .venv/bin/uvicorn app.main:app --reload)
(cd web && npm install && npm run dev)
```

## Provider configuration

`WORKER_CONCURRENCY` (default 2) is how many background jobs the API process
runs at once; `0` disables its worker. `WORKER_ID` names the process in
`jobs.locked_by`; `WORKER_LOCK_TIMEOUT` (default `5m`) is how long a running
job may go unfinished before another worker reclaims it.


`EMBEDDING_PROVIDER=local` (the default) uses a deterministic hash-based stub so
the stack runs with no API keys. Set it to `openai` with `OPENAI_API_KEY` for real
embeddings. `LLM_PROVIDER` selects `anthropic` or `openai` for chat calls; the
matching key must be set.
