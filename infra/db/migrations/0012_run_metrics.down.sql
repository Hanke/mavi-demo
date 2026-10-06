ALTER TABLE filter_runs
    DROP COLUMN IF EXISTS metrics,
    DROP COLUMN IF EXISTS request_id;
