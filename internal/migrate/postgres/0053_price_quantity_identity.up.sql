-- Seat bounds are immutable price terms: changing only quantity creates a
-- new version while the archived version keeps its original terms.
ALTER TABLE billing.prices
    DROP CONSTRAINT prices_product_amount_window_key,
    ADD CONSTRAINT prices_product_amount_window_key UNIQUE NULLS NOT DISTINCT (merchant_id, product_id, key, amount, currency, access_duration_hours, billing_interval_hours, trial_unit_amount, trial_duration_hours, customer_amount, billing_interval_months, provider_access_default, quantity);
