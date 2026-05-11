CREATE INDEX IF NOT EXISTS idx_entitlements_grace_by_subscription_live
    ON billing.entitlements(source_id, entitlement, start_at, end_at)
    WHERE source_type = 'grace' AND revoked_at IS NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_entitlements_subscription_source_live
    ON billing.entitlements(source_id, entitlement, end_at)
    WHERE source_type = 'subscription' AND revoked_at IS NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_entitlements_one_off_source_live
    ON billing.entitlements(source_id, entitlement)
    WHERE source_type = 'one_off' AND revoked_at IS NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_entitlements_live_by_id
    ON billing.entitlements(id)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_credit_withdrawal_idem
    ON billing.credit_transactions(user_id, credit_type_id, source, source_id)
    WHERE transaction_type = 'withdrawal' AND source_id IS NOT NULL;
