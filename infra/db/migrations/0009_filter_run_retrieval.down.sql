ALTER TABLE filter_runs
    DROP CONSTRAINT IF EXISTS filter_runs_retrieval_check,
    DROP COLUMN IF EXISTS retrieval_limit,
    DROP COLUMN IF EXISTS retrieved_ids,
    DROP COLUMN IF EXISTS retrieved_similarities,
    DROP COLUMN IF EXISTS unranked_ids,
    DROP COLUMN IF EXISTS role_embedded;
