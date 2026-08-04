-- or#892 (2 of 2): validate the coordinate constraints and finish the NOT NULL.
--
-- Separate file = separate transaction, which is the ONLY shape that actually
-- reduces lock time here (internal/db/queries/EXEMPTIONS.md): 0042's NOT VALID
-- adds took a brief ACCESS EXCLUSIVE and released it at COMMIT; the scans below
-- take only SHARE UPDATE EXCLUSIVE and do not block reads or writes.
--
-- SET NOT NULL is free rather than a second full scan: PostgreSQL 12+ proves it
-- from the already-VALIDATEd `source IS NOT NULL AND source_id IS NOT NULL`
-- check added in 0042.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.ledger_transfers
    VALIDATE CONSTRAINT chk_ledger_transfers_source_present;

ALTER TABLE openrails.ledger_transfers
    VALIDATE CONSTRAINT chk_ledger_transfers_coordinate_not_blank;

ALTER TABLE openrails.ledger_transfers
    ALTER COLUMN source SET NOT NULL;

ALTER TABLE openrails.ledger_transfers
    ALTER COLUMN source_id SET NOT NULL;
