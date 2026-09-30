-- Reverse dependency order. Extensions belong to 0000_extensions.
DROP TABLE IF EXISTS review_events;
DROP TABLE IF EXISTS matches;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS candidate_profiles;
DROP TABLE IF EXISTS candidates;
DROP FUNCTION IF EXISTS set_updated_at();
