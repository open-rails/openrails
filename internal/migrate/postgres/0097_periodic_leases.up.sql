-- Repair: none-needed A new table; no existing row changes.
-- Hosts embedding one merchant on one database each run OpenRails' periodic
-- jobs. A fleet claims a kind's lease for most of its period before queueing
-- the pass; another fleet finding it held skips that pass.
CREATE TABLE billing.periodic_leases (
    kind text NOT NULL,
    holder text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    claimed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT periodic_leases_pkey PRIMARY KEY (kind),
    CONSTRAINT periodic_leases_kind_check CHECK (kind <> ''),
    CONSTRAINT periodic_leases_holder_check CHECK (holder <> '')
);
COMMENT ON TABLE billing.periodic_leases IS 'Global by design: which fleet (River schema) queues a periodic kind''s pass for the database; one row per kind, taken over once expired.';
COMMENT ON COLUMN billing.periodic_leases.kind IS 'The periodic job''s id: its job kind, or the name its schedule gave.';
COMMENT ON COLUMN billing.periodic_leases.holder IS 'The holding fleet''s River schema; a fleet renews its own lease.';
COMMENT ON COLUMN billing.periodic_leases.expires_at IS 'When another fleet may take the kind over: a fraction of the period after the claim.';
