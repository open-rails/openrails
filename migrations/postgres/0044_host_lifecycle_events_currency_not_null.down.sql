SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.host_lifecycle_events
    ALTER COLUMN currency DROP NOT NULL;

COMMENT ON COLUMN openrails.host_lifecycle_events.currency IS NULL;
