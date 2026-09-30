-- The JD parser extracts the minimum years of experience a role asks for
-- (RoleRequirements.min_years_experience). Like the other must-haves it is
-- promoted to a column, so the shortlist query can compare it with
-- candidate_profiles.years_experience directly. NULL means the JD gave no number.
ALTER TABLE roles ADD COLUMN min_years_experience SMALLINT
    CHECK (min_years_experience IS NULL OR min_years_experience >= 0);
