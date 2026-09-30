-- jobs: Postgres-backed background queue used by the Go API's worker.
--
-- Dequeue pattern (see the job queue ticket):
--   UPDATE jobs SET status = 'running', locked_at = now(), locked_by = $1,
--                   attempts = attempts + 1
--   WHERE id = (SELECT id FROM jobs
--               WHERE status = 'queued' AND run_at <= now()
--               ORDER BY priority DESC, run_at, id
--               FOR UPDATE SKIP LOCKED LIMIT 1)
--   RETURNING *;
-- A failed attempt with attempts < max_attempts goes back to 'queued' with a
-- future run_at; otherwise it lands in 'failed'.
CREATE TABLE jobs (
    id           BIGINT      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    kind         TEXT        NOT NULL,                        -- e.g. extract_profile, embed_role, score_role
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT        NOT NULL DEFAULT 'queued'
                             CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    priority     SMALLINT    NOT NULL DEFAULT 0,              -- higher runs first
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),          -- not before; used for delays and retry backoff
    attempts     INTEGER     NOT NULL DEFAULT 0,
    max_attempts INTEGER     NOT NULL DEFAULT 3 CHECK (max_attempts >= 1),
    last_error   TEXT,
    locked_at    TIMESTAMPTZ,
    locked_by    TEXT,                                        -- worker id; lets a sweeper reclaim stale 'running' rows
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);

-- Exactly the dequeue ordering, restricted to rows a worker can pick up.
CREATE INDEX jobs_dequeue_idx ON jobs (priority DESC, run_at, id) WHERE status = 'queued';
-- Sweeper: find 'running' jobs whose worker went away.
CREATE INDEX jobs_running_idx ON jobs (locked_at) WHERE status = 'running';
CREATE INDEX jobs_kind_status_idx ON jobs (kind, status);

CREATE TRIGGER jobs_set_updated_at
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
