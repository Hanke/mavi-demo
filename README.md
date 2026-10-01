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
- `GET http://localhost:8000/health` — AI service; `200 {"status":"ok","llm_provider":"anthropic","embedding_provider":"local"}`.
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

## AI service

The Python service ([`ai/`](ai/)) is stateless and never touches the database:
five POST endpoints plus `/health`, every request and response a Pydantic model
(the OpenAPI document at `/openapi.json` is exported to
[`ai/openapi.json`](ai/openapi.json) for the Go client, see [Contracts](#contracts)).

| Endpoint | Request | Response |
| --- | --- | --- |
| `POST /parse-resume` | `{text, as_of?}` — the resume as plain text; `as_of` is the date `years_experience` counts to (default today) | `{contact, profile, provider}` — `contact` is the header (`full_name`, `email`, `phone`, `location`, each null when the resume does not give it); `profile` is a `CandidateProfile`: `positions` (title, employer, start and end year), `years_experience`, `certifications`, `software`, `industries` (taxonomy ids, already canonical), `qualifications` (each one as written, with its issuing body, jurisdiction and whether it is fully held; see [Qualifications](#qualifications-across-jurisdictions)), `gaap_exposure` (the frameworks and standards the resume names), plus headline, skills, languages, availability and time zone |
| `POST /parse-jd` | `{text}` | `{company, requirements, provider}` — `requirements` is a `RoleRequirements`, split into must-haves and nice-to-haves in fields that line up with the candidate profile (see [JD requirements](#jd-requirements)); each entry of `required_qualifications` says whether an equivalent is acceptable (`accept_equivalents`) and whether the JD said so (`equivalents_stated`) |
| `POST /rerank` | `{role, candidates: [{id, text}]}` — the JD (or a rendering of the role) and up to 50 candidates, each an opaque id plus the text to judge | `{results: [{id, score, dimensions, reasons}], rubric_version, provider}` — every id exactly once, best first; `dimensions` is the candidate's level (0 to 4, or null) and evidence on each dimension of the [rerank rubric](#rerank-rubric), and `score` (0..1) is computed from those levels |
| `POST /embed` | `{text}` — up to 60,000 characters, like the parsers | `{embedding, dim, provider}` |
| `POST /embed-batch` | `{inputs: [...]}` — up to 256 inputs, each exactly one of `{text}`, `{profile}` (a `CandidateProfile`) or `{requirements}` (a `RoleRequirements`) | `{embeddings, texts, dim, provider}` — one vector per input in the order given, and the text each was computed from |

### Embeddings

Every vector is `EMBEDDING_DIM` wide (1536, the width of the `vector(1536)`
columns). With `EMBEDDING_PROVIDER=openai` the `text-embedding-3` models are
asked for that width, and an answer of any other width is a `502`, never a
stored vector; so is any provider failure, with the reason in `detail`. A batch
is sent to the provider in as few requests as possible (64 texts each): texts
already in the [response cache](#response-cache) are not sent, a text that
appears twice is sent once, and each request's vectors are stored as it
returns, so retrying a batch that failed part-way pays only for the rest.

A role and a profile are compared by the cosine distance of their vectors, so
both are embedded from text of the same form. `/embed-batch` renders a
`profile` or a `requirements` input to the same labelled lines in the same
order ([`ai/app/embedtext.py`](ai/app/embedtext.py)), with taxonomy ids written
as their labels:

| Line | From a profile | From a role |
| --- | --- | --- |
| `Role` | `headline`, then the titles of `positions` | `title` |
| `Qualifications` | `certifications`, `other_certifications` | required, then preferred |
| `Software` | `software`, `other_software` | required, then preferred |
| `Industries` | `industries`, `other_industries` | the same fields |
| `Standards` | `gaap_exposure` | (a JD names them in its must-haves) |
| `Skills` | `skills` | `must_haves`, then `nice_to_haves` |

An empty line is left out. A must-have is free text, so its clauses (split at
`;`) about location, working hours, start date or the right to work are
recognised by their wording and dropped: "Central time; on-site in Chicago"
says nothing a candidate's skills can answer. That is a word list
(`LOGISTICS`), not a parse; a clause it misses stays in as a little noise, and
the rerank still sees every must-have in full. What a hard filter decides by comparison is not in
the text: time zone, start date, availability, and years of experience (the
role's number is a minimum and the profile's a total, so the two would read as
different when the candidate passes). Neither are employer names, contact
details or languages.

The worker sends the stored documents, not text it builds itself
([`api/internal/tasks/embed.go`](api/internal/tasks/embed.go)):

- **Profile**: `candidate_profiles.profile` with the `headline`,
  `certifications` and `software` columns laid over it where they have a
  value. `profile` is free-form JSON in the API, so one that is not a valid
  `CandidateProfile` is refused (`422`) and embedded as plain text instead
  (headline, the JSON, the two lists).
- **Role**: a role with structured requirements (a non-empty `requirements`
  document, or any of `must_haves`, `nice_to_haves`, `required_certifications`,
  `required_software`) is sent as `requirements` with the title and those
  columns laid over it. A role that has only a description, or whose
  requirements are refused, is embedded from the description; such a vector is
  a stopgap until the JD is parsed, since it is not in the comparable form.

The embedding is cleared (and the job queued again) when anything it was
computed from changes: for a role the title, description, `requirements`,
`must_haves`, `nice_to_haves`, `required_certifications` and
`required_software`; for a profile the `profile` JSON, headline,
certifications and software.

```sh
curl -s localhost:8000/embed-batch -H 'Content-Type: application/json' -d '{"inputs": [
  {"requirements": {"title": "Senior Payroll Specialist", "required_software": ["adp"], "must_haves": ["Multi-state payroll"]}},
  {"profile": {"headline": "Payroll Manager", "software": ["adp"], "skills": ["multi-state payroll"]}}
]}' | jq '{dim, texts}'
```

The three chat endpoints share one client ([`ai/app/llm.py`](ai/app/llm.py)).
Each call sends the answer's JSON schema to the provider (Anthropic
`output_config.format`, or OpenAI's JSON-schema response format when
`LLM_PROVIDER=openai`) and validates the text that comes back with the same
Pydantic model, including the taxonomy resolution in
[`ai/app/schemas.py`](ai/app/schemas.py). Output that fails validation, or a
rerank that drops or invents a candidate id or scores a dimension for some
candidates only, is sent back to the model once
with the validation errors quoted; a second failure is a `502` whose `detail`
starts with `llm output invalid:` and names the fields. Nothing that did not
validate is ever returned. A provider failure (no credentials, rate limit,
refusal, truncation) is a `502` with `detail` starting `llm provider error:`;
the Go client treats both as retryable. Prompts and the extraction rules live
in [`ai/app/extract.py`](ai/app/extract.py).

```sh
curl -s -X POST localhost:8000/parse-jd -H 'Content-Type: application/json' \
  -d @<(jq -Rs '{text: .}' infra/fixtures/jds/senior_accountant_strict.txt)
curl -s -X POST localhost:8000/rerank -H 'Content-Type: application/json' \
  -d '{"role":"Senior Accountant, CPA required, NetSuite","candidates":[{"id":"c1","text":"CPA, 6 years NetSuite close"},{"id":"c2","text":"Bookkeeper, QuickBooks"}]}'
```

The resume parser does not take the model's word for what the resume says.
After validation, `ground_resume` ([`ai/app/extract.py`](ai/app/extract.py))
checks every value that is read off the page against the text: a
certification or software id the resume does not name (by label or taxonomy
alias), an `other_*` entry, standard, job title or employer that is not in
the text verbatim, a qualification whose name is not on the page, a year the
text never writes, or an email, phone number or name that is not there is
dropped or set to null, and logged (a qualification's invented quote is
replaced by the line that does name it). Derived values
(headline, skills, industries, availability, time zone) are left to the
model. Missing information therefore comes back as null or an empty list;
`ai/tests/test_parse_resume.py` runs every sample resume through this with
an answer padded with invented values and asserts none survive.

### Rerank rubric

What `/rerank` scores, the 0 to 4 scale with a written description of every
level, the weights and the formula are set out in
[`docs/rerank-rubric.md`](docs/rerank-rubric.md). In short:

| Dimension | Weight | Question |
| --- | --- | --- |
| `must_have_coverage` | 40% | Is each stated requirement met? |
| `experience_depth` | 25% | How closely does the work done match the work of the role? |
| `software_fluency` | 15% | How well does the candidate know the systems the role names? |
| `industry_fit` | 10% | Has the candidate worked in the role's industry? |
| `nice_to_haves` | 10% | How many of the preferred items does the text show? |

The model gives a level and a sentence of evidence per dimension and never an
overall score (an answer that carries one fails validation). The service
computes `score` in [`ai/app/rubric.py`](ai/app/rubric.py): the weighted mean
of the levels over the dimensions that apply to the role, capped at 0.30 or
0.50 when must-have coverage is 0 or 1, rounded to three decimals. The same
levels always give the same score, so it can be recomputed from the
`dimensions` of any result.

The rubric has one source. The tables in the document are rendered from
`rubric.py` (`make rubric-render`), the system prompt is built from the same
text, and the output schema has one field per dimension;
[`ai/tests/test_rubric.py`](ai/tests/test_rubric.py) fails when any of the
three falls behind. Changing the rubric means editing that module, bumping
its `VERSION` (returned as `rubric_version`) and running
`make rubric-render generate`.

### JD requirements

`/parse-jd` returns the requirements in two tiers. The must-haves are the
hard filters: each one is a field of `RoleRequirements`, a column on `roles`
under the same name, and is compared with one column of `candidate_profiles`
(`HARD_FILTER_COLUMNS` in [`ai/app/schemas.py`](ai/app/schemas.py)):

| Must-have (`RoleRequirements` field and `roles` column) | Compared with (`candidate_profiles`) | How |
| --- | --- | --- |
| `required_certifications` (with `required_qualifications` for the detail) | `certifications` | overlap with the ids acceptable for each requirement |
| `required_software` | `software` | containment |
| `min_years_experience` | `years_experience` | `>=`; a profile with no figure does not pass |
| `starts_on` | `available_from` | `<=` |
| `timezone` | `timezone` | stored on both sides; the shortlist query does not filter on it yet |

`min_years_experience` is the fewest total years the JD accepts: 5 for "5+
years", 1 for "1-4 years", the overall 10 of "10 or more years with at least 5
in manufacturing", and null when the JD gives no number ("a couple of years").
A minimum of 0 is stored as null on both services, since no minimum must not
exclude a profile that does not state its years.

The nice-to-haves are `preferred_certifications` and `preferred_software`
(taxonomy ids, with `other_preferred_*` for anything outside the taxonomy).
They never exclude a candidate, so alternatives are simply all listed ("CPP
or FPC" is both ids), and the validators drop anything that is already
required. Everything a JD asks for that has no structured field stays as text
in `must_haves` / `nice_to_haves`, verbatim, for the rerank.

Whatever the model returns is validated as that schema before it leaves the
service: names become taxonomy ids, a preferred "CPA" becomes the one for
where the role is, and an unknown field or an impossible number of years is
sent back to the model once and then refused. Then `ground_jd`
([`ai/app/extract.py`](ai/app/extract.py)) checks the structured requirements
against the JD text, as `ground_resume` does for a resume: a required or
preferred certification or product the JD does not name, an `other_*` entry
that is not there verbatim, or a minimum number of years the JD never writes
is dropped and logged, so a filter the model invented cannot exclude anyone.
[`ai/tests/test_parse_jd.py`](ai/tests/test_parse_jd.py) runs every sample JD
through the parser with its expected must-haves written out, and checks each
must-have field against the columns in `infra/db/migrations`.

Tests run the endpoints over a `ScriptedProvider` that replays canned answers
(`ai/tests/test_endpoints.py`, `ai/tests/test_llm.py`) and over the key-free
`fake` provider (`ai/tests/test_fake.py`; see
[Provider configuration](#provider-configuration)), so nothing in `make test`
or CI calls a model.

## Background jobs

Slow work is not done inside a request. The API writes a row to the `jobs`
table and a worker picks it up; there is no broker to run. Today the kinds
are `embed_role` and `embed_profile`: creating or editing a role, or saving a
profile, leaves the row's embedding `NULL` and queues the job that fills it
through the AI service's `/embed-batch` (see [Embeddings](#embeddings) for
what is sent). (Profile
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
match `breakdown` is free-form too; the `dimensions` and `rubric_version` of a
rerank result ([Rerank rubric](#rerank-rubric)) are what belongs in it once the
matching run is a job.

## Make targets

Run `make` to list them. The main ones:

- `make up` / `make down` / `make logs` / `make ps`
- `make migrate` / `make migrate-down` / `make migrate-status` / `make seed` — run inside the `api` image against the compose DB. `make migrate-down STEPS=2` or `STEPS=all` rolls back further.
- `make seed-render` / `make seed-generate` — rewrite the seed SQL from the committed JSON, or regenerate the synthetic candidates with the model first (see [Seed data](#seed-data))
- `make fixtures-render` — rewrite the fixture PDFs from their text files (see [Parser fixtures](#parser-fixtures))
- `make rubric-render` — rewrite the tables of `docs/rerank-rubric.md` from `ai/app/rubric.py` (see [Rerank rubric](#rerank-rubric))
- `make worker` — an extra background job worker container next to the one inside the API (see [Background jobs](#background-jobs))
- `make test-db` — migration up/down round-trip, the API CRUD / role tests and the job queue tests against the compose DB (each test creates and drops a throwaway database)
- `make generate` / `make check-contracts` — regenerate the shared types from the OpenAPI documents, or fail if regenerating changes anything (see [Contracts](#contracts))
- `make lint` — `gofmt` + `go vet` for the API; `ruff check`, `ruff format --check` and strict `pyright` for the AI service; `eslint` for the web app (`make fmt-ai` fixes what ruff can)
- `make eval` — score the parsers and the reranker on the fixtures (`PROVIDER=fake` for no key, `NO_CACHE=1` to bypass the response cache; see [Eval](#eval)); `make cache-clear` deletes the cached responses
- `make test` — contract check, lint, then the Go, Python and web test suites (host toolchains: Go 1.24, Python 3.12+, Node 22)
- CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs on every push to `main` and every pull request: `go vet` + `go test`, `ruff` + `pytest`, and the web typecheck, lint, tests and build, one job per service. It sets `LLM_PROVIDER=fake`, `EMBEDDING_PROVIDER=local` and no provider keys, so nothing in CI calls a model.
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
| `roles` | Raw JD plus `must_haves` / `nice_to_haves` JSONB, promoted `required_certifications[]`, `required_software[]`, `min_years_experience`, `timezone`, `starts_on`, and an embedding |
| `matches` | One row per (role, candidate): `score`, `explanation`, `breakdown`, `status` (proposed / approved / rejected / swapped), `released_at` (set when ops releases it to the employer) |
| `review_events` | Append-only audit of ops approve / reject / swap / release / unrelease actions on a match |
| `jobs` | Postgres-backed background queue: `kind`, `payload`, `status` (queued / running / succeeded / failed), `priority`, `run_at`, `attempts` / `max_attempts`, `last_error`, `locked_by` / `locked_at`; claimed with `FOR UPDATE SKIP LOCKED` on the partial `jobs_dequeue_idx` (see [Background jobs](#background-jobs)) |

The hard-filter fields are real, indexed columns rather than JSON keys, so a
shortlist query can be written directly in SQL (each must-have the JD parser
returns has a column here, see [JD requirements](#jd-requirements)). Software is containment. A
required qualification is an overlap with the ids the taxonomy accepts for it
(`taxonomy.Acceptable(id, acceptEquivalents)` in Go, one array parameter per
requirement; see [Qualifications](#qualifications-across-jurisdictions)):

```sql
SELECT c.full_name, p.certifications, p.software, p.timezone
FROM roles r
JOIN candidate_profiles p
  ON p.certifications && $2   -- e.g. {cpa_us,cpa_canada,aca_icaew,ca_icas,acca,...} for "CPA or equivalent"
 AND p.software       @> r.required_software
 AND (r.min_years_experience IS NULL OR p.years_experience >= r.min_years_experience)
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
  third hold a CPA; CMA, CIA, EA, CPP, ACCA and others appear in smaller
  numbers; roughly half hold none; a candidate outside the US holds their own
  country's qualification, so the seed has ACA, ICAS, Chartered Accountants
  Ireland, Canadian CPA, CA ANZ and ICAI members and nobody in London with a
  US licence), software (QuickBooks and NetSuite most
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
  work on. The profile's `positions` and `gaap_exposure` are read off that
  text (the EXPERIENCE heading lines and the standards it names), not taken
  from the model, and so is each `qualifications` record (the line of the
  resume that states it). After a change to the plan or the schema,
  `python -m app.seedgen reassemble` re-derives every stored profile from the
  stored text and lists the slots whose resume no longer carries the plan's
  facts, so only those need new prose. `make seed-generate` does this over the API (`ANTHROPIC_API_KEY` in
  `.env`; `SLOTS=3,17` or `SLOTS=1-20` regenerates a subset). Each batch's
  answer is kept in the [response cache](#response-cache), so running it again
  the same day (the specs carry today's date) only calls the model for batches
  it has not seen; `NO_CACHE=1` forces fresh prose. Without a key,
  `python -m app.seedgen specs --slots 1-20` prints the same brief and specs
  for a model session to write, and `python -m app.seedgen ingest <file>`
  applies the same checks and merges the result. The committed file records
  which model wrote it.

No real candidate data is involved: names, employers, emails and phone numbers
are invented, and emails use `example.com`.

## Parser fixtures

[`infra/fixtures`](infra/fixtures) is a small hand-written set the resume
and JD parser tests, the pipeline tests and the eval all share: thirteen resumes
(plain text plus a PDF of the same text) and seven job descriptions, each with
the structured output the parser is expected to produce.

| Path | What |
| --- | --- |
| `resumes/<slug>.txt` | The resume as plain text, in a deliberately varied style: chronological, competency-block, skills-first, narrative, paragraph-only, a UK CV, a Canadian one, and one messy file (`messy_ap_specialist`: contact details at the bottom, tabs, three bullet styles, typos) |
| `resumes/<slug>.pdf` | The same text rendered to PDF by `make fixtures-render` (`python -m app.fixtures render`); deterministic, so the tests compare it byte for byte with a re-render |
| `resumes/<slug>.expected.json` | `{"$comment", "contact", "profile"}`: the header details and a complete, canonical `CandidateProfile` |
| `jds/<slug>.txt` | The job description; `vague_finance_generalist` has no hard requirements at all and `senior_accountant_strict` has seven, with a fixed start date |
| `jds/<slug>.expected.json` | `{"$comment", "company", "requirements", "hard_filter_matches"}`: a complete `RoleRequirements` and the resume slugs whose expected profile passes its certification, software and years-of-experience filters |

Load them with `app.fixtures.load_resumes()` / `load_jds()`; a parser test
feeds each `text` (or `pdf_path`) in and compares with `expected`. The
`$comment` in every expected file says what that fixture is there to catch.
`tests/test_fixtures.py` keeps the set honest: every expected output is
exactly what the validators produce (ids already canonical, every field
present), every certification and software id it claims is named in the
text, every qualification's quote is verbatim, every must-have is a verbatim
fragment of the JD, `hard_filter_matches` equals what the hard filter
computes, and the PDFs are not stale.

Conventions the expected outputs follow, so the parser and the eval agree:

- Only credentials the candidate holds count in `certifications`; exam
  progress or "studying for" is not an `other_certifications` entry either.
  It is still recorded, in `qualifications`, with status `part_qualified` or
  `in_progress`.
- A qualification is stored as what it is: `ACA` is `aca_icaew`, never `cpa`.
  "CPA" on its own is the US, Canadian or Australian one according to the
  issuing body or, failing that, where the candidate is.
- Products and credentials outside the taxonomy go to the `other_*` field
  verbatim (`Dext`, `CCH Axcess`, `AuditBoard`); anything the taxonomy knows
  must be the id, never free text.
- A JD's "CPA or CMA" is not a hard filter: the two are different kinds of
  qualification and listing both would demand both, so the either/or stays in
  `must_haves` and `required_certifications` is empty. "CPA or equivalent
  (ACA, ACCA, CA)" is one requirement with `accept_equivalents` true, not
  four. A certification listed under "nice to have" is not required either;
  it goes in `preferred_certifications`, where "CPP or FPC" is both ids
  because a preference never filters.
- `min_years_experience` is the overall minimum a must-have states as a
  number; a figure for part of it ("at least 5 in manufacturing") stays in the
  must-have text, and a JD with no number gets null.
- `years_experience` runs from the first professional role to
  `app.fixtures.AS_OF` (2026-09-30, the day these were written), so a parser
  test passes that date as "today"; overlapping part-time roles do not add.
- `positions` are the jobs in the order the resume lists them, title and
  employer as written. Two titles in one date range ("Staff Accountant, then
  Accountant") are one position with the later title; clients of a
  self-employed or consulting role are not positions; an internship is a
  position but does not count towards `years_experience`. A current role has
  `current: true` and no `end_year`.
- `gaap_exposure` lists the accounting frameworks and standards the resume
  names, as written (`US GAAP`, `ASC 606`, `IFRS 17`). Nothing is inferred
  from the candidate's country or title, and SOX is not an accounting standard.
- `availability` is the nearest bucket to what the resume says;
  `available_from` is set only when a date is written; "not looking" is
  `unavailable`, no statement is `unknown`.
- `languages` is the language the resume is written in plus any it names.
- `must_haves` and `nice_to_haves` are the JD's own words, minus the bullet
  and the trailing full stop.

Every person, employer and contact detail is invented; emails use `example.com`.

## Taxonomy

The hard filters compare a role's `required_certifications` / `required_software`
with a profile's `certifications` / `software` as arrays of ids, which only
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

## Qualifications across jurisdictions

A US job description asks for a "CPA". A UK candidate will never have one:
their equivalent is an ACA, an ACCA or a CA. Comparing ids exactly would
throw every one of them out, so qualifications are handled in two steps.

**Extraction records what is held, never what it is equivalent to.** Each
entry of `CandidateProfile.qualifications` has `name_as_written` ("ACA"),
`canonical` (`aca_icaew`), `issuing_body`, `jurisdiction`, `status`
(`qualified`, `part_qualified` or `in_progress`), `year_obtained` and the
supporting `quote`. The model proposes; the validators in
[`ai/app/schemas.py`](ai/app/schemas.py) and
[`ai/app/qualifications.py`](ai/app/qualifications.py) decide:

- The written name fixes the family. A model that answers `cpa` for an "ACA"
  is overruled.
- Letters that several bodies share resolve to an ambiguous id (`cpa`, `ca`,
  `aca`) and move to a specific one (`cpa_us`, `cpa_canada`, `cpa_australia`,
  `aca_icaew`, `ca_icas`, `ca_ireland`, `ca_anz`, `ca_icai`) only when the
  issuing body named next to it, a stated country, or failing those the
  candidate's time zone settles it. Otherwise the id stays ambiguous and
  `jurisdiction` stays empty.
- "Part-qualified ACCA", "ACCA finalist" and "CPA candidate" are downgraded
  from `qualified` whatever the model said, and only fully held
  qualifications are in `certifications`, the hard-filter column.

**Matching decides equivalence from a hand-maintained table.** Certification
terms in [`infra/taxonomy.json`](infra/taxonomy.json) carry a `group`:

| Group | Members |
| --- | --- |
| `qualified_accountant` (CPA level) | US CPA; Canadian CPA; CPA Australia; ACA (ICAEW); CA (ICAS); ACA / CA (Chartered Accountants Ireland); CA (CA ANZ); CA (ICAI); ACCA |
| `management_accountant` | CIMA, CGMA, CMA (US) |
| `accounting_technician` | AAT |

A required qualification (`RoleRequirements.required_qualifications`) is met
by the same qualification, or, when `accept_equivalents` is true, by any
body-specific member of its group. `accept_equivalents` is false only when
the JD rules equivalents out ("active US CPA licence required"); when the JD
does not say, it defaults to true with `equivalents_stated: false`, which is
the cue for the intake screen to show the employer the default so they can
change it (the role's `requirements` JSON is theirs to `PUT`). A candidate
whose "CPA" could not be placed is not assumed to be an equivalent.

[`ai/app/matching.py`](ai/app/matching.py) applies this and says why in
words, for the match explanation:

```
Holds ACA (ICAEW, UK), equivalent to the US CPA this role asks for
Holds ACA (ICAEW, UK), the same level as a US CPA, but this role requires the US CPA itself
Part-qualified ACCA (UK): not yet qualified, so it does not meet the US CPA requirement
```

The Go side has the same rule as `taxonomy.Acceptable`, for the shortlist
query's `&&`; both run the `acceptable` cases in
[`infra/taxonomy_cases.json`](infra/taxonomy_cases.json).

An equivalent qualification is not equivalent experience. The filter only
says the candidate is a qualified accountant; whether someone who has
reported under FRS 102 all their career can run a US GAAP close is scored
separately by the rerank, from `gaap_exposure` and the resume text (the
rerank prompt says so explicitly).

Two things this does not do:

- **The equivalence table has not been reviewed** against the professional
  bodies' own recognition pages (`qualification_groups.reviewed` is `false`
  in the taxonomy file). It groups by level; it is not a statement about
  mutual recognition agreements or the right to practise. Review it before
  relying on it outside the demo.
- **Nothing is verified against a registry.** No ICAEW member directory or US
  state board lookup is made: the candidates are synthetic, so there is
  nobody to look up. [`ai/app/verification.py`](ai/app/verification.py) is
  the stub interface (`QualificationVerifier`; the only implementation
  answers `unverified`).

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


`EMBEDDING_PROVIDER=local` (the default) uses a deterministic hashed bag of words so
the stack runs with no API keys: texts that share vocabulary sit close together,
which is enough for a payroll JD to rank payroll profiles first, but it knows
nothing about synonyms. Set it to `openai` with `OPENAI_API_KEY` for real
embeddings. `LLM_PROVIDER` selects `anthropic` or `openai` for the chat calls
behind `/parse-resume`, `/parse-jd` and `/rerank`; the matching key must be set,
or those endpoints answer `502 llm provider error: ... no credentials configured`
while everything else keeps working. `LLM_MODEL` overrides the provider's default
chat model (`claude-opus-5-5` for Anthropic).

`LLM_PROVIDER=fake` needs no key and no network. It is the same `Provider`
interface with [`ai/app/fake.py`](ai/app/fake.py) behind it: regexes and
taxonomy lookups that answer all three endpoints with schema-valid output, the
same way every time. It finds what can be found mechanically (the email, the
taxonomy terms a text names, the qualifications and whether the line says
"part-qualified" or "or equivalent", the bullets under a "Requirements" heading; the
rerank levels are the share of the role's certifications, software and
industries a candidate names, and word overlap for experience) and leaves the rest null. Use it for
offline development and for wiring; it is not a parser, and a term that is
also an ordinary word (the "Monday" in a start date) will fool it.

### Response cache

Every LLM completion and every provider embedding is stored on disk under a
SHA-256 of the provider, the model, the prompt (system and user) and the
output schema, or the model, the width and the input text for an embedding
([`ai/app/cache.py`](ai/app/cache.py)). Re-running the seed generation, the
eval or a demo reads those answers back instead of paying for them again, and
gets the same results. Entries live in `ai/.cache` (git-ignored; the
`ai_cache` volume under compose), one JSON file each; `AI_CACHE_DIR` moves it.

| `AI_CACHE` | Behaviour |
| --- | --- |
| `on` (default) | Read a stored answer when there is one, store new ones |
| `off` | Bypass: always call the provider, store nothing |
| `refresh` | Always call the provider and overwrite the stored answer |

`make eval NO_CACHE=1` and `make seed-generate NO_CACHE=1` bypass it for one
run, and `make cache-clear` deletes the local entries. Things to know:

- Provider errors are never stored. A completion that came back but failed
  validation is: the correction retry is a different prompt with its own
  entry, so a replay takes the same path as the original run, and an input
  that failed twice keeps failing until `AI_CACHE=refresh`.
- `/parse-resume` puts today's date (or `as_of`) in the prompt, so the same
  resume is a new entry each day unless the caller pins `as_of`.
- Changing a prompt, a schema (including the taxonomy ids in it) or the model
  changes the key; nothing needs clearing by hand.
- The `local` embedder is already a pure function and is not cached.

## Eval

`make eval` (`python -m app.eval`) runs the parsers and the reranker over the
[parser fixtures](#parser-fixtures) with the configured provider and prints a
score per field: exact match for scalars, F1 for the lists (taxonomy ids,
named standards, and positions compared by title and years), and for
each JD with hard-filter matches the share of the top k ranked resumes that
are among its k matches. `make eval PROVIDER=fake` runs it with no key. The
last line counts the calls that reached the provider, and a second run reports
none:

```
$ make eval PROVIDER=fake | tail -1
provider calls: 22   cache hits: 0
$ make eval PROVIDER=fake | tail -1
provider calls: 0   cache hits: 22
```
