-- The last stages of a matching run (the match_role job): the shortlist of a
-- filter run is reranked and written to matches, and the best of it goes to
-- ops for review.

-- pending_review is the review queue: the top of a run's ranking, waiting for
-- an ops decision. The rest of what a run scored stays 'proposed', in reserve.
-- Neither is visible to an employer: that is released_at, which a run never
-- sets.
ALTER TABLE matches DROP CONSTRAINT matches_status_check;
ALTER TABLE matches ADD CONSTRAINT matches_status_check
    CHECK (status IN ('proposed', 'pending_review', 'approved', 'rejected', 'swapped'));

-- When the run's matches were written (Store.ReplaceRunMatches); NULL for a
-- run of the filters alone, or one whose rerank did not finish. A run older
-- than the latest one written does not overwrite it.
ALTER TABLE filter_runs ADD COLUMN matched_at TIMESTAMPTZ;
