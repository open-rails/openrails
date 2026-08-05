-- Restore catalog_credit_purchase_prices.round in its 0001 shape (nullable
-- text, no default). Values are NOT recoverable and none ever mattered: the
-- column was written and never read, so an empty column restores exactly the
-- information content the deployment had.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.catalog_credit_purchase_prices
    ADD COLUMN round text;
