-- #602: introductory & trial pricing — a recurring price whose FIRST period
-- differs from the recurring terms. CCBill (and Stripe) express this natively;
-- e.g. doujins legacy CCBill RBO 0000000931: $19.95 for the first 30 days, then
-- $14.95 every 30 days. A free trial is the same shape with initial_amount = 0
-- (e.g. $0 for 7 days, then $15/30d).
--
--   initial_amount      = the first-period price (minor units; 0 = free trial).
--   initial_period_days = the first-period length in days.
-- Both NULL = a flat price (today's behavior, unchanged). The recurring terms
-- stay in amount + billing_cycle_days; the price identity is unchanged.
ALTER TABLE openrails.prices
    ADD COLUMN IF NOT EXISTS initial_amount bigint,
    ADD COLUMN IF NOT EXISTS initial_period_days integer;

-- Both-or-neither; a non-negative initial amount (0 allowed = free trial); a
-- positive initial period when present.
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_intro_both_or_neither_chk
        CHECK ((initial_amount IS NULL) = (initial_period_days IS NULL));
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_intro_amount_nonneg_chk
        CHECK ((initial_amount IS NULL) OR (initial_amount >= 0));
ALTER TABLE openrails.prices
    ADD CONSTRAINT prices_intro_period_positive_chk
        CHECK ((initial_period_days IS NULL) OR (initial_period_days > 0));

COMMENT ON COLUMN openrails.prices.initial_amount IS '#602 intro/trial: first-period price (minor units); 0 = free trial; NULL = flat price.';
COMMENT ON COLUMN openrails.prices.initial_period_days IS '#602 intro/trial: first-period length in days; NULL = flat price.';
