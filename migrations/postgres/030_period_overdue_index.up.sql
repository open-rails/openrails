-- #575: index the LIFE period-overdue lane so ListPeriodOverdueSubscriptions is a
-- due-time range seek instead of a full scan of every active subscription.
-- Partial on status='active' (the candidate state) + ordered by the due column:
-- the repair flips active -> past_due, so a processed row leaves the index, and
-- a `current_period_ends_at < now` scan is O(overdue), independent of population.
CREATE INDEX idx_subscriptions_period_overdue
    ON openrails.subscriptions (current_period_ends_at)
    WHERE status = 'active'::openrails.subscription_status;
