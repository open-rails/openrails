-- Separate file = separate transaction: the only shape that actually reduces
-- lock time here (internal/db/queries/EXEMPTIONS.md).
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';
ALTER TABLE openrails.ledger_transfers VALIDATE CONSTRAINT ledger_transfers_type_check;
