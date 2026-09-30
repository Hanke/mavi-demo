-- Employers only ever see a match after ops has released it. `released_at`
-- is independent of `status` so ops can approve a match internally and
-- release it later, and un-release it without losing the review history.
ALTER TABLE matches ADD COLUMN released_at TIMESTAMPTZ;

-- Employer shortlist: released matches for a role, best first.
CREATE INDEX matches_released_role_idx ON matches (role_id, score DESC) WHERE released_at IS NOT NULL;

-- Releasing and un-releasing are ops decisions, so they go in the audit trail.
ALTER TABLE review_events DROP CONSTRAINT review_events_action_check;
ALTER TABLE review_events ADD CONSTRAINT review_events_action_check
    CHECK (action IN ('approve', 'reject', 'swap', 'release', 'unrelease'));
