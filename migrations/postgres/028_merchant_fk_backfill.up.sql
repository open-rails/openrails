-- #(advisor-006) Backfill merchant_id FKs on core merchant-owned tables.
-- ON DELETE RESTRICT (not CASCADE): catalog/financial rows must never be
-- silently destroyed by a merchant delete. Merchants are not hard-deleted today
-- (no DELETE FROM merchants in internal/db/queries/; suspension removed in 022),
-- so this is protective only and changes no current runtime behavior.
--
-- Tables already covered (skipped here):
--   customers              (001: customers_merchant_id_fkey)
--   merchant_configurations (001: merchant_configurations_merchant_fk)
--   provider_accounts      (009: provider_accounts_merchant_fk, CASCADE)
--   external_provider_mutation_logs (019: ..._merchant_fk, CASCADE)
--   provider_refresh_watermarks     (027: ..._merchant_fk, CASCADE)

ALTER TABLE openrails.products
    ADD CONSTRAINT products_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.entitlement_features
    ADD CONSTRAINT entitlement_features_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.product_entitlement_features
    ADD CONSTRAINT product_entitlement_features_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.payment_methods
    ADD CONSTRAINT payment_methods_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.checkout_sessions
    ADD CONSTRAINT checkout_sessions_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.grants
    ADD CONSTRAINT grants_merchant_fk
    FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
