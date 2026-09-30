"""Synthetic seed data for the demo: ~200 finance / accounting candidates and a handful of job descriptions.

Three stages, each a subcommand of `python -m app.seedgen`:

* `plan`     - a deterministic distribution (seeded RNG, no network): who the
               candidates are - role family, years, certifications, software,
               GAAP exposure, industries, availability, timezone, language.
               The plan is what makes hard filters and ranking visibly change
               results, so it is code, not left to the model.
* `generate` - asks Claude to write each candidate's resume text, headline and
               skills for the plan's spec, checks that every certification and
               software product in the spec is actually mentioned, and writes
               infra/db/seed/data/candidates.json. Needs ANTHROPIC_API_KEY.
* `render`   - turns the committed JSON (candidates, plus the hand-written
               roles.json) into the SQL files `api seed` loads. No key, no
               network, byte-for-byte reproducible; tests/test_seed_data.py
               fails when the SQL is stale.

Only the output is committed, so `make seed` never calls a provider.
"""
