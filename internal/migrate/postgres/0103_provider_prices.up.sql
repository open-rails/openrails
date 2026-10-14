-- Repair: preserve Each NMI, Stripe or CCBill binding of a price becomes a provider price of its own, on the same product and terms, keyed <price key>-<psp key>; the provider-run subscriptions on that PSP move to it. Engine subscriptions and Solana plans stay on our price.
-- A price is ours (OpenRails vaults the card and runs the billing; nothing is
-- written to a provider's catalog) or a provider's (the provider owns and runs
-- it; OpenRails reads its terms and caches them on the row).
-- A provider agreement without its provider object's identity cannot be split
-- safely. Link its existing object on the previous release before upgrading.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM billing.subscriptions s JOIN billing.psps psp ON psp.merchant_id = s.merchant_id AND psp.id = s.psp_id
        LEFT JOIN billing.price_psp_bindings b ON b.merchant_id = s.merchant_id AND b.price_id = s.price_id AND b.psp_id = s.psp_id
        WHERE s.collection_policy <> 'engine' AND s.price_id IS NOT NULL AND psp.rail IN ('nmi', 'stripe', 'ccbill')
            AND NOT COALESCE((psp.rail = 'nmi' AND b.plan_id IS NOT NULL) OR (psp.rail = 'stripe' AND b.price_ref IS NOT NULL)
                OR (psp.rail = 'ccbill' AND (b.flex_id IS NOT NULL OR b.recurring_billing_option_id IS NOT NULL)), false)
    ) THEN
        RAISE EXCEPTION 'a provider-run subscription lacks its provider price reference; link its existing NMI plan, Stripe price or CCBill object using OpenRails v0.255.0 before upgrading' USING ERRCODE = '23514';
    END IF;
END $$;

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
    p.key || '-' || psp.key || CASE WHEN psp.superseded_at IS NULL THEN '' ELSE '-' || left(psp.id::text, 8) END ||
    CASE WHEN EXISTS (SELECT 1 FROM billing.prices collision WHERE collision.merchant_id = p.merchant_id AND collision.product_id = p.product_id
        AND collision.key = p.key || '-' || psp.key || CASE WHEN psp.superseded_at IS NULL THEN '' ELSE '-' || left(psp.id::text, 8) END)
        THEN '-provider-' || md5(p.product_id::text || ':' || p.key || ':' || psp.id::text) ELSE '' END,
    true, p.created_at, p.updated_at, p.deleted_at
FROM billing.price_psp_bindings b
JOIN billing.psps psp ON psp.merchant_id = b.merchant_id AND psp.id = b.psp_id
JOIN billing.prices p ON p.merchant_id = b.merchant_id AND p.id = b.price_id
WHERE (psp.rail = 'nmi' AND b.plan_id IS NOT NULL) OR (psp.rail = 'stripe' AND b.price_ref IS NOT NULL)
    OR (psp.rail = 'ccbill' AND (b.flex_id IS NOT NULL OR b.recurring_billing_option_id IS NOT NULL))
ORDER BY p.created_at, p.id, psp.id;

UPDATE billing.subscriptions s SET price_id = md5(s.price_id::text || ':' || s.psp_id::text)::uuid
FROM billing.price_psp_bindings b
JOIN billing.psps psp ON psp.merchant_id = b.merchant_id AND psp.id = b.psp_id
WHERE b.merchant_id = s.merchant_id AND b.price_id = s.price_id AND b.psp_id = s.psp_id
  AND psp.rail IN ('nmi', 'stripe', 'ccbill') AND s.collection_policy <> 'engine';

-- Future scheduled changes of a provider-run subscription keep their provider
-- price on that subscription's account. Already applied/canceled history stays.
UPDATE billing.scheduled_changes c SET
    from_price_id = CASE WHEN EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = c.merchant_id AND b.price_id = c.from_price_id AND b.psp_id = s.psp_id
        AND ((psp.rail = 'nmi' AND b.plan_id IS NOT NULL) OR (psp.rail = 'stripe' AND b.price_ref IS NOT NULL)
            OR (psp.rail = 'ccbill' AND (b.flex_id IS NOT NULL OR b.recurring_billing_option_id IS NOT NULL))))
        THEN md5(c.from_price_id::text || ':' || s.psp_id::text)::uuid ELSE c.from_price_id END,
    price_id = CASE WHEN EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = c.merchant_id AND b.price_id = c.price_id AND b.psp_id = s.psp_id
        AND ((psp.rail = 'nmi' AND b.plan_id IS NOT NULL) OR (psp.rail = 'stripe' AND b.price_ref IS NOT NULL)
            OR (psp.rail = 'ccbill' AND (b.flex_id IS NOT NULL OR b.recurring_billing_option_id IS NOT NULL))))
        THEN md5(c.price_id::text || ':' || s.psp_id::text)::uuid ELSE c.price_id END
FROM billing.subscriptions s JOIN billing.psps psp ON psp.merchant_id = s.merchant_id AND psp.id = s.psp_id
WHERE s.merchant_id = c.merchant_id AND s.id = c.subscription_id AND s.collection_policy <> 'engine'
    AND psp.rail IN ('nmi', 'stripe', 'ccbill') AND c.status IN ('scheduled', 'blocked');

UPDATE billing.price_psp_bindings b SET price_id = md5(b.price_id::text || ':' || b.psp_id::text)::uuid,
    configuration = b.configuration || jsonb_build_object('provider_billing_interval_hours', COALESCE(p.billing_interval_hours::text, 'none'))
FROM billing.psps psp, billing.prices p
WHERE psp.merchant_id = b.merchant_id AND psp.id = b.psp_id AND psp.rail IN ('nmi', 'stripe', 'ccbill')
    AND p.merchant_id = b.merchant_id AND p.id = b.price_id
    AND ((psp.rail = 'nmi' AND b.plan_id IS NOT NULL) OR (psp.rail = 'stripe' AND b.price_ref IS NOT NULL)
        OR (psp.rail = 'ccbill' AND (b.flex_id IS NOT NULL OR b.recurring_billing_option_id IS NOT NULL)));

-- Old empty card bindings expressed checkout eligibility, not provider
-- objects. Our prices now sell through merchant charge PSPs without bindings.
DELETE FROM billing.price_psp_bindings b USING billing.psps psp
WHERE psp.merchant_id = b.merchant_id AND psp.id = b.psp_id
    AND ((psp.rail = 'nmi' AND b.plan_id IS NULL) OR (psp.rail = 'stripe' AND b.price_ref IS NULL)
        OR (psp.rail = 'ccbill' AND b.flex_id IS NULL AND b.recurring_billing_option_id IS NULL));

-- A provider price's document field is its provider reference; prices name no PSPs.
DELETE FROM billing.catalog_field_owners WHERE object = 'price' AND field = 'psps';
ALTER TABLE billing.catalog_field_owners DROP CONSTRAINT catalog_field_owners_field_check;
ALTER TABLE billing.catalog_field_owners ADD CONSTRAINT catalog_field_owners_field_check CHECK ((
    ((object = 'product'::text) AND (field = ANY (ARRAY['display_name'::text, 'description'::text, 'tier_group'::text, 'tier_rank'::text, 'archived'::text, 'ownership'::text, 'entitlements'::text, 'required_entitlements'::text, 'credit_grant'::text, 'rate_cards'::text])))
    OR ((object = 'price'::text) AND (field = ANY (ARRAY['currency'::text, 'unit_amount'::text, 'access_duration_hours'::text, 'billing_interval_hours'::text, 'trial_unit_amount'::text, 'trial_duration_hours'::text, 'customer_amount'::text, 'quantity'::text, 'archived'::text, 'sell_via'::text, 'provider'::text])))
    OR ((object = 'meter'::text) AND (field = ANY (ARRAY['event_type'::text, 'value_property'::text, 'aggregation'::text, 'unit'::text, 'group_by'::text])))));

-- Our prices bind only the Solana plans OpenRails publishes; a provider price
-- binds exactly its one NMI, Stripe or CCBill object.
-- Provider references take one price binding even under repeatable-read
-- snapshots. Solana plans name plan_pda and are excluded from this index.
CREATE UNIQUE INDEX provider_price_one_binding_key ON billing.price_psp_bindings (merchant_id, price_id)
WHERE plan_pda IS NULL AND (plan_id IS NOT NULL OR price_ref IS NOT NULL OR flex_id IS NOT NULL OR recurring_billing_option_id IS NOT NULL);

CREATE FUNCTION billing.guard_price_binding_owner() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE owned boolean; binding_rail text;
BEGIN
    SELECT p.provider_owned INTO owned FROM billing.prices p WHERE p.merchant_id = NEW.merchant_id AND p.id = NEW.price_id FOR UPDATE;
    SELECT s.rail INTO binding_rail FROM billing.psps s WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.psp_id;
    IF owned IS DISTINCT FROM (binding_rail IN ('nmi', 'stripe', 'ccbill')) THEN
        RAISE EXCEPTION 'price % cannot bind a % object: our prices bind only Solana plans, provider prices only their provider''s object', NEW.price_id, binding_rail USING ERRCODE = '23514';
    END IF;
    IF owned AND EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = NEW.merchant_id AND b.price_id = NEW.price_id AND b.psp_id <> NEW.psp_id) THEN
        RAISE EXCEPTION 'provider price % binds one PSP', NEW.price_id USING ERRCODE = '23514';
    END IF;
    IF owned AND (NEW.plan_pda IS NOT NULL
        OR (binding_rail = 'nmi' AND (NEW.plan_id IS NULL OR NEW.price_ref IS NOT NULL OR NEW.flex_id IS NOT NULL OR NEW.recurring_billing_option_id IS NOT NULL))
        OR (binding_rail = 'stripe' AND (NEW.price_ref IS NULL OR NEW.plan_id IS NOT NULL OR NEW.flex_id IS NOT NULL OR NEW.recurring_billing_option_id IS NOT NULL))
        OR (binding_rail = 'ccbill' AND (NEW.plan_id IS NOT NULL OR NEW.price_ref IS NOT NULL OR (NEW.flex_id IS NULL AND NEW.recurring_billing_option_id IS NULL)))) THEN
        RAISE EXCEPTION 'provider price % must name its provider object on %', NEW.price_id, binding_rail USING ERRCODE = '23514';
    END IF;
    IF NOT owned AND (NEW.price_ref IS NOT NULL OR NEW.flex_id IS NOT NULL OR NEW.recurring_billing_option_id IS NOT NULL OR (NEW.plan_id IS NOT NULL AND NEW.plan_pda IS NULL)) THEN
        RAISE EXCEPTION 'our price % binds only a published Solana plan', NEW.price_id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER price_binding_owner BEFORE INSERT OR UPDATE ON billing.price_psp_bindings
FOR EACH ROW EXECUTE FUNCTION billing.guard_price_binding_owner();
COMMENT ON TABLE billing.price_psp_bindings IS 'A price''s objects at its PSPs: the one provider object a provider price mirrors, or the Solana plans OpenRails published for one of ours. Each rail uses its own reference column.';
