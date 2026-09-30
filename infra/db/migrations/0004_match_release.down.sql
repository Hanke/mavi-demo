DELETE FROM review_events WHERE action IN ('release', 'unrelease');
ALTER TABLE review_events DROP CONSTRAINT review_events_action_check;
ALTER TABLE review_events ADD CONSTRAINT review_events_action_check
    CHECK (action IN ('approve', 'reject', 'swap'));
DROP INDEX IF EXISTS matches_released_role_idx;
ALTER TABLE matches DROP COLUMN IF EXISTS released_at;
