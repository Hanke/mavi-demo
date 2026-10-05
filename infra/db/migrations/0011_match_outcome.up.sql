-- How a matching run ended (the match_role job), for the cases where the
-- pipeline cannot deliver the two profiles it promises. A run that could is
-- 'matched'. One that could not is 'needs_attention', with why:
--
--   too_few_passed     zero or one candidate passed the hard filters
--   too_few_qualified  fewer than two of the reranked scored at or above min_score
--   ai_failed          the AI service failed or timed out during the rerank;
--                      attention_detail is its error, and no matches were written
--
-- match_status is NULL for a run of the filters alone (POST
-- /roles/{id}/filter-runs) and for one that never got as far as an outcome.
-- min_score is the threshold the run applied and qualified how many of its
-- ranking met it; both are NULL when nothing was scored.
ALTER TABLE filter_runs
    ADD COLUMN match_status     TEXT,
    ADD COLUMN attention_reason TEXT,
    ADD COLUMN attention_detail TEXT,
    ADD COLUMN min_score        DOUBLE PRECISION,
    ADD COLUMN qualified        INTEGER,
    ADD CONSTRAINT filter_runs_match_status_check
        CHECK (match_status IN ('matched', 'needs_attention')),
    ADD CONSTRAINT filter_runs_attention_check
        CHECK (attention_reason IN ('too_few_passed', 'too_few_qualified', 'ai_failed')
               AND (attention_reason IS NOT NULL) = (match_status IS NOT DISTINCT FROM 'needs_attention')
               AND (attention_detail IS NULL OR attention_reason IS NOT NULL));

-- The latest run with an outcome is the role's matching status.
CREATE INDEX filter_runs_outcome_idx ON filter_runs (role_id, created_at DESC) WHERE match_status IS NOT NULL;
