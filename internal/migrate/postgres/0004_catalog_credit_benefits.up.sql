-- parent: 3 sha256:ddeb68c931bf73e826b3696f62a28c24603ead460fcbaac083e35a8ba9e2cac0
-- Repair: none-needed New nullable columns preserve every existing row; old prices have no customer amount range or credit benefit.
ALTER TABLE billing.products ADD COLUMN credit_grant jsonb;
ALTER TABLE billing.prices ADD COLUMN customer_amount jsonb;
ALTER TABLE billing.payments ADD COLUMN credit_grant_snapshot jsonb;

ALTER TABLE billing.products ADD CONSTRAINT products_credit_grant_object
    CHECK (credit_grant IS NULL OR (
        jsonb_typeof(credit_grant) = 'object'
        AND COALESCE(credit_grant->>'currency' ~ '^[A-Z0-9]{3,12}$', false)
        AND COALESCE((credit_grant->>'expires_after_days')::int BETWEEN 1 AND 36500, false)
        AND (
            (COALESCE((credit_grant->>'amount')::bigint > 0, false) AND NOT COALESCE((credit_grant->>'from_payment')::boolean, false))
            OR (NOT credit_grant ? 'amount' AND COALESCE((credit_grant->>'from_payment')::boolean, false))
        )
    ));
ALTER TABLE billing.prices ADD CONSTRAINT prices_customer_amount_terms
    CHECK (customer_amount IS NULL OR (
        jsonb_typeof(customer_amount) = 'object' AND amount = 0 AND NOT auto_renew
        AND access_duration_hours IS NULL AND trial_unit_amount IS NULL AND trial_duration_hours IS NULL
        AND customer_amount ? 'min_amount' AND customer_amount ? 'max_amount'
        AND COALESCE((customer_amount->>'min_amount')::bigint > 0, false)
        AND COALESCE((customer_amount->>'max_amount')::bigint >= (customer_amount->>'min_amount')::bigint, false)
    ));
ALTER TABLE billing.prices DROP CONSTRAINT prices_product_amount_window_key;
ALTER TABLE billing.prices ADD CONSTRAINT prices_product_amount_window_key
    UNIQUE NULLS NOT DISTINCT (merchant_id, product_id, key, amount, currency, access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours, customer_amount);

COMMENT ON COLUMN billing.products.credit_grant IS 'Purchased currency credit policy; accepted checkouts freeze amount and expiry duration.';
COMMENT ON COLUMN billing.prices.customer_amount IS 'Immutable inclusive customer-selected deposit bounds in currency micros; NULL means fixed amount.';
COMMENT ON COLUMN billing.payments.credit_grant_snapshot IS 'Accepted credit promise and first successful fulfillment dates; independent of subsequent catalog edits.';
