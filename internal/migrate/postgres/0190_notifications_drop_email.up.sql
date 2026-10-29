-- Repair: none-needed. notifications holds only merchant alerts, which are
-- never emailed from it: nothing reads or writes the customer email marker,
-- its lease or the index of unsent customer rows.
DROP INDEX billing.notifications_created_at_id_idx;
ALTER TABLE billing.notifications
    DROP CONSTRAINT notifications_recipient_check,
    DROP COLUMN emailed_at,
    DROP COLUMN email_lease_expires_at,
    ADD CONSTRAINT notifications_recipient_check CHECK (
        (recipient_kind = 'customer' AND customer_id IS NOT NULL AND severity IS NULL AND title IS NULL AND body IS NULL AND link IS NULL)
        OR (recipient_kind = 'merchant' AND customer_id IS NULL AND event_type = 'operator.alert'
            AND severity IS NOT NULL AND title IS NOT NULL AND body IS NOT NULL));
