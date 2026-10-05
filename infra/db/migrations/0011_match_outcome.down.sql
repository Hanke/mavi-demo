DROP INDEX IF EXISTS filter_runs_outcome_idx;
ALTER TABLE filter_runs
    DROP CONSTRAINT IF EXISTS filter_runs_attention_check,
    DROP CONSTRAINT IF EXISTS filter_runs_match_status_check,
    DROP COLUMN IF EXISTS qualified,
    DROP COLUMN IF EXISTS min_score,
    DROP COLUMN IF EXISTS attention_detail,
    DROP COLUMN IF EXISTS attention_reason,
    DROP COLUMN IF EXISTS match_status;
