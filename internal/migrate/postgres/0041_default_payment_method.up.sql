-- parent: 40 sha256:9290dc29214a22da9316ff981c9f04da768aa8174e47cabdf0197698f3bcf119
-- The two-level default card. A customer's default card per currency pays
-- their invoices in it and every card subscription in it without a card of its
-- own; a subscription's payment_method_id becomes that override.
ALTER TABLE billing.money_settings RENAME COLUMN collection_payment_method_id TO default_payment_method_id;
ALTER TABLE billing.money_settings RENAME CONSTRAINT money_settings_customer_id_collection_payment_method_id_fkey TO money_settings_customer_id_default_payment_method_id_fkey;
COMMENT ON COLUMN billing.money_settings.default_payment_method_id IS 'The customer''s default card in this currency: it collects their invoices and pays every card subscription in the currency without a card of its own.';
COMMENT ON COLUMN billing.subscriptions.payment_method_id IS 'The subscription''s own card. NULL on a card subscription (NMI, or Stripe collected by OpenRails) follows its customer''s default card for the currency of its price.';

-- A card that is no longer active is nobody's default; its followers wait for
-- a new one.
UPDATE billing.money_settings ms SET default_payment_method_id = NULL, updated_at = now()
FROM billing.payment_methods pm
WHERE pm.merchant_id = ms.merchant_id AND pm.id = ms.default_payment_method_id AND pm.status <> 'active';

CREATE FUNCTION billing.release_inactive_default() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF OLD.status = 'active' AND NEW.status <> 'active' THEN
        UPDATE billing.money_settings SET default_payment_method_id = NULL, updated_at = now()
        WHERE merchant_id = NEW.merchant_id AND customer_id = NEW.customer_id AND default_payment_method_id = NEW.id;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER release_inactive_default AFTER UPDATE OF status ON billing.payment_methods FOR EACH ROW EXECUTE FUNCTION billing.release_inactive_default();

-- The card a subscription charges: its own, else, on a card subscription, its
-- customer's default for the currency of its price.
CREATE FUNCTION billing.subscription_payment_method_id(p_merchant uuid, p_customer uuid, p_own uuid, p_price uuid, p_rail text, p_policy text) RETURNS uuid
    LANGUAGE sql STABLE PARALLEL SAFE
    AS $$
SELECT COALESCE(p_own, (
    SELECT ms.default_payment_method_id
    FROM billing.prices p
    JOIN billing.money_settings ms ON ms.merchant_id = p.merchant_id AND ms.customer_id = p_customer AND ms.currency = p.currency
    WHERE p.merchant_id = p_merchant AND p.id = p_price
      AND p_rail IN ('nmi', 'stripe') AND p_policy IN ('engine', 'nmi_schedule')))
$$;
COMMENT ON FUNCTION billing.subscription_payment_method_id(p_merchant uuid, p_customer uuid, p_own uuid, p_price uuid, p_rail text, p_policy text) IS 'The card a subscription charges: its own (payment_method_id), else on a card subscription its customer''s default for its price''s currency.';

-- A subscription whose own card is already its customer's default follows it.
UPDATE billing.subscriptions s SET payment_method_id = NULL
FROM billing.prices p, billing.money_settings ms
WHERE p.merchant_id = s.merchant_id AND p.id = s.price_id
  AND ms.merchant_id = s.merchant_id AND ms.customer_id = s.customer_id AND ms.currency = p.currency
  AND s.payment_method_id = ms.default_payment_method_id
  AND s.rail IN ('nmi', 'stripe') AND s.collection_policy IN ('engine', 'nmi_schedule');
