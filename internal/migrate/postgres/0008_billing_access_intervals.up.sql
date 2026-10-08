-- parent: 7 sha256:0a6ac0886ba4b019a0187612d2549b4b03d8077a86e4818b694cbe253ff72158
-- Repair: preserve Existing recurring prices retain their billing cadence and
-- access terms. Existing subscriptions retain their accepted access duration;
-- grant history and already accepted checkout promises are not rewritten.
ALTER TABLE billing.prices ADD COLUMN billing_interval_hours integer;
ALTER TABLE billing.prices DISABLE TRIGGER immutable_price_terms;
ALTER TABLE billing.prices DISABLE TRIGGER catalog_authored_price;
UPDATE billing.prices SET billing_interval_hours = access_duration_hours WHERE auto_renew;
ALTER TABLE billing.prices ENABLE TRIGGER catalog_authored_price;
ALTER TABLE billing.prices ENABLE TRIGGER immutable_price_terms;
ALTER TABLE billing.prices
    DROP CONSTRAINT prices_auto_renew_needs_duration_check,
    DROP CONSTRAINT prices_trial_needs_auto_renew_check,
    DROP CONSTRAINT prices_customer_amount_terms,
    DROP CONSTRAINT prices_product_amount_window_key,
    DROP COLUMN auto_renew;
ALTER TABLE billing.prices
    ADD CONSTRAINT prices_billing_interval_positive_check
        CHECK (billing_interval_hours IS NULL OR billing_interval_hours > 0),
    ADD CONSTRAINT prices_trial_needs_billing_interval_check
        CHECK (trial_unit_amount IS NULL OR billing_interval_hours IS NOT NULL),
    ADD CONSTRAINT prices_customer_amount_terms CHECK (customer_amount IS NULL OR (
        jsonb_typeof(customer_amount) = 'object' AND amount = 0 AND billing_interval_hours IS NULL
        AND access_duration_hours IS NULL AND trial_unit_amount IS NULL AND trial_duration_hours IS NULL
        AND customer_amount ? 'min_amount' AND customer_amount ? 'max_amount'
        AND COALESCE((customer_amount->>'min_amount')::bigint > 0, false)
        AND COALESCE((customer_amount->>'max_amount')::bigint >= (customer_amount->>'min_amount')::bigint, false)
    )),
    ADD CONSTRAINT prices_product_amount_window_key
        UNIQUE NULLS NOT DISTINCT (merchant_id, product_id, key, amount, currency, access_duration_hours, billing_interval_hours, trial_unit_amount, trial_duration_hours, customer_amount);
COMMENT ON COLUMN billing.prices.access_duration_hours IS 'Access granted by each purchase in hours; NULL means no scheduled expiry, independently of billing cadence.';
COMMENT ON COLUMN billing.prices.billing_interval_hours IS 'Recurring billing cadence in hours; NULL means one-time. Recurring subscriptions bill until canceled.';

ALTER TABLE billing.subscriptions ADD COLUMN access_duration_hours_snapshot integer
    CHECK (access_duration_hours_snapshot IS NULL OR access_duration_hours_snapshot > 0);
UPDATE billing.subscriptions s SET access_duration_hours_snapshot = p.access_duration_hours
FROM billing.prices p WHERE p.merchant_id = s.merchant_id AND p.id = s.price_id;
COMMENT ON COLUMN billing.subscriptions.access_duration_hours_snapshot IS 'Access duration accepted for the current paid phase in hours; NULL means no scheduled expiry. Retained independently of repricing.';

-- Older provider subscriptions projected one open entitlement for all their
-- paid grants. Restore its finite bound from the complete live grant history,
-- not just its first grant or the current catalog, preserving trials and every
-- later paid extension. Reconciliation materializes missing per-grant windows.
WITH paid_bounds AS (
    SELECT e.merchant_id, e.id,
           CASE WHEN bool_or(g.ends_at IS NULL) THEN NULL ELSE max(g.ends_at) END AS ends_at
    FROM billing.entitlements e
    JOIN billing.subscriptions s ON s.merchant_id = e.merchant_id AND s.id = e.source_id
    JOIN billing.grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id
      AND g.source_type = 'subscription' AND g.source_id = s.id::text
      AND g.kind = 'entitlement' AND g.event = 'grant'
      AND g.spec_snapshot->'entitlements' ? e.entitlement
    WHERE e.source_type = 'subscription' AND e.ends_at IS NULL
      AND e.revoked_at IS NULL AND e.deleted_at IS NULL
      AND s.access_duration_hours_snapshot IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM billing.grants terminal
          WHERE terminal.merchant_id = g.merchant_id AND terminal.supersedes_id = g.id
            AND terminal.event IN ('revoke', 'expire', 'supersede'))
    GROUP BY e.merchant_id, e.id
)
UPDATE billing.entitlements e SET ends_at = bounds.ends_at
FROM paid_bounds bounds WHERE bounds.merchant_id = e.merchant_id AND bounds.id = e.id
  AND bounds.ends_at IS NOT NULL;
