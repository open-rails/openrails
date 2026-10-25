-- Repair: preserve Existing fixed-duration prices and accepted periods are unchanged.
ALTER TABLE billing.prices DROP CONSTRAINT prices_calendar_cadence_check;
ALTER TABLE billing.prices ADD CONSTRAINT prices_calendar_cadence_check
    CHECK (billing_interval_months IS NULL OR (billing_interval_hours IS NULL AND customer_amount IS NULL AND billing_interval_months BETWEEN 1 AND 1200));
COMMENT ON COLUMN billing.prices.billing_interval_months IS 'Calendar-month cadence, independent of fixed hours. Own subscriptions retain their original UTC calendar anchor in accepted agreements; provider-owned subscriptions retain provider-observed periods.';

ALTER TABLE billing.order_lines ADD COLUMN billing_interval_months integer;
ALTER TABLE billing.order_lines ADD CONSTRAINT order_lines_calendar_cadence_check
    CHECK (billing_interval_months IS NULL OR (billing_interval_hours IS NULL AND billing_interval_months BETWEEN 1 AND 1200));
COMMENT ON COLUMN billing.order_lines.billing_interval_months IS 'Accepted calendar-month cadence. NULL preserves historical fixed-hour and one-time lines. The initial completed order fixes the UTC calendar anchor.';
ALTER TABLE billing.order_lines DROP CONSTRAINT order_lines_quantified_check;
ALTER TABLE billing.order_lines ADD CONSTRAINT order_lines_quantified_check
    CHECK (quantity IS NOT NULL OR billing_interval_hours IS NOT NULL OR billing_interval_months IS NOT NULL OR source IS NOT NULL);
