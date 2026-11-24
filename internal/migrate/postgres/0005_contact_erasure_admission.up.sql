-- A directory's erasure of a customer's contact (SCIM DELETE) is an admitted
-- destructive command: its actor spends the destructive budget.
ALTER TABLE billing.destructive_actions DROP CONSTRAINT destructive_actions_kind_check;
ALTER TABLE billing.destructive_actions ADD CONSTRAINT destructive_actions_kind_check
    CHECK (kind IN ('refund', 'cancel_subscription', 'delete_payment_method', 'revoke_credit', 'revoke_access', 'waive_order', 'mark_uncollectible', 'catalog_change', 'configuration_change', 'erase_contact'));
