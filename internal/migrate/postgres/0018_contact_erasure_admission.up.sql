-- A directory's erasure of a customer's contact (SCIM DELETE) is an admitted
-- destructive command with a budget and holds of its own: the merchant's
-- erasure scope, which no other command counts or holds.
ALTER TABLE billing.destructive_actions DROP CONSTRAINT destructive_actions_kind_check;
ALTER TABLE billing.destructive_actions ADD CONSTRAINT destructive_actions_kind_check
    CHECK (kind IN ('refund', 'cancel_subscription', 'delete_payment_method', 'block_payment_method', 'revoke_credit', 'revoke_access', 'waive_order', 'mark_uncollectible', 'catalog_change', 'configuration_change', 'erase_contact'));

ALTER TABLE billing.destructive_holds DROP CONSTRAINT destructive_holds_scope_check;
ALTER TABLE billing.destructive_holds ADD CONSTRAINT destructive_holds_scope_check
    CHECK (scope IN ('actor', 'merchant', 'deployment', 'erasure'));

-- The queue drains oldest first; the budget counts finished erasures.
CREATE INDEX destructive_actions_erasure_queue_idx ON billing.destructive_actions (merchant_id, admission_sequence)
    WHERE kind = 'erase_contact' AND state IN ('reserved', 'started');
CREATE INDEX destructive_actions_erasure_recent_idx ON billing.destructive_actions (merchant_id, GREATEST(admitted_at, finished_at)) INCLUDE (units)
    WHERE kind = 'erase_contact' AND state = 'succeeded';
