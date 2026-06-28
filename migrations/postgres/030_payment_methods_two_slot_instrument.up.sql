-- #588: make the rail instrument handle explicit as two slots, hard-cutting the
-- overloaded vault_id (+ the NMI-ism billing_id).
--
--   rail_customer_ref = the customer-scope handle (NMI customer_vault_id; '' for
--                       rails whose customer scope lives in rail_customers, e.g.
--                       Stripe's cus_).
--   rail_method_ref   = the instrument-scope handle (NMI billing_id; the token for
--                       Stripe/Spreedly/HyperSwitch; whatever vault_id held for the rest).
--
-- Value-preserving hard cut: the per-rail backfill carries the EXACT handles the
-- charge paths already send to each provider, so no charge changes value — only the
-- column it reads from. No dual-read/compat lane is kept (no legacy support).
ALTER TABLE openrails.payment_methods
    ADD COLUMN IF NOT EXISTS rail_customer_ref text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS rail_method_ref   text NOT NULL DEFAULT '';

-- Per-rail value-preserving backfill of existing rows:
--   NMI-backed (mobius/nmi): vault_id IS the customer vault, billing_id the instrument.
--   all others (stripe/ccbill/...): vault_id IS the instrument token; customer scope
--   (if any) already lives in rail_customers.
UPDATE openrails.payment_methods SET
    rail_customer_ref = CASE WHEN rail IN ('mobius', 'nmi') THEN vault_id ELSE '' END,
    rail_method_ref   = CASE WHEN rail IN ('mobius', 'nmi') THEN COALESCE(billing_id, '') ELSE vault_id END;

-- Replace vault_id-scoped uniqueness with instrument-aware uniqueness so a single
-- customer vault can hold MORE THAN ONE stored instrument (the NMI multi-billing
-- case the old indexes forbade). Identity = (rail_customer_ref, rail_method_ref).
DROP INDEX IF EXISTS openrails.uq_payment_methods_customer_vault_legacy;
DROP INDEX IF EXISTS openrails.uq_payment_methods_merchant_rail_vault_legacy;
DROP INDEX IF EXISTS openrails.uq_payment_methods_provider_account_vault;
DROP INDEX IF EXISTS openrails.idx_payment_methods_vault_id;

CREATE UNIQUE INDEX uq_payment_methods_customer_instrument_legacy
    ON openrails.payment_methods (merchant_id, customer_id, rail_customer_ref, rail_method_ref)
    WHERE provider_account_id IS NULL;
CREATE UNIQUE INDEX uq_payment_methods_merchant_rail_instrument_legacy
    ON openrails.payment_methods (merchant_id, rail, rail_customer_ref, rail_method_ref)
    WHERE provider_account_id IS NULL;
CREATE UNIQUE INDEX uq_payment_methods_provider_account_instrument
    ON openrails.payment_methods (merchant_id, provider_account_id, rail_customer_ref, rail_method_ref)
    WHERE provider_account_id IS NOT NULL;
CREATE INDEX idx_payment_methods_method_ref ON openrails.payment_methods (rail, rail_method_ref);

-- Hard cut: drop the overloaded columns. initial_transaction_id is left in place
-- (a separate stored-credential anchor; its generalization is a later issue).
ALTER TABLE openrails.payment_methods
    DROP COLUMN IF EXISTS vault_id,
    DROP COLUMN IF EXISTS billing_id;

COMMENT ON COLUMN openrails.payment_methods.rail_customer_ref IS 'Customer-scope rail handle (e.g. NMI customer_vault_id); '''' when the customer scope lives in rail_customers (Stripe).';
COMMENT ON COLUMN openrails.payment_methods.rail_method_ref IS 'Instrument-scope rail handle (e.g. NMI billing_id, Stripe pm_, Spreedly/HyperSwitch token).';
