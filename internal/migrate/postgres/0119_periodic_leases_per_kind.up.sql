-- Repair: none-needed Leases are rebuilt by the next claim of each kind.
-- Hosts sharing a database declare the same merchants, so a periodic pass has
-- one lease per kind.
DELETE FROM billing.periodic_leases;
ALTER TABLE billing.periodic_leases DROP CONSTRAINT periodic_leases_pkey;
ALTER TABLE billing.periodic_leases DROP COLUMN merchant_set;
ALTER TABLE billing.periodic_leases ADD CONSTRAINT periodic_leases_pkey PRIMARY KEY (kind);
COMMENT ON TABLE billing.periodic_leases IS 'Global by design: which fleet (River schema) queues a periodic kind''s pass for the database; one row per kind, taken over once expired.';
