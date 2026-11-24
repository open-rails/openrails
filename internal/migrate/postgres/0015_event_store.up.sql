-- The event store attached to this database (Deps.ClickHouse): its identity,
-- recorded when one first attaches, so New refuses an empty or different
-- ClickHouse under a database that has used one.
CREATE TABLE billing.event_store (
    singleton boolean NOT NULL CHECK (singleton),
    store_id uuid NOT NULL,
    attached_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (singleton)
);
COMMENT ON TABLE billing.event_store IS 'Global by design, one row: the identity of the ClickHouse event store this database''s usage lives in, recorded when one first attaches. New refuses a store with another identity: an empty or different ClickHouse.';
