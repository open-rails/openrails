ALTER TABLE billing.subscriptions DROP CONSTRAINT IF EXISTS chk_cancelled_no_retry_schedule;
ALTER TABLE billing.subscriptions DROP CONSTRAINT IF EXISTS chk_past_due_has_period_end;

DROP INDEX IF EXISTS billing.idx_subscriptions_due_dunning;
DROP INDEX IF EXISTS billing.idx_credit_holds_active_expires;
DROP INDEX IF EXISTS billing.uniq_subscriptions_processor_subscription_id_nonempty;
DROP INDEX IF EXISTS billing.idx_subscriptions_user_product_lifecycle_owner;
