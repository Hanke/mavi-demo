-- Queue the embedding jobs for everything the seed left without a vector.
-- The worker inside the API container runs them through the AI service's
-- /embed, so `make seed` ends with embedded profiles and roles and needs no
-- provider key when EMBEDDING_PROVIDER=local. Runs last (files load in name
-- order). The NOT EXISTS guard makes a re-run a no-op while the first batch
-- is still waiting (the same rule as the queue's jobs_queued_dedupe_idx), and
-- the `embedding IS NULL` guard makes it a no-op once they have run.
INSERT INTO jobs (kind, payload)
SELECT 'embed_profile', jsonb_build_object('candidate_id', p.candidate_id::text)
FROM candidate_profiles p
WHERE p.embedding IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM jobs j
    WHERE j.kind = 'embed_profile'
      AND j.status = 'queued'
      AND j.payload = jsonb_build_object('candidate_id', p.candidate_id::text));

INSERT INTO jobs (kind, payload)
SELECT 'embed_role', jsonb_build_object('role_id', r.id::text)
FROM roles r
WHERE r.embedding IS NULL AND btrim(r.description) <> ''
  AND NOT EXISTS (
    SELECT 1 FROM jobs j
    WHERE j.kind = 'embed_role'
      AND j.status = 'queued'
      AND j.payload = jsonb_build_object('role_id', r.id::text));
