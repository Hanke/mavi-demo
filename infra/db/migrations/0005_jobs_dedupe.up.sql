-- One queued job per (kind, payload). Saving a role five times while its
-- embedding job is still waiting must not queue five embeddings; the Go
-- queue's Enqueue inserts with ON CONFLICT ... DO NOTHING against this index
-- and returns the job already waiting. Only 'queued' rows take part, so a
-- job can be enqueued again once the earlier one is running or finished.
-- Payloads are small id objects; a btree on jsonb is fine at that size.
CREATE UNIQUE INDEX jobs_queued_dedupe_idx ON jobs (kind, payload) WHERE status = 'queued';
