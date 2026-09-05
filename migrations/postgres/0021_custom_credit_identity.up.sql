SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

-- Product and financial snapshots must retain the same immutable unit identity.
CREATE FUNCTION openrails.credit_spec_has_canonical_units(spec jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE WHEN spec IS NULL THEN true WHEN jsonb_typeof(spec) <> 'object' THEN false ELSE NOT EXISTS (
  SELECT 1 FROM jsonb_each(spec) entry
  WHERE COALESCE(entry.value->>'unit','') !~ '^[A-Z0-9]{3,12}$'
    AND COALESCE(entry.value->>'unit','') !~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
 ) END
$$;

-- Never reinterpret a former merchant slug or rewrite append-only financial history.
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM openrails.products WHERE NOT openrails.credit_spec_has_canonical_units(credits_spec)) THEN RAISE EXCEPTION 'custom credit identity cutover: products.credits_spec contains noncanonical units. Export and explicitly recreate the affected pre-launch catalog; migration does not rewrite stored specs.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.subscriptions WHERE NOT openrails.credit_spec_has_canonical_units(credits_spec_snapshot)) THEN RAISE EXCEPTION 'custom credit identity cutover: subscriptions.credits_spec_snapshot contains noncanonical units. Inventory affected subscriptions and decide how to recreate pre-launch state; migration does not rewrite snapshots.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.payments WHERE NOT openrails.credit_spec_has_canonical_units(credits_spec_snapshot)) THEN RAISE EXCEPTION 'custom credit identity cutover: payments.credits_spec_snapshot contains noncanonical units. Inventory affected payments and decide how to recreate pre-launch state; migration does not rewrite history.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.metered_rating_watermarks WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.metered_rating_watermarks contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.catalog_credit_purchase_prices WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.catalog_credit_purchase_prices contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.host_lifecycle_events WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.host_lifecycle_events contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.invoices WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.invoices contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.ledger_accounts WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.ledger_accounts contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.ledger_transfers WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.ledger_transfers contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.prices WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.prices contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.tier_schedules WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.tier_schedules contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.usage_events WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.usage_events contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.customer_delinquency WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.customer_delinquency contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.customer_minimum_spend WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.customer_minimum_spend contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.invoice_items WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.invoice_items contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.invoice_payments WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.invoice_payments contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.money_settings WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.money_settings contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.payments WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.payments contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.payment_settlement_events WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.payment_settlement_events contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.checkout_sessions WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.checkout_sessions contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.grants WHERE currency LIKE '%/%') THEN RAISE EXCEPTION 'custom credit identity cutover: openrails.grants contains mutable unit codes. Inventory the affected merchant/customer rows and decide how to recreate this pre-launch data; migration performs no history rewrite.'; END IF;
 IF EXISTS(SELECT 1 FROM openrails.catalog_credit_balances WHERE unit !~ '^[A-Z0-9]{3,12}$' AND unit !~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') THEN RAISE EXCEPTION 'custom credit identity cutover: catalog_credit_balances has name-based units. Export and explicitly recreate these mutable catalog definitions after upgrade; financial history is never rewritten.'; END IF;
END $$;

ALTER TABLE openrails.metered_rating_watermarks DROP CONSTRAINT metered_rating_watermarks_currency_shape;
ALTER TABLE openrails.metered_rating_watermarks ADD CONSTRAINT metered_rating_watermarks_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.catalog_credit_purchase_prices DROP CONSTRAINT catalog_credit_purchase_prices_currency_shape;
ALTER TABLE openrails.catalog_credit_purchase_prices ADD CONSTRAINT catalog_credit_purchase_prices_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.host_lifecycle_events DROP CONSTRAINT host_lifecycle_events_currency_shape;
ALTER TABLE openrails.host_lifecycle_events ADD CONSTRAINT host_lifecycle_events_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.invoices DROP CONSTRAINT invoices_currency_shape;
ALTER TABLE openrails.invoices ADD CONSTRAINT invoices_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.ledger_accounts DROP CONSTRAINT ledger_accounts_currency_shape;
ALTER TABLE openrails.ledger_accounts ADD CONSTRAINT ledger_accounts_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.ledger_transfers DROP CONSTRAINT ledger_transfers_currency_shape;
ALTER TABLE openrails.ledger_transfers ADD CONSTRAINT ledger_transfers_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.prices DROP CONSTRAINT prices_currency_shape;
ALTER TABLE openrails.prices ADD CONSTRAINT prices_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.tier_schedules DROP CONSTRAINT tier_schedules_currency_shape;
ALTER TABLE openrails.tier_schedules ADD CONSTRAINT tier_schedules_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.usage_events DROP CONSTRAINT usage_events_currency_shape;
ALTER TABLE openrails.usage_events ADD CONSTRAINT usage_events_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.customer_delinquency DROP CONSTRAINT customer_delinquency_currency_shape;
ALTER TABLE openrails.customer_delinquency ADD CONSTRAINT customer_delinquency_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.customer_minimum_spend DROP CONSTRAINT customer_minimum_spend_currency_shape;
ALTER TABLE openrails.customer_minimum_spend ADD CONSTRAINT customer_minimum_spend_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.invoice_items DROP CONSTRAINT invoice_items_currency_shape;
ALTER TABLE openrails.invoice_items ADD CONSTRAINT invoice_items_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.invoice_payments DROP CONSTRAINT invoice_payments_currency_shape;
ALTER TABLE openrails.invoice_payments ADD CONSTRAINT invoice_payments_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.money_settings DROP CONSTRAINT money_settings_currency_shape;
ALTER TABLE openrails.money_settings ADD CONSTRAINT money_settings_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.payments DROP CONSTRAINT payments_currency_shape;
ALTER TABLE openrails.payments ADD CONSTRAINT payments_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.payment_settlement_events DROP CONSTRAINT payment_settlement_events_currency_shape;
ALTER TABLE openrails.payment_settlement_events ADD CONSTRAINT payment_settlement_events_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.checkout_sessions DROP CONSTRAINT checkout_sessions_currency_shape;
ALTER TABLE openrails.checkout_sessions ADD CONSTRAINT checkout_sessions_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.grants DROP CONSTRAINT grants_currency_shape;
ALTER TABLE openrails.grants ADD CONSTRAINT grants_currency_shape CHECK(currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

COMMENT ON TABLE openrails.custom_credit_types IS 'Merchant-owned custom credit identities and scales. Financial rows reference credit:<id>; external names resolve through the current merchant namespace.';

ALTER TABLE openrails.catalog_credit_balances ADD CONSTRAINT catalog_credit_balances_unit_identity CHECK(unit ~ '^[A-Z0-9]{3,12}$' OR unit ~ '^credit:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') NOT VALID;

ALTER TABLE openrails.products ADD CONSTRAINT products_credit_units_canonical CHECK(openrails.credit_spec_has_canonical_units(credits_spec)) NOT VALID;
ALTER TABLE openrails.subscriptions ADD CONSTRAINT subscriptions_credit_units_canonical CHECK(openrails.credit_spec_has_canonical_units(credits_spec_snapshot)) NOT VALID;
ALTER TABLE openrails.payments ADD CONSTRAINT payments_credit_units_canonical CHECK(openrails.credit_spec_has_canonical_units(credits_spec_snapshot)) NOT VALID;
