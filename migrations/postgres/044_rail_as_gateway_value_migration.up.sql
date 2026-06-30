-- #630: model rail = gateway, provider account = instance. The white-label NMI
-- rail name "mobius" collapses into the gateway rail "nmi"; the account identity
-- "mobius" lives on as a provider-account NAME (provider_accounts), not a rail.
-- The off-rail recording channels (admin, manual) stay in the same source column
-- but are now a separate Channel concept in Go, not rails.
--
-- This migration is for EXISTING (pre-production) deployments built from the
-- previous baseline. On a fresh DB built from the updated 001 baseline these
-- statements are all no-ops (payments.rail is already text, no rail_type enum,
-- no 'mobius' rows/keys).

-- 1. payments.rail was the openrails.rail_type enum; make it plain text so a
--    value can be either a rail or an off-rail channel (admin/manual), and so the
--    'mobius' -> 'nmi' value migration is a simple text update.
ALTER TABLE openrails.payments
    ALTER COLUMN rail TYPE text USING rail::text;

-- 2. Collapse the white-label NMI rail name 'mobius' into the gateway rail 'nmi'
--    across every column that records a rail value.
UPDATE openrails.payments         SET rail = 'nmi' WHERE rail = 'mobius';
UPDATE openrails.subscriptions    SET rail = 'nmi' WHERE rail = 'mobius';
UPDATE openrails.payment_methods  SET rail = 'nmi' WHERE rail = 'mobius';
UPDATE openrails.rail_customers   SET rail = 'nmi' WHERE rail = 'mobius';
UPDATE openrails.checkout_sessions SET rail = 'nmi' WHERE rail = 'mobius';

-- provider_intents/refresh watermarks/provider_accounts record the rail in their
-- provider/provider_type columns; collapse any legacy 'mobius' there too.
UPDATE openrails.provider_intents            SET provider = 'nmi' WHERE provider = 'mobius';
UPDATE openrails.provider_refresh_watermarks SET provider = 'nmi' WHERE provider = 'mobius';
UPDATE openrails.provider_accounts           SET provider_type = 'nmi' WHERE provider_type = 'mobius';

-- 3. Migrate the price provider-link JSONB key 'mobius' -> 'nmi' (the catalog
--    adapter / provider_links key is now the gateway rail). If a price somehow
--    already carries an 'nmi' key, the legacy 'mobius' key is dropped.
UPDATE openrails.prices
    SET rails = jsonb_set(rails - 'mobius', '{nmi}', rails -> 'mobius')
    WHERE rails ? 'mobius' AND NOT (rails ? 'nmi');
UPDATE openrails.prices
    SET rails = rails - 'mobius'
    WHERE rails ? 'mobius' AND (rails ? 'nmi');

-- 4. Backfill provider_account_id for migrated NMI rows that lack one, from the
--    merchant's primary NMI provider account (best effort; rows with no declared
--    NMI account are left untouched for the operator to reconcile).
UPDATE openrails.payments p
    SET provider_account_id = pa.id
    FROM openrails.provider_accounts pa
    WHERE p.provider_account_id IS NULL
      AND p.rail = 'nmi'
      AND pa.merchant_id = p.merchant_id
      AND pa.provider_type = 'nmi'
      AND pa.role = 'primary'
      AND pa.status = 'enabled';

UPDATE openrails.subscriptions s
    SET provider_account_id = pa.id
    FROM openrails.provider_accounts pa
    WHERE s.provider_account_id IS NULL
      AND s.rail = 'nmi'
      AND pa.merchant_id = s.merchant_id
      AND pa.provider_type = 'nmi'
      AND pa.role = 'primary'
      AND pa.status = 'enabled';

-- 5. Drop the now-unused rail_type enum. payments.rail (its only user) is text now.
DROP TYPE IF EXISTS openrails.rail_type;
