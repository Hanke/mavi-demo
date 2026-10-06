-- What a matching run cost, and the id it is logged under.
--
-- request_id is the id of the request that made the run: the match_role job
-- attempt or, for a run of the filters alone (POST /roles/{id}/filter-runs),
-- the HTTP request. Both services write it on every log line of that request
-- (api/internal/reqlog, ai/app/logs.py), so it is how a run is found in their
-- logs. NULL on a run recorded before this column existed.
--
-- metrics is written by the match_role job as it ends (Store.RecordRunMetrics;
-- the shape is the API contract's RunMetrics): how long each stage took, and
-- what the rerank spent on the chat model as the AI service reported it: its
-- calls, tokens and estimated cost. NULL for a run of the filters alone, and
-- for a run whose job never got to its end (the worker stopped).
ALTER TABLE filter_runs
    ADD COLUMN request_id TEXT,
    ADD COLUMN metrics    JSONB;
