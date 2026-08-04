SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.ledger_transfers
    ALTER COLUMN source DROP NOT NULL,
    ALTER COLUMN source_id DROP NOT NULL;
