ALTER TABLE billing.payments
    DROP COLUMN IF EXISTS credits_spec_snapshot,
    DROP COLUMN IF EXISTS entitlements_spec_snapshot;

ALTER TABLE billing.subscriptions
    DROP COLUMN IF EXISTS credits_spec_snapshot,
    DROP COLUMN IF EXISTS entitlements_spec_snapshot;
