-- What the matching runs took and cost (`make run-metrics`), from the metrics
-- every match_role job leaves on its filter_runs row: the latest runs one by
-- one, then the figures to quote, over the runs that reached the model.
\echo 'Latest matching runs'
SELECT to_char(f.created_at, 'MM-DD HH24:MI:SS')        AS run_at,
       left(r.title, 28)                                AS role,
       coalesce(f.attention_reason, f.match_status, '-') AS outcome,
       f.request_id,
       (f.metrics->>'total_ms')::int                    AS total_ms,
       (f.metrics->'stage_ms'->>'filter')::int          AS filter_ms,
       (f.metrics->'stage_ms'->>'texts')::int           AS texts_ms,
       (f.metrics->'stage_ms'->>'rerank')::int          AS rerank_ms,
       (f.metrics->'stage_ms'->>'persist')::int         AS persist_ms,
       (f.metrics->>'candidates')::int                  AS reranked,
       (f.metrics->>'llm_calls')::int                   AS llm_calls,
       (f.metrics->>'cache_hits')::int                  AS cache_hits,
       (f.metrics->>'input_tokens')::int                AS tokens_in,
       (f.metrics->>'output_tokens')::int               AS tokens_out,
       (f.metrics->>'estimated_cost_usd')::numeric      AS cost_usd,
       f.metrics->>'model'                              AS model
FROM filter_runs f
JOIN roles r ON r.id = f.role_id
WHERE f.metrics IS NOT NULL
ORDER BY f.created_at DESC
LIMIT 20;

-- A run answered from the response cache made no call and cost nothing, so it
-- says nothing about what a match costs: only runs with a call are counted.
\echo 'Per matching run, by model (runs that called the model; cached replays left out)'
SELECT f.metrics->>'model'                                                              AS model,
       count(*)                                                                         AS runs,
       round(avg((f.metrics->>'candidates')::int), 1)                                   AS avg_reranked,
       round(avg((f.metrics->>'llm_calls')::int), 1)                                    AS avg_llm_calls,
       round(avg((f.metrics->>'input_tokens')::int))                                    AS avg_tokens_in,
       round(avg((f.metrics->>'output_tokens')::int))                                   AS avg_tokens_out,
       round(avg((f.metrics->>'estimated_cost_usd')::numeric), 4)                       AS avg_cost_usd,
       round(sum((f.metrics->>'estimated_cost_usd')::numeric)
             / nullif(sum((f.metrics->>'candidates')::int), 0), 4)                      AS cost_per_candidate_usd,
       round(avg((f.metrics->>'total_ms')::int))                                        AS avg_total_ms,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY (f.metrics->>'total_ms')::int)       AS p50_total_ms,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY (f.metrics->>'total_ms')::int)      AS p95_total_ms,
       round(avg((f.metrics->'stage_ms'->>'rerank')::int))                              AS avg_rerank_ms
FROM filter_runs f
WHERE (f.metrics->>'llm_calls')::int > 0
GROUP BY 1
ORDER BY runs DESC;
