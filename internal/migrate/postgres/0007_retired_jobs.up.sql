-- A billing fact's customer message and its send job are written with the
-- fact, in its transaction (billingfact.Record): the trigger, its wake-up
-- notification and the pending status are gone. A message still pending
-- has no send job and nothing queues one now: it ends failed.

DROP TRIGGER notification_event_customer_message ON billing.notification_events;
DROP FUNCTION billing.queue_customer_message();
DROP INDEX billing.customer_messages_pending_idx;
UPDATE billing.customer_messages SET status = 'failed', last_error_code = 'unavailable', updated_at = now()
WHERE status = 'pending';
ALTER TABLE billing.customer_messages DROP CONSTRAINT customer_messages_status_check;
ALTER TABLE billing.customer_messages ADD CONSTRAINT customer_messages_status_check
    CHECK (status IN ('queued', 'sending', 'sent', 'disabled', 'test_environment', 'no_contact', 'rate_limited', 'failed'));
COMMENT ON COLUMN billing.customer_messages.status IS 'queued with its fact and send job until the job settles it, sending while the job hands it to the sender; then sent, or why not: disabled (the merchant does not send this event type), test_environment (its PSP moves no real money), no_contact (no address), rate_limited (the customer''s hourly cap) or failed.';
