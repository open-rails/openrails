-- Checkout facts on the order: who its customer is and which purchase it is
-- for them, how far they got, where they are and which language they read, and the host's
-- attribution. A subscription names the checkout that bought it, and each of
-- its bills names that checkout too.
--
-- Constraints are added NOT VALID and validated after. The migration runs in
-- one transaction (migratekit's default), so indexes are built in it, not
-- concurrently.
ALTER TABLE billing.orders
    ADD COLUMN customer_type text,
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
    ADD COLUMN attribution_term text,
    ADD COLUMN attribution_content text,
    ADD COLUMN attribution_referrer text,
    ADD COLUMN attribution_affiliate text,
    ADD COLUMN checkout_order_id uuid;
ALTER TABLE billing.subscriptions ADD COLUMN checkout_order_id uuid;

ALTER TABLE billing.orders ADD CONSTRAINT orders_purchase_number_check
    CHECK (purchase_number IS NULL OR (purchase_number >= 1 AND origin IN ('customer', 'merchant'))) NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_customer_type_check
    CHECK (customer_type IN ('new', 'returning', 'reactivated') AND (customer_type IS NULL) = (purchase_number IS NULL)) NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_checkout_facts_check CHECK (
    purchase_number IS NOT NULL
    OR (opened_at IS NULL AND submitted_at IS NULL AND billing_country IS NULL AND card_country IS NULL AND ip_country IS NULL
        AND language IS NULL AND attribution_source IS NULL AND attribution_medium IS NULL AND attribution_campaign IS NULL
        AND attribution_term IS NULL AND attribution_content IS NULL AND attribution_referrer IS NULL AND attribution_affiliate IS NULL)
) NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_submitted_check CHECK (submitted_at IS NULL OR opened_at IS NOT NULL) NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_country_check CHECK (
    billing_country ~ '^[A-Z]{2}$' AND card_country ~ '^[A-Z]{2}$' AND ip_country ~ '^[A-Z]{2}$'
) NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_language_check CHECK (language ~ '^[a-z]{2,3}(-([A-Z]{2}|[0-9]{3}))?$') NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_attribution_check CHECK (
    octet_length(attribution_source) >= 1 AND octet_length(attribution_source) <= 100
    AND octet_length(attribution_medium) >= 1 AND octet_length(attribution_medium) <= 100
    AND octet_length(attribution_campaign) >= 1 AND octet_length(attribution_campaign) <= 100
    AND octet_length(attribution_term) >= 1 AND octet_length(attribution_term) <= 100
    AND octet_length(attribution_content) >= 1 AND octet_length(attribution_content) <= 100
    AND attribution_referrer ~ '^[a-z0-9_]([a-z0-9_.-]{0,251}[a-z0-9_])?$'
    AND octet_length(attribution_affiliate) >= 1 AND octet_length(attribution_affiliate) <= 100
) NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_checkout_order_check CHECK (checkout_order_id IS NULL OR origin = 'subscription') NOT VALID;
ALTER TABLE billing.orders ADD CONSTRAINT orders_checkout_order_id_fkey
    FOREIGN KEY (merchant_id, checkout_order_id) REFERENCES billing.orders (merchant_id, id) ON DELETE SET NULL (checkout_order_id) DEFERRABLE INITIALLY DEFERRED NOT VALID;
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_checkout_order_id_fkey
    FOREIGN KEY (merchant_id, checkout_order_id) REFERENCES billing.orders (merchant_id, id) ON DELETE SET NULL (checkout_order_id) DEFERRABLE INITIALLY DEFERRED NOT VALID;

ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_purchase_number_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_customer_type_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_checkout_facts_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_submitted_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_country_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_language_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_attribution_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_checkout_order_check;
ALTER TABLE billing.orders VALIDATE CONSTRAINT orders_checkout_order_id_fkey;
ALTER TABLE billing.subscriptions VALIDATE CONSTRAINT subscriptions_checkout_order_id_fkey;

COMMENT ON TABLE billing.orders IS 'One payable (ord_ id): frozen lines and total in one currency, paid by at most one live order attempt at a time: a purchase, or a renewal of an engine-owned subscription. A decline leaves it open with last_payment_error; it is numbered from order_number_seq when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 397 days after they closed; every other order is permanent.';
COMMENT ON COLUMN billing.orders.customer_type IS 'Set on a checkout when it was created: reactivated when the customer had MRR with the merchant and has none now, new on a first purchase without MRR history, returning otherwise.';
COMMENT ON COLUMN billing.orders.purchase_number IS 'Set on a checkout (an order OpenRails started for its customer to pay): 1 + the customer''s paid purchases when it was created. Two checkouts open at once can share one. NULL on every other order and on checkouts created before it was recorded.';
COMMENT ON COLUMN billing.orders.opened_at IS 'When the customer first read the open checkout in person (the payment form loaded), or created it with its payment.';
COMMENT ON COLUMN billing.orders.submitted_at IS 'When the customer first asked to pay the checkout, whatever the answer.';
COMMENT ON COLUMN billing.orders.billing_country IS 'ISO 3166-1 alpha-2 country of the billing address the latest pay request gave, or of the saved card it charged.';
COMMENT ON COLUMN billing.orders.card_country IS 'ISO 3166-1 alpha-2 issuing country of the card; not recorded yet.';
COMMENT ON COLUMN billing.orders.ip_country IS 'ISO 3166-1 alpha-2 country a Cloudflare proxy reported for the customer''s first request in person. The address itself is never stored.';
COMMENT ON COLUMN billing.orders.language IS 'The language the customer''s first request in person preferred (Accept-Language), as language[-region].';
COMMENT ON COLUMN billing.orders.attribution_referrer IS 'The host of the referrer the merchant reported; never its path or query.';
COMMENT ON COLUMN billing.orders.checkout_order_id IS 'On a subscription''s bill: the checkout that bought the subscription, whose facts its money is reported under.';
COMMENT ON COLUMN billing.subscriptions.checkout_order_id IS 'The checkout whose payment created the subscription; NULL for one bought before checkouts were recorded, imported or started at its provider.';

CREATE INDEX orders_customer_purchases_idx ON billing.orders (merchant_id, customer_id)
    WHERE status = 'complete' AND (origin IN ('customer', 'merchant') OR subscription_bill_kind = 'initial');
CREATE INDEX orders_checkout_order_id_idx ON billing.orders (merchant_id, checkout_order_id)
    WHERE checkout_order_id IS NOT NULL;
CREATE INDEX subscriptions_checkout_order_id_idx ON billing.subscriptions (merchant_id, checkout_order_id)
    WHERE checkout_order_id IS NOT NULL;
-- Checkout measures read checkouts by creation and sales by payment.
CREATE INDEX orders_checkout_created_at_idx ON billing.orders (merchant_id, created_at)
    WHERE purchase_number IS NOT NULL;
CREATE INDEX orders_sale_completed_at_idx ON billing.orders (merchant_id, completed_at)
    WHERE purchase_number IS NOT NULL AND status = 'complete';

-- Naming its checkout is not a new version of a subscription.
CREATE OR REPLACE FUNCTION billing.subscriptions_row_version() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    unchanged billing.subscriptions := NEW;
BEGIN
    unchanged.due_recheck_at := OLD.due_recheck_at;
    unchanged.facts_authority := OLD.facts_authority;
    unchanged.row_version := OLD.row_version;
    unchanged.destructive_run_class := OLD.destructive_run_class;
    unchanged.rail_subscription_key := OLD.rail_subscription_key;
    unchanged.checkout_order_id := OLD.checkout_order_id;
    IF billing.rail_subscription_key(NEW.rail, NEW.rail_subscription_id) IS NOT DISTINCT FROM OLD.rail_subscription_key THEN
        unchanged.rail_subscription_id := OLD.rail_subscription_id;
    END IF;
    IF unchanged IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

-- Every writer of a subscription's bill (engine renewals, provider bills,
-- imports) copies the subscription's checkout: one row read.
CREATE FUNCTION billing.assign_checkout_order() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    SELECT s.checkout_order_id INTO NEW.checkout_order_id
    FROM billing.subscriptions s
    WHERE s.merchant_id = NEW.merchant_id AND s.id = NEW.subscription_id;
    RETURN NEW;
END;
$$;
CREATE TRIGGER assign_checkout_order BEFORE INSERT ON billing.orders
    FOR EACH ROW WHEN (NEW.origin = 'subscription' AND NEW.checkout_order_id IS NULL) EXECUTE FUNCTION billing.assign_checkout_order();

-- Saved dashboards drop every widget naming subscriber_type, which
-- customer_type replaced; a layout left without widgets is deleted, so its
-- merchant gets the health overview.
WITH widgets AS (
    SELECT c.merchant_id, w.widget, w.position,
        COALESCE(w.widget #> '{query,by}' ? 'subscriber_type', false)
            OR COALESCE(w.widget #> '{query,filters}' ? 'subscriber_type', false) AS stale
    FROM billing.dashboard_configs c
    CROSS JOIN LATERAL jsonb_array_elements(c.layout) WITH ORDINALITY AS w(widget, position)
    WHERE jsonb_typeof(c.layout) = 'array'
), cleaned AS (
    SELECT merchant_id, COALESCE(jsonb_agg(widget ORDER BY position) FILTER (WHERE NOT stale), '[]'::jsonb) AS layout
    FROM widgets
    GROUP BY merchant_id
    HAVING bool_or(stale)
), emptied AS (
    DELETE FROM billing.dashboard_configs c
    USING cleaned k
    WHERE c.merchant_id = k.merchant_id AND k.layout = '[]'::jsonb
)
UPDATE billing.dashboard_configs c
SET layout = k.layout, updated_at = now()
FROM cleaned k
WHERE c.merchant_id = k.merchant_id AND k.layout <> '[]'::jsonb;

COMMENT ON TABLE billing.dashboard_configs IS 'Per-merchant dashboard widget layout: [{id, title, viz (kpi|stat|line|area|bar|donut|table|cohort|funnel|heading|programs), query, grid{x,y,w,h}, fixed_range}]. Absent row = the health overview built in code for the merchant''s profile.';
