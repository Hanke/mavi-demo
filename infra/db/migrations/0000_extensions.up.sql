-- Extensions the rest of the chain depends on. Idempotent when the compose
-- init script already created them; required on any database it did not
-- (plain createdb, CI, the throwaway DB the Go tests create).
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
