-- parent: 50 sha256:ff3bf04ba9290d69970a5b7951d651762ee4e55b0f6e45bcb7ed862e22c1e5bd
-- An order has two status axes, as Stripe's Order and Shopify's do: its own
-- (open, processing, complete, canceled, expired) and its payment's, as a
-- PaymentIntent's (requires_payment_method, requires_action, processing,
-- succeeded). A paid order is complete; its event is order.completed.
ALTER TABLE billing.orders DROP CONSTRAINT orders_status_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_paid_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_paid_payment_check;
DROP INDEX billing.orders_expires_at_live_idx;
ALTER TABLE billing.orders RENAME COLUMN paid_at TO completed_at;
ALTER TABLE billing.orders ADD COLUMN payment_status text;
UPDATE billing.orders SET
    payment_status = CASE
        WHEN status = 'requires_action' THEN 'requires_action'
        WHEN status = 'processing' THEN 'processing'
        WHEN status = 'paid' OR payment_id IS NOT NULL THEN 'succeeded'
        ELSE 'requires_payment_method' END,
    status = CASE status WHEN 'requires_action' THEN 'open' WHEN 'paid' THEN 'complete' ELSE status END;
ALTER TABLE billing.orders ALTER COLUMN payment_status SET NOT NULL;
ALTER TABLE billing.orders
    ADD CONSTRAINT orders_status_check CHECK (status IN ('open', 'processing', 'complete', 'canceled', 'expired')),
    ADD CONSTRAINT orders_payment_status_check CHECK (CASE status
        WHEN 'open' THEN payment_status IN ('requires_payment_method', 'requires_action')
        WHEN 'processing' THEN payment_status = 'processing'
        WHEN 'complete' THEN payment_status = 'succeeded'
        ELSE payment_status IN ('requires_payment_method', 'succeeded') END),
    ADD CONSTRAINT orders_succeeded_check CHECK ((payment_status = 'succeeded') = (status = 'complete' OR payment_id IS NOT NULL)),
    ADD CONSTRAINT orders_complete_check CHECK ((status = 'complete') = (number IS NOT NULL AND completed_at IS NOT NULL)),
    ADD CONSTRAINT orders_complete_payment_check CHECK (status <> 'complete' OR total = 0 OR payment_id IS NOT NULL);
CREATE INDEX orders_expires_at_live_idx ON billing.orders USING btree (merchant_id, expires_at) WHERE status = 'open';
COMMENT ON COLUMN billing.orders.status IS 'open (takes payment, or awaits the customer''s action on it), processing (the provider has its payment), complete (paid and fulfilled), canceled or expired.';
COMMENT ON COLUMN billing.orders.payment_status IS 'The payment''s own status: requires_payment_method (none yet, or the last declined: last_payment_error), requires_action, processing or succeeded. A closed order paid late is succeeded with its payment refunded.';
COMMENT ON COLUMN billing.orders.payment_method_id IS 'The card the latest attempt charged; a card the attempt saved from the customer''s token stays as evidence after it is removed.';
COMMENT ON TABLE billing.orders IS 'One purchase (ord_ id): frozen lines and total in one currency, paid by at most one live checkout attempt at a time. A decline leaves it open with last_payment_error; it is numbered from document_sequences when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 90 days after they closed; every other order is permanent.';

ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
UPDATE billing.host_outbox SET
    event_type = CASE event_type WHEN 'order.paid' THEN 'order.completed' ELSE event_type END,
    dedupe_key = CASE WHEN event_type = 'order.paid' THEN 'order.completed' || substr(dedupe_key, length('order.paid') + 1) ELSE dedupe_key END,
    data = data || jsonb_build_object(
        'status', CASE data->>'status' WHEN 'paid' THEN 'complete' WHEN 'requires_action' THEN 'open' ELSE data->>'status' END,
        'payment_status', CASE data->>'status' WHEN 'paid' THEN 'succeeded' WHEN 'requires_action' THEN 'requires_action'
            WHEN 'processing' THEN 'processing' ELSE 'requires_payment_method' END)
WHERE subject_type = 'order';
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
);
