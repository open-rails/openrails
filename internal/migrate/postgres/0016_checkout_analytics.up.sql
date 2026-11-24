-- Checkout facts on the order: which purchase it is for its customer, how far
-- they got, where they are and which language they read, and the host's
-- attribution. A renewal names the checkout that bought its subscription.
ALTER TABLE billing.orders
    ADD COLUMN purchase_number integer,
    ADD COLUMN opened_at timestamptz,
    ADD COLUMN submitted_at timestamptz,
    ADD COLUMN billing_country text,
    ADD COLUMN card_country text,
    ADD COLUMN ip_country text,
    ADD COLUMN language text,
    ADD COLUMN attribution_source text,
    ADD COLUMN attribution_medium text,
    ADD COLUMN attribution_campaign text,
    ADD COLUMN attribution_referrer text,
    ADD COLUMN attribution_affiliate text,
    ADD COLUMN checkout_order_id uuid;

ALTER TABLE billing.orders ADD CONSTRAINT orders_purchase_number_check
    CHECK (purchase_number IS NULL OR (purchase_number >= 1 AND origin IN ('customer', 'merchant')));
ALTER TABLE billing.orders ADD CONSTRAINT orders_checkout_facts_check CHECK (
    purchase_number IS NOT NULL
    OR (opened_at IS NULL AND submitted_at IS NULL AND billing_country IS NULL AND card_country IS NULL AND ip_country IS NULL
        AND language IS NULL AND attribution_source IS NULL AND attribution_medium IS NULL AND attribution_campaign IS NULL
        AND attribution_referrer IS NULL AND attribution_affiliate IS NULL)
);
ALTER TABLE billing.orders ADD CONSTRAINT orders_submitted_check CHECK (submitted_at IS NULL OR opened_at IS NOT NULL);
ALTER TABLE billing.orders ADD CONSTRAINT orders_country_check CHECK (
    billing_country ~ '^[A-Z]{2}$' AND card_country ~ '^[A-Z]{2}$' AND ip_country ~ '^[A-Z]{2}$'
);
ALTER TABLE billing.orders ADD CONSTRAINT orders_language_check CHECK (language ~ '^[a-z]{2,3}(-([A-Z]{2}|[0-9]{3}))?$');
ALTER TABLE billing.orders ADD CONSTRAINT orders_attribution_check CHECK (
    octet_length(attribution_source) >= 1 AND octet_length(attribution_source) <= 100
    AND octet_length(attribution_medium) >= 1 AND octet_length(attribution_medium) <= 100
    AND octet_length(attribution_campaign) >= 1 AND octet_length(attribution_campaign) <= 100
    AND attribution_referrer ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$'
    AND octet_length(attribution_affiliate) >= 1 AND octet_length(attribution_affiliate) <= 100
);
ALTER TABLE billing.orders ADD CONSTRAINT orders_checkout_order_check CHECK (checkout_order_id IS NULL OR origin = 'subscription');
ALTER TABLE billing.orders ADD CONSTRAINT orders_checkout_order_id_fkey
    FOREIGN KEY (merchant_id, checkout_order_id) REFERENCES billing.orders (merchant_id, id) ON DELETE SET NULL (checkout_order_id) DEFERRABLE INITIALLY DEFERRED;

COMMENT ON TABLE billing.orders IS 'One payable (ord_ id): frozen lines and total in one currency, paid by at most one live order attempt at a time: a purchase, or a renewal of an engine-owned subscription. A decline leaves it open with last_payment_error; it is numbered from order_number_seq when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 397 days after they closed; every other order is permanent.';
COMMENT ON COLUMN billing.orders.purchase_number IS 'Set on a checkout (an order OpenRails started for its customer to pay): 1 + the customer''s paid purchases before it was created. NULL on every other order and on checkouts created before it was recorded.';
COMMENT ON COLUMN billing.orders.opened_at IS 'When the customer first read the open checkout in person (the payment form loaded), or created it with its payment.';
COMMENT ON COLUMN billing.orders.submitted_at IS 'When the customer first asked to pay the checkout, whatever the answer.';
COMMENT ON COLUMN billing.orders.billing_country IS 'ISO 3166-1 alpha-2 country of the billing address the latest pay request gave, or of the saved card it charged.';
COMMENT ON COLUMN billing.orders.card_country IS 'ISO 3166-1 alpha-2 issuing country of the card, as its PSP reports it.';
COMMENT ON COLUMN billing.orders.ip_country IS 'ISO 3166-1 alpha-2 country a trusted proxy reported for the customer''s first request in person. The address itself is never stored.';
COMMENT ON COLUMN billing.orders.language IS 'The language the customer''s first request in person preferred (Accept-Language), as language[-region].';
COMMENT ON COLUMN billing.orders.attribution_referrer IS 'The host of the referrer the merchant reported; never its path or query.';
COMMENT ON COLUMN billing.orders.checkout_order_id IS 'On a subscription''s bill: the checkout that bought the subscription, whose facts its money is reported under.';

CREATE INDEX orders_customer_purchases_idx ON billing.orders (merchant_id, customer_id)
    WHERE status = 'complete' AND (origin IN ('customer', 'merchant') OR subscription_bill_kind = 'initial');
CREATE INDEX orders_checkout_order_id_idx ON billing.orders (merchant_id, checkout_order_id)
    WHERE checkout_order_id IS NOT NULL;

-- Every writer of a subscription's bill (engine renewals, provider bills,
-- imports) names the checkout that bought the subscription.
CREATE FUNCTION billing.assign_checkout_order() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    SELECT o.id INTO NEW.checkout_order_id
    FROM billing.order_lines l
    JOIN billing.orders o ON o.merchant_id = l.merchant_id AND o.id = l.order_id
    WHERE l.merchant_id = NEW.merchant_id AND l.subscription_id = NEW.subscription_id
      AND o.purchase_number IS NOT NULL
    ORDER BY o.created_at, o.id
    LIMIT 1;
    RETURN NEW;
END;
$$;
CREATE TRIGGER assign_checkout_order BEFORE INSERT ON billing.orders
    FOR EACH ROW WHEN (NEW.origin = 'subscription' AND NEW.checkout_order_id IS NULL) EXECUTE FUNCTION billing.assign_checkout_order();
