-- The retrieval stage of a matching run, in the same statement as the hard
-- filters (Store.RunHardFilter): of the candidates who passed, the closest to
-- the role by the cosine distance of their embeddings, up to retrieval_limit.
-- retrieved_ids and retrieved_similarities are parallel and in rank order,
-- nearest first. unranked_ids is who passed but could not be compared (by
-- name): their profile or the role has no usable embedding, or the two came
-- from different providers. They are not in retrieved_ids. role_embedded says
-- whether the role had an embedding to compare with; when it did not, everyone
-- who passed is unranked and the run is not ready rather than empty.
--
-- Runs recorded before this migration retrieved nothing: a limit of 0.
ALTER TABLE filter_runs
    ADD COLUMN retrieval_limit        INTEGER            NOT NULL DEFAULT 0,
    ADD COLUMN retrieved_ids          UUID[]             NOT NULL DEFAULT '{}',
    ADD COLUMN retrieved_similarities DOUBLE PRECISION[] NOT NULL DEFAULT '{}',
    ADD COLUMN unranked_ids           UUID[]             NOT NULL DEFAULT '{}',
    ADD COLUMN role_embedded          BOOLEAN            NOT NULL DEFAULT false,
    ADD CONSTRAINT filter_runs_retrieval_check
        CHECK (retrieval_limit >= 0
               AND cardinality(retrieved_ids) = cardinality(retrieved_similarities)
               AND cardinality(retrieved_ids) <= retrieval_limit
               AND cardinality(retrieved_ids) + cardinality(unranked_ids) <= after_timezone_overlap);
