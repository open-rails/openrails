CREATE TABLE IF NOT EXISTS billing.manual_rebill_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subscription_id UUID NOT NULL REFERENCES billing.subscriptions(id),
    period_end TIMESTAMPTZ NOT NULL,
    processor TEXT NOT NULL,
    order_reference TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'unknown')),
    transaction_id TEXT,
    failure_reason TEXT,
    claimed_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    CONSTRAINT uniq_manual_rebill_subscription_period UNIQUE (subscription_id, period_end),
    CONSTRAINT uniq_manual_rebill_processor_order UNIQUE (processor, order_reference)
);

CREATE UNIQUE INDEX IF NOT EXISTS uniq_manual_rebill_processor_transaction
    ON billing.manual_rebill_attempts(processor, transaction_id)
    WHERE transaction_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_manual_rebill_attempts_status_claimed
    ON billing.manual_rebill_attempts(status, claimed_until);
