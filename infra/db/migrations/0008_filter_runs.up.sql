-- The hard-filter stage of a matching run, in SQL: the pool of active
-- candidates is narrowed on the role's must-haves before anything is embedded
-- or reranked (Store.RunHardFilter), and each run is recorded here so the UI
-- or the logs can show how the pool narrowed.

-- overlap_minutes is the time-zone overlap of that stage: how many minutes of
-- a role's working day on on_day (09:00 to 17:00 in role_tz) fall inside a
-- candidate's working hours (work_start to work_end in cand_tz; an end at or
-- before the start runs past midnight). It is availability.OverlapMinutes in
-- SQL, the same definition: for each minute of the role's day, what does the
-- candidate's clock read. That is right on the days a zone changes its
-- clocks, when a local time happens twice or not at all.
--
-- NULL when Postgres cannot read either zone: the caller treats that as not
-- met, never as an overlap of zero that a role asking for none would pass.
-- It reads more than IANA names, though (abbreviations, POSIX strings such as
-- 'XYZ5', names in the wrong case), so the filter only calls this for zones
-- listed in pg_timezone_names by that exact name; looking each one up here
-- would scan that view on every call.
CREATE FUNCTION overlap_minutes(role_tz TEXT, cand_tz TEXT, work_start TIME, work_end TIME, on_day DATE)
    RETURNS INT
    LANGUAGE plpgsql STABLE AS $$
BEGIN
    RETURN (
        SELECT count(*) FILTER (WHERE CASE
                   WHEN work_end > work_start THEN c.clock >= work_start AND c.clock < work_end
                   ELSE c.clock >= work_start OR c.clock < work_end
               END)
        FROM generate_series(
                 (on_day + TIME '09:00') AT TIME ZONE role_tz,
                 (on_day + TIME '17:00') AT TIME ZONE role_tz - INTERVAL '1 minute',
                 INTERVAL '1 minute') AS m (t)
        CROSS JOIN LATERAL (SELECT (m.t AT TIME ZONE cand_tz)::time AS clock) c
    );
EXCEPTION
    WHEN invalid_parameter_value THEN -- time zone not recognized
        RETURN NULL;
END;
$$;

-- One row per run of the hard filters for a role. The after_* columns are
-- the funnel: how many candidates were still in after each filter, applied in
-- this order, so each is at most the one before it and the last is the number
-- that passed. candidate_ids is who passed, by name; they are the only
-- candidates a later stage of the run may look at. It is a record of the run,
-- not a reference: a candidate deleted since stays listed.
CREATE TABLE filter_runs (
    id                     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id                UUID        NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    overlap_on             DATE        NOT NULL,   -- the day the time-zone overlap was worked out for
    pool                   INTEGER     NOT NULL,   -- active candidates
    after_profile          INTEGER     NOT NULL,   -- ... with a parsed profile
    after_certifications   INTEGER     NOT NULL,   -- ... holding every required qualification, or an accepted equivalent
    after_software         INTEGER     NOT NULL,   -- ... listing all the required software
    after_experience       INTEGER     NOT NULL,   -- ... with the minimum years
    after_availability     INTEGER     NOT NULL,   -- ... who answered, can start in time and offer the hours
    after_timezone_overlap INTEGER     NOT NULL,   -- ... whose working hours cover enough of the role's day
    candidate_ids          UUID[]      NOT NULL DEFAULT '{}',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (pool >= after_profile AND after_profile >= after_certifications
           AND after_certifications >= after_software AND after_software >= after_experience
           AND after_experience >= after_availability AND after_availability >= after_timezone_overlap
           AND after_timezone_overlap = cardinality(candidate_ids))
);

CREATE INDEX filter_runs_role_idx ON filter_runs (role_id, created_at DESC);
