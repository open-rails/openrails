-- Repair: preserve Each NMI, Stripe or CCBill binding of a price becomes a provider price of its own, on the same product and terms, keyed <price key>-<psp key>; the provider-run subscriptions on that PSP move to it. Engine subscriptions and Solana plans stay on our price.
-- A price is ours (OpenRails vaults the card and runs the billing; nothing is
-- written to a provider's catalog) or a provider's (the provider owns and runs
-- it; OpenRails reads its terms and caches them on the row).
ALTER TABLE billing.prices ADD COLUMN provider_owned boolean NOT NULL DEFAULT false;
-- One of ours may sell through only some of the merchant's methods.
ALTER TABLE billing.prices ADD COLUMN sell_via text[];
ALTER TABLE billing.prices ADD CONSTRAINT prices_sell_via_check CHECK (sell_via IS NULL OR (NOT provider_owned AND cardinality(sell_via) > 0 AND sell_via <@ ARRAY['card', 'solana']::text[]));
COMMENT ON COLUMN billing.prices.sell_via IS 'The methods one of our prices sells through: card (the merchant''s card vault and charge PSPs) and solana. NULL: every method the merchant has. Settable like archived; it is not a term.';
DROP TRIGGER immutable_price_terms ON billing.prices;
CREATE TRIGGER immutable_price_terms BEFORE UPDATE OR DELETE ON billing.prices
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('archived', 'updated_at', 'deleted_at', 'sell_via');
COMMENT ON COLUMN billing.prices.provider_owned IS 'A provider owns and runs the price: its one price_psp_bindings row names the PSP and the provider object, whose terms this row caches. False: ours, bound only to the Solana plans OpenRails published for it.';

-- A split provider price's id derives from the price and PSP it splits.
INSERT INTO billing.prices (id, merchant_id, product_id, archived, amount, currency, access_duration_hours, billing_interval_hours,
    trial_unit_amount, trial_duration_hours, customer_amount, quantity, key, provider_owned, created_at, updated_at, deleted_at)
SELECT md5(p.id::text || ':' || psp.id::text)::uuid, p.merchant_id, p.product_id, p.archived, p.amount, p.currency,
    p.access_duration_hours, p.billing_interval_hours, p.trial_unit_amount, p.trial_duration_hours, p.customer_amount, p.quantity,
    p.key || '-' || psp.key || CASE WHEN psp.superseded_at IS NULL THEN '' ELSE '-' || left(psp.id::text, 8) END,
    true, p.created_at, p.updated_at, p.deleted_at
FROM billing.price_psp_bindings b
JOIN billing.psps psp ON psp.merchant_id = b.merchant_id AND psp.id = b.psp_id
JOIN billing.prices p ON p.merchant_id = b.merchant_id AND p.id = b.price_id
WHERE psp.rail IN ('nmi', 'stripe', 'ccbill')
ORDER BY p.created_at, p.id, psp.id;

UPDATE billing.subscriptions s SET price_id = md5(s.price_id::text || ':' || s.psp_id::text)::uuid
FROM billing.price_psp_bindings b
JOIN billing.psps psp ON psp.merchant_id = b.merchant_id AND psp.id = b.psp_id
WHERE b.merchant_id = s.merchant_id AND b.price_id = s.price_id AND b.psp_id = s.psp_id
  AND psp.rail IN ('nmi', 'stripe', 'ccbill') AND s.collection_policy <> 'engine';

UPDATE billing.price_psp_bindings b SET price_id = md5(b.price_id::text || ':' || b.psp_id::text)::uuid
FROM billing.psps psp
WHERE psp.merchant_id = b.merchant_id AND psp.id = b.psp_id AND psp.rail IN ('nmi', 'stripe', 'ccbill');

-- A provider price's document field is its provider reference; prices name no PSPs.
DELETE FROM billing.catalog_field_owners WHERE object = 'price' AND field = 'psps';
ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_field_check;
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_field_check CHECK ((
    ((object = 'product'::text) AND (field = ANY (ARRAY['display_name'::text, 'description'::text, 'tier_group'::text, 'tier_rank'::text, 'archived'::text, 'ownership'::text, 'entitlements'::text, 'required_entitlements'::text, 'credit_grant'::text, 'rate_cards'::text])))
    OR ((object = 'price'::text) AND (field = ANY (ARRAY['currency'::text, 'unit_amount'::text, 'access_duration_hours'::text, 'billing_interval_hours'::text, 'trial_unit_amount'::text, 'trial_duration_hours'::text, 'customer_amount'::text, 'quantity'::text, 'archived'::text, 'sell_via'::text, 'provider'::text])))
    OR ((object = 'meter'::text) AND (field = ANY (ARRAY['event_type'::text, 'value_property'::text, 'aggregation'::text, 'unit'::text, 'group_by'::text])))));

-- Our prices bind only the Solana plans OpenRails publishes; a provider price
-- binds exactly its one NMI, Stripe or CCBill object.
CREATE FUNCTION billing.guard_price_binding_owner() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE owned boolean; binding_rail text;
BEGIN
    SELECT p.provider_owned INTO owned FROM billing.prices p WHERE p.merchant_id = NEW.merchant_id AND p.id = NEW.price_id;
    SELECT s.rail INTO binding_rail FROM billing.psps s WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.psp_id;
    IF owned IS DISTINCT FROM (binding_rail IN ('nmi', 'stripe', 'ccbill')) THEN
        RAISE EXCEPTION 'price % cannot bind a % object: our prices bind only Solana plans, provider prices only their provider''s object', NEW.price_id, binding_rail USING ERRCODE = '23514';
    END IF;
    IF owned AND EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = NEW.merchant_id AND b.price_id = NEW.price_id AND b.psp_id <> NEW.psp_id) THEN
        RAISE EXCEPTION 'provider price % binds one PSP', NEW.price_id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER price_binding_owner BEFORE INSERT OR UPDATE ON billing.price_psp_bindings
FOR EACH ROW EXECUTE FUNCTION billing.guard_price_binding_owner();
COMMENT ON TABLE billing.price_psp_bindings IS 'A price''s objects at its PSPs: the one provider object a provider price mirrors, or the Solana plans OpenRails published for one of ours. Each rail uses its own reference column.';
