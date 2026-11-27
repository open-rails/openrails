-- The reservations are a customer's own card removals and cancellations: the
-- table and its objects are named for customers. Stored kinds and action keys
-- keep their spelling.

ALTER TABLE billing.user_destructive_reservations RENAME TO customer_destructive_reservations;
ALTER TABLE billing.customer_destructive_reservations RENAME CONSTRAINT user_destructive_reservations_pkey TO customer_destructive_reservations_pkey;
ALTER TABLE billing.customer_destructive_reservations RENAME CONSTRAINT user_destructive_reservations_merchant_id_fkey TO customer_destructive_reservations_merchant_id_fkey;
ALTER TABLE billing.customer_destructive_reservations RENAME CONSTRAINT user_destructive_reservations_action_key_check TO customer_destructive_reservations_action_key_check;
ALTER TABLE billing.customer_destructive_reservations RENAME CONSTRAINT user_destructive_reservations_kind_check TO customer_destructive_reservations_kind_check;
ALTER INDEX billing.user_destructive_reservations_expiry_idx RENAME TO customer_destructive_reservations_expiry_idx;
ALTER INDEX billing.user_destructive_reservations_window_idx RENAME TO customer_destructive_reservations_window_idx;
COMMENT ON TABLE billing.customer_destructive_reservations IS 'Merchant-global customer card-delete and soft-cancel rolling-hour/day rate reservations. No actor can obtain a separate budget. customer_id is an opaque identity and deliberately has no cascading customer FK. The canonical operation/card/subscription remains authoritative after pruning. Retention: rows are deleted 24 hours after reserved_at.';
