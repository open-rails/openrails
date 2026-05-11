DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM billing.subscriptions
        WHERE status IN ('active', 'pending', 'past_due')
        GROUP BY user_id, product_id
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'duplicate lifecycle-owner subscriptions exist; repair before creating idx_subscriptions_user_product_lifecycle_owner';
    END IF;
END$$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_subscriptions_user_product_lifecycle_owner
    ON billing.subscriptions(user_id, product_id)
    WHERE status IN ('active', 'pending', 'past_due');

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM billing.subscriptions
        WHERE processor_subscription_id <> ''
        GROUP BY processor, processor_subscription_id
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'duplicate processor subscription IDs exist; repair before creating uniq_subscriptions_processor_subscription_id_nonempty';
    END IF;
END$$;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_subscriptions_processor_subscription_id_nonempty
    ON billing.subscriptions(processor, processor_subscription_id)
    WHERE processor_subscription_id <> '';

CREATE INDEX IF NOT EXISTS idx_subscriptions_due_dunning
    ON billing.subscriptions(next_retry_at, processor)
    WHERE status = 'past_due' AND next_retry_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_credit_holds_active_expires
    ON billing.credit_transactions(expires_at)
    WHERE transaction_type = 'hold' AND status = 'active';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chk_past_due_has_period_end'
          AND connamespace = 'billing'::regnamespace
    ) THEN
        ALTER TABLE billing.subscriptions
            ADD CONSTRAINT chk_past_due_has_period_end
            CHECK (status <> 'past_due' OR current_period_ends_at IS NOT NULL) NOT VALID;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chk_cancelled_no_retry_schedule'
          AND connamespace = 'billing'::regnamespace
    ) THEN
        ALTER TABLE billing.subscriptions
            ADD CONSTRAINT chk_cancelled_no_retry_schedule
            CHECK (status <> 'cancelled' OR (next_retry_at IS NULL AND grace_ends_at IS NULL)) NOT VALID;
    END IF;
END$$;
