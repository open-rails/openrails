-- Prices express their access window in HOURS, not days, so a purchase can grant
-- sub-day access (e.g. a 12h rental). The #622 day columns are renamed to hours
-- and existing values converted (x24). PostgreSQL rewrites the check constraints
-- (positive / auto_renew-needs-duration / trial-period-positive) onto the new
-- column names automatically on RENAME, so they keep holding.
--
-- The recurring billing cadence that providers consume (Stripe interval, NMI
-- day-frequency, Solana period) is still derived in whole days from this window
-- (hours/24) by Price.RecurringCycleDays — sub-day windows are meant for one-off
-- prices; a sub-day auto_renew cadence floors to 0 days and is simply not pushed
-- to a provider.
--
-- Trial terms also JOIN the price identity: a price with a trial first phase is a
-- different offer than the same recurring price without one. The unique key
-- becomes the full financial substance, with NULLS NOT DISTINCT so "no trial"
-- and "no window" count as concrete values — otherwise NULL would make every
-- no-trial / durable price unique and silently disable de-dup.
ALTER TABLE openrails.prices
    DROP CONSTRAINT IF EXISTS unique_prices_product_amount_window;

ALTER TABLE openrails.prices RENAME COLUMN access_duration_days TO access_duration_hours;
ALTER TABLE openrails.prices RENAME COLUMN trial_duration_days TO trial_duration_hours;

UPDATE openrails.prices SET access_duration_hours = access_duration_hours * 24 WHERE access_duration_hours IS NOT NULL;
UPDATE openrails.prices SET trial_duration_hours = trial_duration_hours * 24 WHERE trial_duration_hours IS NOT NULL;

ALTER TABLE openrails.prices
    ADD CONSTRAINT unique_prices_product_amount_window
        UNIQUE NULLS NOT DISTINCT
        (product_id, amount, currency, access_duration_hours, auto_renew,
         trial_unit_amount, trial_duration_hours);

COMMENT ON COLUMN openrails.prices.access_duration_hours IS 'access window in HOURS a purchase grants; NULL = indefinite/durable. For auto_renew, hours/24 is the provider billing cadence in days.';
COMMENT ON COLUMN openrails.prices.auto_renew IS '#622 whether the price recharges and extends the window after access_duration_hours (recurring).';
COMMENT ON COLUMN openrails.prices.trial_duration_hours IS 'optional trial first-phase length in HOURS; NULL = no trial.';
