-- #832: validation runs in its own migratekit transaction so adding the CHECK
-- in 0014 does not hold an ACCESS EXCLUSIVE lock while existing rows are read.
SET lock_timeout = '5s';
SET statement_timeout = '5min';

ALTER TABLE openrails.ledger_transfers
    VALIDATE CONSTRAINT ledger_transfers_type_check;
