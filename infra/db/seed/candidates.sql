-- Demo candidates with hard-filter fields populated. Embeddings are left NULL;
-- the embed job fills them in. Fixed UUIDs plus a targetless ON CONFLICT keep
-- this idempotent even if a demo user has reused one of the seed emails.
INSERT INTO candidates (id, full_name, email, location, source, resume_text) VALUES
    ('11111111-0000-0000-0000-000000000001', 'Ada Okafor',    'ada@example.com',    'Chicago, IL',   'seed',
     'Senior accountant, CPA, 9 years closing books in NetSuite and QuickBooks. Available immediately.'),
    ('11111111-0000-0000-0000-000000000002', 'Ben Larsen',    'ben@example.com',    'Denver, CO',    'seed',
     'Project manager, PMP certified, runs Salesforce and Jira rollouts for mid-market clients. Two weeks notice.'),
    ('11111111-0000-0000-0000-000000000003', 'Chloe Martin',  'chloe@example.com',  'Lyon, France',  'seed',
     'Financial analyst with FP&A background in Excel and Anaplan. Open to remote, available in a month.')
ON CONFLICT DO NOTHING;

INSERT INTO candidate_profiles
    (candidate_id, headline, years_experience, certifications, software, availability, available_from, timezone, profile)
VALUES
    ('11111111-0000-0000-0000-000000000001', 'Senior Accountant', 9,
     '{cpa}', '{netsuite,quickbooks}', 'immediate', CURRENT_DATE, 'America/Chicago',
     '{"skills": ["month-end close", "gaap", "reconciliations"], "languages": ["en"]}'),
    ('11111111-0000-0000-0000-000000000002', 'Project Manager', 7,
     '{pmp}', '{salesforce,jira}', 'two_weeks', CURRENT_DATE + 14, 'America/Denver',
     '{"skills": ["crm rollout", "stakeholder management"], "languages": ["en"]}'),
    ('11111111-0000-0000-0000-000000000003', 'Financial Analyst', 4,
     '{}', '{excel,anaplan}', 'one_month', CURRENT_DATE + 30, 'Europe/Paris',
     '{"skills": ["fp&a", "forecasting"], "languages": ["fr", "en"]}')
ON CONFLICT DO NOTHING;
