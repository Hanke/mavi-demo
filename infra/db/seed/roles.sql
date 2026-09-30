-- Demo roles whose must-haves line up with the seeded candidates. Fixed UUIDs
-- keep this idempotent.
INSERT INTO roles
    (id, title, company, description, must_haves, nice_to_haves,
     required_certifications, required_software, timezone, starts_on)
VALUES
    ('22222222-0000-0000-0000-000000000001', 'Senior Accountant', 'Northwind Foods',
     'Own month-end close for a $40M CPG business. CPA required, NetSuite required, Central time hours.',
     '["cpa", "netsuite", "5+ years close experience"]', '["quickbooks", "cpg experience"]',
     '{cpa}', '{netsuite}', 'America/Chicago', CURRENT_DATE + 7),
    ('22222222-0000-0000-0000-000000000002', 'Salesforce Implementation PM', 'Contoso Health',
     'Lead a Salesforce Service Cloud rollout across three clinics. PMP required, must start within 30 days.',
     '["pmp", "salesforce", "healthcare rollout"]', '["jira", "hipaa familiarity"]',
     '{pmp}', '{salesforce}', 'America/Denver', CURRENT_DATE + 30)
ON CONFLICT DO NOTHING;
