-- Queue the embedding jobs for everything the seed left without a vector.
-- The worker inside the API container runs them through the AI service's
-- /embed, so `make seed` ends with embedded profiles and roles and needs no
-- provider key when EMBEDDING_PROVIDER=local. Runs last (files load in name
-- order). The partial unique index on queued (kind, payload) makes a re-run
-- a no-op while the first batch is still waiting, and the `embedding IS NULL`
-- guard makes it a no-op once they have run.
INSERT INTO jobs (kind, payload)
SELECT 'embed_profile', jsonb_build_object('candidate_id', candidate_id::text)
FROM candidate_profiles
WHERE embedding IS NULL
ON CONFLICT (kind, payload) WHERE status = 'queued' DO NOTHING;

INSERT INTO jobs (kind, payload)
SELECT 'embed_role', jsonb_build_object('role_id', id::text)
FROM roles
WHERE embedding IS NULL AND btrim(description) <> ''
ON CONFLICT (kind, payload) WHERE status = 'queued' DO NOTHING;
