-- Core matching schema: candidates, their AI-extracted profiles, roles, the
-- matches between them, and the ops review audit trail.
--
-- Conventions
--   * Owned by the Go API. The Python service never touches these tables.
--   * Fields the hard filters need (certifications, software, availability,
--     timezone) are real columns, not keys inside the JSONB blobs, so they can
--     be indexed and filtered in SQL. The JSONB columns keep the full
--     structured extraction for the LLM-facing paths.
--   * Array columns hold values normalised by the API (trimmed, lower-cased)
--     so `@>` containment checks are exact.
--   * Enumerations are TEXT + CHECK rather than ENUM types so later migrations
--     can add values without a type rewrite.

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- ---------------------------------------------------------------------------
-- candidates: the person as ingested. Raw resume text lives here; everything
-- derived from it lives in candidate_profiles.
-- ---------------------------------------------------------------------------
CREATE TABLE candidates (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    full_name     TEXT        NOT NULL,
    email         TEXT,
    phone         TEXT,
    location      TEXT,                          -- free text, e.g. "Austin, TX"
    resume_text   TEXT        NOT NULL DEFAULT '', -- raw input to profile extraction
    source        TEXT,                          -- e.g. upload, referral, linkedin
    status        TEXT        NOT NULL DEFAULT 'active'
                              CHECK (status IN ('active', 'archived')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX candidates_email_key ON candidates (lower(email)) WHERE email IS NOT NULL;
CREATE INDEX candidates_status_idx ON candidates (status);

CREATE TRIGGER candidates_set_updated_at
    BEFORE UPDATE ON candidates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- candidate_profiles: one structured profile per candidate (1:1). `profile`
-- is the full extraction; the hard-filter fields are promoted to columns.
-- ---------------------------------------------------------------------------
CREATE TABLE candidate_profiles (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    candidate_id     UUID        NOT NULL UNIQUE REFERENCES candidates (id) ON DELETE CASCADE,
    profile          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    headline         TEXT,
    years_experience SMALLINT    CHECK (years_experience IS NULL OR years_experience >= 0),

    -- Hard-filter columns ---------------------------------------------------
    certifications   TEXT[]      NOT NULL DEFAULT '{}',   -- e.g. {pmp, cpa}
    software         TEXT[]      NOT NULL DEFAULT '{}',   -- e.g. {salesforce, netsuite}
    availability     TEXT        NOT NULL DEFAULT 'unknown'
                                 CHECK (availability IN ('immediate', 'two_weeks', 'one_month', 'unavailable', 'unknown')),
    available_from   DATE,                                -- precise start when known
    timezone         TEXT,                                -- IANA name, e.g. America/Chicago

    -- Embedding --------------------------------------------------------------
    -- 1536 matches text-embedding-3-small; the local stub pads to the same width.
    embedding        vector(1536),
    embedding_model  TEXT,                                -- model that produced `embedding`
    embedded_at      TIMESTAMPTZ,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX candidate_profiles_certifications_idx ON candidate_profiles USING gin (certifications);
CREATE INDEX candidate_profiles_software_idx       ON candidate_profiles USING gin (software);
CREATE INDEX candidate_profiles_availability_idx   ON candidate_profiles (availability, available_from);
CREATE INDEX candidate_profiles_timezone_idx       ON candidate_profiles (timezone);
CREATE INDEX candidate_profiles_embedding_idx      ON candidate_profiles USING hnsw (embedding vector_cosine_ops);

CREATE TRIGGER candidate_profiles_set_updated_at
    BEFORE UPDATE ON candidate_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- roles: raw job description plus the structured requirements parsed from it.
-- must_haves / nice_to_haves keep the full parsed lists; the hard-filter
-- requirements are promoted to columns so a match query can compare them to
-- candidate_profiles directly (e.g. p.certifications @> r.required_certifications).
-- ---------------------------------------------------------------------------
CREATE TABLE roles (
    id                      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    title                   TEXT        NOT NULL,
    company                 TEXT,
    description             TEXT        NOT NULL DEFAULT '',   -- raw JD
    requirements            JSONB       NOT NULL DEFAULT '{}'::jsonb, -- full structured parse
    must_haves              JSONB       NOT NULL DEFAULT '[]'::jsonb,
    nice_to_haves           JSONB       NOT NULL DEFAULT '[]'::jsonb,

    -- Hard-filter requirements ------------------------------------------------
    required_certifications TEXT[]      NOT NULL DEFAULT '{}',
    required_software       TEXT[]      NOT NULL DEFAULT '{}',
    timezone                TEXT,                                -- IANA name the role operates in
    starts_on               DATE,                                -- candidate must be available by this date

    status                  TEXT        NOT NULL DEFAULT 'open'
                                        CHECK (status IN ('open', 'filled', 'closed')),

    embedding               vector(1536),
    embedding_model         TEXT,
    embedded_at             TIMESTAMPTZ,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (jsonb_typeof(must_haves) = 'array'),
    CHECK (jsonb_typeof(nice_to_haves) = 'array')
);

CREATE INDEX roles_status_idx    ON roles (status);
CREATE INDEX roles_embedding_idx ON roles USING hnsw (embedding vector_cosine_ops);

CREATE TRIGGER roles_set_updated_at
    BEFORE UPDATE ON roles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- matches: one row per (role, candidate) pair the matcher has scored.
-- ---------------------------------------------------------------------------
CREATE TABLE matches (
    id           UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id      UUID             NOT NULL REFERENCES roles (id)      ON DELETE CASCADE,
    candidate_id UUID             NOT NULL REFERENCES candidates (id) ON DELETE CASCADE,
    score        DOUBLE PRECISION NOT NULL CHECK (score >= 0 AND score <= 1),
    explanation  TEXT             NOT NULL DEFAULT '',            -- LLM-written rationale
    breakdown    JSONB            NOT NULL DEFAULT '{}'::jsonb,   -- component scores, filter results
    status       TEXT             NOT NULL DEFAULT 'proposed'
                                  CHECK (status IN ('proposed', 'approved', 'rejected', 'swapped')),
    created_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),

    UNIQUE (role_id, candidate_id)
);

-- Shortlist query: best matches for a role, optionally by status.
CREATE INDEX matches_role_status_score_idx ON matches (role_id, status, score DESC);
CREATE INDEX matches_candidate_idx         ON matches (candidate_id);

CREATE TRIGGER matches_set_updated_at
    BEFORE UPDATE ON matches
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- review_events: append-only audit trail of ops decisions on matches.
-- Deliberately no ON DELETE CASCADE: a match with review history cannot be
-- hard-deleted; close the role instead.
-- ---------------------------------------------------------------------------
CREATE TABLE review_events (
    id                       BIGINT      PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    match_id                 UUID        NOT NULL REFERENCES matches (id) ON DELETE RESTRICT,
    action                   TEXT        NOT NULL CHECK (action IN ('approve', 'reject', 'swap')),
    actor                    TEXT        NOT NULL,                   -- ops user identifier (email)
    reason                   TEXT,
    replacement_candidate_id UUID        REFERENCES candidates (id) ON DELETE RESTRICT,
    metadata                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (action <> 'swap' OR replacement_candidate_id IS NOT NULL)
);

CREATE INDEX review_events_match_idx ON review_events (match_id, created_at);
CREATE INDEX review_events_actor_idx ON review_events (actor, created_at);
