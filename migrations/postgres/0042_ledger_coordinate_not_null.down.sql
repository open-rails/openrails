SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.idx_ledger_transfers_operation_once;

ALTER TABLE openrails.ledger_transfers
    DROP CONSTRAINT chk_ledger_transfers_coordinate_not_blank,
    DROP CONSTRAINT chk_ledger_transfers_source_present;

CREATE UNIQUE INDEX idx_ledger_transfers_credit_spend_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, operation, source, source_id, grant_id)
    WHERE transfer_type = 'credit_spend';

CREATE UNIQUE INDEX idx_ledger_transfers_owed_accrual_once
    ON openrails.ledger_transfers (merchant_id, customer_id, currency, operation, source, source_id)
    WHERE transfer_type = 'owed_accrual';
