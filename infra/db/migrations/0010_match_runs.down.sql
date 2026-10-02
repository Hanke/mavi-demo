ALTER TABLE filter_runs DROP COLUMN IF EXISTS matched_at;

UPDATE matches SET status = 'proposed' WHERE status = 'pending_review';
ALTER TABLE matches DROP CONSTRAINT matches_status_check;
ALTER TABLE matches ADD CONSTRAINT matches_status_check
    CHECK (status IN ('proposed', 'approved', 'rejected', 'swapped'));
