-- or#823: drop catalog_credit_purchase_prices.round — a declared knob nothing
-- reads.
--
-- The column was written by the catalog sidecar push (syncCreditPurchases) and
-- read by nothing: the one runtime SELECT, money.loadCatalogCreditPurchase,
-- lists its columns explicitly and `round` is not among them, and
-- catalogCreditPurchaseRow has no field for it. It never even reached the
-- column with meaning attached — catalog.Price.ratePrice() builds the RatePrice
-- that becomes the `price` jsonb WITHOUT Round, so the top-level manifest
-- `round:` was validated and then dropped on the floor.
--
-- The rounding that IS applied to a credit-purchase quote lives inside the
-- `price` jsonb as per_unit.round, which RatePrice.ToChargeModel() carries into
-- ChargeModel.Round and mulDivRound honours. That path stays, now pinned by an
-- integration test that a non-default mode changes the quoted micros.
--
-- Not load-bearing for invertibility either: QuoteUnitsForSpend binary-searches
-- Rate(), so it inverts any MONOTONE model. Monotonicity is what the doctrine
-- requires and what graduated-only enforces (volume has tier cliffs); every
-- rounding mode preserves it.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- squawk ban-drop-column: "may break existing clients" is the right default and
-- the wrong verdict here, and squawk cannot tell the difference — it sees a
-- column name, not who reads it. Every client that named this column is in this
-- repo and stops naming it in this same commit: the sidecar INSERT
-- (service.syncCreditPurchases) and the embedded manifest dump
-- (dumpCatalogCreditPurchasePrices, which echoed it straight back out). No
-- query outside them ever selected it, so no read loses an answer and no write
-- loses a value. The one client the rule really protects — an older binary
-- still INSERTing the column against the new schema — is a catalog-apply /
-- dump path, not the money path, and migratekit runs at boot ahead of the
-- binary that needs it.
ALTER TABLE openrails.catalog_credit_purchase_prices
    -- squawk-ignore ban-drop-column
    DROP COLUMN round;
