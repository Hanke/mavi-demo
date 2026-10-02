ALTER TABLE roles DROP CONSTRAINT IF EXISTS roles_overlap_needs_timezone;
ALTER TABLE roles DROP COLUMN IF EXISTS hours_per_week;
ALTER TABLE roles DROP COLUMN IF EXISTS min_overlap_hours;
DROP TABLE IF EXISTS candidate_availability;
