-- Dev seed data. Applied after schema.sql by docker-compose initdb.
-- Populates mce_reach so EligibleMCEs works in the local test environment.

INSERT INTO mce_reach (mce, site, segment) VALUES
    ('dev', 'dc1', 'vlan-100'),
    ('dev', 'dc1', 'vlan-200')
ON CONFLICT DO NOTHING;
