-- Availability and time-zone overlap are hard filters that cannot be read
-- from a resume, so the candidate supplies them. They live in a table of
-- their own rather than on candidate_profiles: a profile is replaced whenever
-- a resume is parsed, and what the candidate said must survive that. The
-- availability / available_from / timezone columns on candidate_profiles stay
-- as what the resume suggests, and no filter reads them.
--
-- A row holds all four answers or does not exist, so "has the candidate
-- supplied them" is one join. A candidate with no row is excluded from
-- matching.
CREATE TABLE candidate_availability (
    candidate_id   UUID        PRIMARY KEY REFERENCES candidates (id) ON DELETE CASCADE,
    timezone       TEXT        NOT NULL,                -- IANA name, e.g. America/Chicago
    work_start     TIME        NOT NULL,                -- working hours, local to `timezone`
    work_end       TIME        NOT NULL,                -- at or before work_start: runs past midnight
    hours_per_week SMALLINT    NOT NULL CHECK (hours_per_week BETWEEN 1 AND 80),
    available_from DATE        NOT NULL,                -- earliest start date
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (work_start <> work_end)
);

CREATE TRIGGER candidate_availability_set_updated_at
    BEFORE UPDATE ON candidate_availability
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The role side. The role's working day is 09:00 to 17:00 in roles.timezone;
-- min_overlap_hours is how much of it the candidate's working hours must
-- cover. hours_per_week is what the role needs, compared with what the
-- candidate offers. NULL means the role does not ask.
ALTER TABLE roles ADD COLUMN min_overlap_hours SMALLINT
    CHECK (min_overlap_hours IS NULL OR min_overlap_hours BETWEEN 1 AND 8);
ALTER TABLE roles ADD COLUMN hours_per_week SMALLINT
    CHECK (hours_per_week IS NULL OR hours_per_week BETWEEN 1 AND 80);
ALTER TABLE roles ADD CONSTRAINT roles_overlap_needs_timezone
    CHECK (min_overlap_hours IS NULL OR timezone IS NOT NULL);
