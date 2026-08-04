-- #832: the second half of the two-transaction CHECK. 0014 now adds
-- ledger_transfers_type_check already validated (or#838: NOT VALID buys nothing
-- inside a single-transaction migrator), so on a fresh database this file is a
-- no-op. It stays because a database that applied the earlier NOT VALID form of
-- 0014 still needs the validating scan, and it must not run in 0014's
-- ACCESS EXCLUSIVE transaction.
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.ledger_transfers
    VALIDATE CONSTRAINT ledger_transfers_type_check;
