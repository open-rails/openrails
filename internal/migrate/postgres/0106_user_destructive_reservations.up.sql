-- Repair: none-needed New short-lived reservations for customer-requested destructive actions.
CREATE TABLE billing.user_destructive_reservations (
    merchant_id uuid NOT NULL REFERENCES billing.merchants(id) ON DELETE CASCADE,
    customer_id uuid NOT NULL,
    action_key text NOT NULL CHECK (action_key <> '' AND octet_length(action_key) <= 512),
    kind text NOT NULL CHECK (kind IN ('delete_payment_method', 'cancel_subscription')),
    reserved_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, action_key)
);
CREATE INDEX user_destructive_reservations_expiry_idx ON billing.user_destructive_reservations (reserved_at, merchant_id, action_key);
CREATE INDEX user_destructive_reservations_window_idx ON billing.user_destructive_reservations (merchant_id, reserved_at) INCLUDE (customer_id);
COMMENT ON TABLE billing.user_destructive_reservations IS 'Merchant-global customer card-delete and soft-cancel rolling-hour/day rate reservations. No actor can obtain a separate budget. customer_id is an opaque identity and deliberately has no cascading customer FK. Operational only: retain 24 hours, with bounded cleanup; retries reuse their key while retained. The canonical operation/card/subscription remains authoritative after pruning.';
