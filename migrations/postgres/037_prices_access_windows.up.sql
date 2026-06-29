-- #622: prices declare explicit product-access windows. HARD CUT from the
-- billing_cycle_days + intro model to the access-window model:
--
--   access_duration_days = the access window a purchase of this price grants.
--                          NULL = indefinite/durable (perpetual ownership).
--                          A positive value = a finite window (rental, one-off,
--                          or the billing period when auto_renew).
--   auto_renew           = whether the price charges again and extends the
--                          window at the end of access_duration_days. A
--                          recurring subscription is auto_renew=true with a
--                          finite access_duration_days (the billing period).
--   trial_unit_amount    = optional first-phase price (micros; 0 = free trial).
--   trial_duration_days  = optional first-phase length in days.
--
-- A price's identity is its financial substance:
--   (product_id, amount, currency, access_duration_days, auto_renew).
ALTER TABLE openrails.prices
    DROP CONSTRAINT IF EXISTS unique_prices_product_amount_cycle;

ALTER TABLE openrails.prices
    DROP COLUMN IF EXISTS billing_cycle_days,
    DROP COLUMN IF EXISTS initial_amount,
    DROP COLUMN IF EXISTS initial_period_days;

ALTER TABLE openrails.prices
    ADD COLUMN access_duration_days integer,
    ADD COLUMN auto_renew boolean DEFAULT false NOT NULL,
    ADD COLUMN trial_unit_amount bigint,
    ADD COLUMN trial_duration_days integer;

-- A finite window must be positive; NULL = indefinite.
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_access_duration_positive_chk
        CHECK ((access_duration_days IS NULL) OR (access_duration_days > 0));

-- Nothing to renew without a finite window: auto_renew requires a duration.
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_auto_renew_needs_duration_chk
        CHECK ((NOT auto_renew) OR (access_duration_days IS NOT NULL));

-- Trial is both-or-neither, non-negative amount (0 = free), positive length,
-- and only on an auto-renewing price (a first phase then recurring terms).
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_trial_both_or_neither_chk
        CHECK ((trial_unit_amount IS NULL) = (trial_duration_days IS NULL));
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_trial_amount_nonneg_chk
        CHECK ((trial_unit_amount IS NULL) OR (trial_unit_amount >= 0));
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_trial_period_positive_chk
        CHECK ((trial_duration_days IS NULL) OR (trial_duration_days > 0));
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_trial_needs_auto_renew_chk
        CHECK ((trial_unit_amount IS NULL) OR auto_renew);

ALTER TABLE openrails.prices
    ADD CONSTRAINT unique_prices_product_amount_window
        UNIQUE (product_id, amount, currency, access_duration_days, auto_renew);

COMMENT ON COLUMN openrails.prices.access_duration_days IS '#622 access window in days a purchase grants; NULL = indefinite/durable.';
COMMENT ON COLUMN openrails.prices.auto_renew IS '#622 whether the price recharges and extends the window after access_duration_days (recurring).';
COMMENT ON COLUMN openrails.prices.trial_unit_amount IS '#622 optional first-phase price (micros); 0 = free trial; NULL = no trial.';
COMMENT ON COLUMN openrails.prices.trial_duration_days IS '#622 optional first-phase length in days; NULL = no trial.';
