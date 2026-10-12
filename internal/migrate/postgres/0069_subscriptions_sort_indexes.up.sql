-- Repair: none-needed New indexes.
-- The admin subscription list's orders by period end and by cancellation.
CREATE INDEX subscriptions_current_period_ends_at_id_idx ON billing.subscriptions USING btree (merchant_id, current_period_ends_at, id)
    WHERE deleted_at IS NULL AND current_period_ends_at IS NOT NULL;
CREATE INDEX subscriptions_canceled_at_id_idx ON billing.subscriptions USING btree (merchant_id, canceled_at DESC, id DESC)
    WHERE deleted_at IS NULL AND canceled_at IS NOT NULL;
