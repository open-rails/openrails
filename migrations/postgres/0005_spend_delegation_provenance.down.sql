-- Remove the or#911 provenance reference. Callers go back to keeping their own
-- grant→document mapping.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.invoker_spend_limits
    DROP COLUMN provenance;
