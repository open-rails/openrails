-- Repair: none-needed Widened checks refuse no stored row.
-- A customer revoking a merchant's payment authority is a host event.
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_currency_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_currency_check CHECK ((currency IS NULL) = (event_type IN ('product.entitlements_changed', 'customer.network_linked', 'customer.network_unlinked', 'customer.payment_authority_revoked')) AND (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$'));
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired', 'order.voided')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
    OR (event_type IN ('customer.network_linked', 'customer.network_unlinked', 'customer.payment_authority_revoked')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
);
COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions, product key changes, order transitions, customer links and payment authority revocations. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';
