INSERT INTO documents (title, content) VALUES
    ('Welcome', 'Mavi is a demo stack: React web app, Go API, Python AI service, and Postgres with pgvector.'),
    ('Health checks', 'Every service exposes a health endpoint. The API health check also reports whether it can reach the database and the AI service.'),
    ('Embeddings', 'Documents are embedded by the AI service and stored in a pgvector column for similarity search.')
ON CONFLICT DO NOTHING;
