DROP TABLE IF EXISTS openrails.operation_authorizations;

ALTER TABLE ONLY openrails.ledger_accounts
    DROP CONSTRAINT IF EXISTS ledger_accounts_merchant_id_id_key;
