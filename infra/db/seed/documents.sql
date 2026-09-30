-- documents has no natural key, so guard on title instead of ON CONFLICT.
INSERT INTO documents (title, content)
SELECT v.title, v.content
FROM (VALUES
    ('Welcome', 'Mavi is a demo stack: React web app, Go API, Python AI service, and Postgres with pgvector.'),
    ('Health checks', 'Every service exposes a health endpoint. The API health check also reports whether it can reach the database and the AI service.'),
    ('Embeddings', 'Documents are embedded by the AI service and stored in a pgvector column for similarity search.')
) AS v(title, content)
WHERE NOT EXISTS (SELECT 1 FROM documents d WHERE d.title = v.title);
